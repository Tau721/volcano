# UnassignedNumaPods 写入与删除现状分析（修复前）

> 状态：本文是**修复前**的现状分析，用于定位根因。修复已落地，修复后的写入/删除路径见
> [修复方案](./Pod信息残留导致NUMA感知调度失败问题修复方案.md)。

本文只分析 `NodeInfo.UnassignedNumaPods`（`pkg/scheduler/api/node_info.go:75`）**修复前**
的写入与删除两侧：各自在哪里发生、检查了什么、遗漏了什么，以及两侧为什么不对称。
不变量定义与修复方案见
[UnassignedNumaPods 字段维护分析](./numa-unassigned-pods-maintenance-analysis.md)。

## 0. 结论摘要

- **写入**：只有一个入口（`numa-aware` 插件 `OnSessionClose`），一次性汇总本 session 中
  `AllocateFunc` 记录的任务。它**不区分** allocate / pipeline、**不检查**任务是否真的
  进入 bind 流程、**不检查** pod 是否仍存在。
- **删除**：有两个事件入口（`DeletePod`、`syncTask NotFound`）和一个整体清空
  （`RefreshNumaSchedulerInfoByCrd`）。事件入口**只按 `TaskInfo.NodeName` 定位条目**，
  而该字段在 pipeline、resync 重建、task 缺失时为空。
- **不对称**：写入依据"插件内存里记录过"，删除依据"cache 中当前的状态"。
  两侧的集合定义不一致，导致"写入的条目不一定能删除"。

## 1. 写入现状

**适用范围**：本字段只记录 Guaranteed pod。

- `numa-aware` 的 predicate 对非 Guaranteed pod 直接返回 nil（`numaaware.go:123-126`），
  不会产生 `assignRes` 记录；
- `AllocateFunc` 在 `pp.assignRes[task.UID]` 不存在时直接返回（`numaaware.go:89-92`），
  因此不会被记入 `taskBindNodeMap`。

下文讨论的"任务"均指 Guaranteed pod。由此也可以排除 `backfill`：
backfill 只拾取无资源请求的 task（`backfill.go:164-169`，BestEffort，DRA-only 也排除），
与 numaaware 的处理范围没有交集。

### 1.1 唯一写入链

```text
Statement.Allocate / Pipeline      ← 动作层调用
   └─ AllocateFunc（插件注册）      numaaware.go:87-118
        taskBindNodeMap[taskID] = node
        taskPodMetaMap[taskID] = PodMeta
        assignRes[taskID][node] = ResNumaSets

Session 结束
   └─ numaaware.OnSessionClose      numaaware.go:263-292
        └─ ssn.AddUnassignedNumaPods session.go:965-967
             └─ cache.AddUnassignedNumaPods  cache.go:1176-1196
                  node.UnassignedNumaPods[podMeta] = resSets
```

要点：

- `AllocateFunc` 被四条路径调用：`Session.Allocate`（`session.go:805-812`）、
  `Statement.Allocate`（`statement.go:298-311`）、`Session.Pipeline`（`session.go:763-769`）、
  `Statement.Pipeline`（`statement.go:191-203`）。其中 `Session.Allocate` 在生产代码中
  只有 `backfill` 使用（`actions/backfill/backfill.go:121`），而 backfill 只处理
  BestEffort task，不会进入本字段的写入集合（见本节开头的适用范围）。
- `OnSessionClose` 遍历 `taskBindNodeMap`，从 `assignRes` 取资源集合，从 `taskPodMetaMap`
  取 key；缺少任一项时打 warning 并跳过（`numaaware.go:273-287`）。
- `cache.AddUnassignedNumaPods` 按 `(PodMeta, node)` 写入，节点不存在时 warning 并跳过
  （`cache.go:1184-1186`），map 懒初始化（`1188-1190`）。
- 写入发生在 session 关闭时；bind 是异步且与 session 并发的（`AddBindTask` 在
  `Statement.Commit` 中入队，`cache.go:1491-1531`），因此**写入时 bind 结果未知**。

### 1.2 实际写入集合的构成

| 来源 | 是否真的会 bind | 当前是否写入 | 说明 |
| --- | --- | --- | --- |
| 正常 allocate + bind 成功 | 是 | 是 | 预期行为 |
| 正常 allocate + bind 失败 | 否 | **是** | session 状态仍是 Binding，写入侧无感知 |
| pipeline（超卖/抢占/reclaim） | 否（本会话不 dispatch） | **是** | `Allocated`/`Pipelined` 状态未被区分 |
| statement rollback 的 task | 否 | 否 | `DeallocateFunc` 已清掉插件记录 |
| session 中途被删除的 pod | 否 | **是** | 写入不检查 pod 是否仍存在 |

上表中 pipeline、bind 失败、session 中途删除都是**确定性**的越界写入：
写入只遍历插件的 `taskBindNodeMap`，既不检查 bind 结果，也不检查 pod 是否仍存在，
而 bind 失败不会触发 `DeallocateFunc`，插件记录会一直保留到 `OnSessionClose`。

"bind 失败发生在 session 关闭前还是后"只影响**清理**，不影响是否写入：

- 失败在 `OnSessionClose` 之后：条目已写入，失败处理若带清理可以删除它；
- 失败在 `OnSessionClose` 之前：失败处理先执行，随后写入会把条目重新加回来
  （"先回滚、后写入"）；
- 当前实现没有 bind 失败的清理逻辑，两种情况都会残留，直到 resync / pod 删除 /
  下一次成功分配。

### 1.3 写入侧问题清单

- **W1 不区分操作类型**：`AllocateFunc` 同时服务 allocate 和 pipeline，
  `taskBindNodeMap` 不记录类型。Pipeline 任务本会话不会 bind，后续真正调度时会重新
  allocate 并重新写入，提前写入没有收益（见 1.2）。
- **W2 不检查 pod 存活**：session 期间 pod 被删除后，插件内存记录仍在，
  `OnSessionClose` 照常写入（场景 C）。
- **W3 不感知 bind 结果**：bind 失败只更新 cache 和 pod status，session 副本不变；
  写入侧无法区分"已提交待 bind"和"已提交且 bind 已失败"。
- **W4 不跨节点去重**：写入以 `(PodMeta, node)` 为粒度，`AddUnassignedNumaPods` 不会
  清理同一 pod 在其他节点上的旧条目（`cache.go:1181-1193`）。
- **W5 写入时机固定**：只在 session 关闭时发生，无法表达"任务已 dispatch 但 bind 结果
  未知"这一状态，也无法在 bind 失败后撤销。

### 1.4 写入侧的良性设计

- **内存态、按 session 隔离**：插件实例每个 session 重建（`framework.go:75-81`），
  `taskBindNodeMap` / `assignRes` 不会跨 session 累积。
- **同 key 幂等覆盖**：重复写入同一 `(PodMeta, node)` 只更新不新增（`cache.go:1191`）。
- **并发安全**：`AddUnassignedNumaPods` 持有 `sc.Mutex`（`cache.go:1178`），
  与 `DeletePod` 等事件互斥；写入与删除之间只有逻辑顺序问题，没有数据竞争。

## 2. 删除现状

### 2.1 删除入口

| 入口 | 触发条件 | 调用链 |
| --- | --- | --- |
| 事件删除 | pod 删除事件 | `DeletePod`（`event_handlers.go:605-637`）→ `clearUnassignedNumaPod`（`443-454`）→ `clearUnassignedNumaTask`（`400-411`） |
| resync 删除 | `syncTask` 发现 pod `NotFound` | `event_handlers.go:312-339` → `clearUnassignedNumaTask`（`400-411`） |
| 整体清空 | 所有条目都被 Numatopology CRD 认定已分配 | `RefreshNumaSchedulerInfoByCrd`（`node_info.go:205-208`） |

### 2.2 事件删除的前提

`clearUnassignedNumaTask`（`event_handlers.go:400-411`）只在满足以下条件时才删：

1. `len(ti.NodeName) != 0`；
2. `sc.Nodes[ti.NodeName]` 存在；
3. map 中存在由 `ti.Pod.UID/Name/Namespace` 组成的 `PodMeta`。

其中第 1 条是当前的硬约束：**删除无法按 UID 全局查找，只能按节点定位**。

### 2.3 删除所依赖的 NodeName 从哪来

| 来源 | 何时生效 | 备注 |
| --- | --- | --- |
| `NodeInfo.AddTask` 回写（`node_info.go:498-500`） | `AddBindTask` 调用 `node.AddTask` 时，**bind 之前** | 被 allocate 的 task 会有值 |
| informer 更新（`NewTaskInfo`） | pod `spec.nodeName` 变化后 | `updatePod` 对 allocated 且 nodeName 为空的更新有守卫（`event_handlers.go:363-368`） |

会让它变空/缺失的情况：

- **pipeline 任务**：不调用 `AddBindTask`，cache task 一直 `Pending`，`NodeName` 为空；
- **bind 失败后的 resync**：pod 仍存在时 `syncTask` 用 `NewTaskInfo(pod)` 重建 task，
  `NodeName` 变空（`event_handlers.go:331-338`）；
- **task 已不在 `sc.Jobs`**：回退 `NewTaskInfo(pod)`，未绑定 pod 的 `spec.nodeName` 为空。

### 2.4 删除侧问题清单

- **D1 依赖易变字段**：删除只能按 `NodeName` 定位，`NodeName` 为空时条目无法删除，
  且没有其他兜底入口。
- **D2 resync 后失效**：bind 失败 → resync 重建 task（`NodeName` 为空）→ 之后删除
  找不到节点（4.2）。
- **D3 删除早于写入**：pod 在 session 中途被删除时，`DeletePod` 先执行（map 中还没有条目），
  `OnSessionClose` 随后写入，之后再无事件清理（场景 C）。
- **D4 回退路径可能拿不到节点**：`clearUnassignedNumaPod` 在 task 缺失时回退
  `NewTaskInfo(pod)`（`event_handlers.go:443-454`），未绑定 pod 的 `spec.nodeName` 为空。
- **D5 无跨节点删除**：即使拿到正确的 UID，也只删 `ti.NodeName` 对应节点；
  换节点重调度留下的旧条目不会被清理。
- **D6 整体清空是 all-or-nothing**：`unassignedNumber == 0` 才置 nil
  （`node_info.go:205-208`）；一条永远不被 CRD 确认的条目会让整张 map 永远无法清空，
  并且 else 分支会对所有条目预占（`node_info.go:210-213`）。
- **D7 无周期 GC**：除整体清空外，没有任何"条目对应 pod 是否仍存在"的校验。
- **D8 update 路径不清理**：`updatePod` 走 `deletePod + addPod`，不调用
  `clearUnassignedNumaPod`（`event_handlers.go:363-398`）；对存活 pod 正确，但路径不对称。
- **D9 节点删除顺带清理**：节点本身被删除/重建时条目随 `NodeInfo` 消失——这是少数
  无需事件即可清理的路径。

### 2.5 删除能力矩阵

| 条目来源 | `DeletePod` 能删 | `syncTask NotFound` 能删 | 整体清空能清 | 实际结果 |
| --- | --- | --- | --- | --- |
| 正常 allocate，bind 成功后删除 | 是（`NodeName` 有值） | 是 | 是（CRD 确认后） | 正常 |
| bind 失败后被删除（resync 前） | 是（cache task 仍带 `NodeName`） | 是 | 否（CRD 无记录） | 竞态窗口内可清 |
| bind 失败后被删除（resync 后） | **否**（`NodeName` 已被清空） | **否** | **否** | 残留 |
| pipeline 后被删除 | **否** | **否** | **否** | 残留（确定性） |
| session 中途删除（写入发生在删除后） | 删除时无条目 | 无条目 | **否** | 残留 |
| 换节点重调度后的旧节点条目 | 只删新节点 | 只删新节点 | 仅旧节点无其他未分配条目时 | 可能残留 |

### 2.6 删除侧的良性设计

- **删除事件类型全覆盖**：`DeletePod` 处理了 `*v1.Pod` 和 `DeletedFinalStateUnknown`
  （`event_handlers.go:611-617`），不会因 informer 缓存状态漏事件。
- **删除与写入互斥**：两者都持 `sc.Mutex`，无数据竞争。
- **清空策略保守**：不会因部分 CRD 确认而丢弃未确认条目（INV-8），避免了
  "信息丢失导致重复分配"的更严重问题。

## 3. 写入与删除的不对称

| 维度 | 写入 | 删除 |
| --- | --- | --- |
| 依据 | 插件内存"记录过" | cache 中"当前状态" |
| 触发 | session 关闭，一次性汇总 | pod 删除事件 / resync / Snapshot |
| 是否看 bind 结果 | 否 | 否（只看 `NodeName`） |
| 是否检查 pod 存活 | 否 | 是（事件驱动），但可能被写入"抢跑" |
| 定位方式 | key 含 `PodMeta` + 所属节点 | 只能按 `NodeName` 定位，无 UID 索引 |
| 越界写入 | pipeline、已删除、bind 失败 | — |
| 无法删除 | — | pipeline、resync 后、删除早于写入、跨节点旧条目 |

一句话概括：**写入是"决策驱动"的，删除是"状态驱动"的；决策与缓存状态在 bind 这个
异步边界上没有对齐，因此存在写入集合 ⊄ 可删除集合。**

## 4. 现状结论

1. 写入入口唯一但过于宽松：把"记录过的决策"等同于"应预占的分配"，
   缺少对操作类型、bind 结果、pod 存活的校验。
2. 删除入口完全依赖 `NodeName`，而 `NodeName` 在 pipeline、resync、task 缺失时不可用，
   且没有按 UID 的回退路径。
3. 整体清空只能在 CRD 全量确认时发生，任何一条幽灵条目都会让整张 map 自锁。
4. 因此当前实现无法保证"写入的条目最终都会被删除"，残留是结构性问题，不是偶发。

最小修复方向（已按此实施，详见修复方案）：

- 写入侧：在 `AddBindTask` 提交 bind 时写入，pipeline/rollback 不写；
- 删除侧：按 `PodMeta.UID` 遍历节点删除；bind 失败按 pod 当前 `spec.nodeName` 对账；
- 兜底：Snapshot 时按 cache task 状态与节点一致性对账（`keepNumaReservation`，
  不能用 pod phase）。
