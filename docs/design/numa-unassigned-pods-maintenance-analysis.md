# UnassignedNumaPods 字段维护分析

> 状态：第 3~7 节描述修复前的实现与违例分析；第 8、10、11 节的方案已实施，
> 最终代码见 [修复方案](./Pod信息残留导致NUMA感知调度失败问题修复方案.md)。

本文按"字段语义 → 不变量 → 当前实现 → 违例分析 → 残留场景 → 修复建议"的顺序，
分析 `NodeInfo.UnassignedNumaPods`（`pkg/scheduler/api/node_info.go:75`）的维护问题。

## 1. 字段语义与格式

### 1.1 为什么需要这个字段

`numa-aware` 插件为 Guaranteed pod 选择 NUMA 亲和位置后，pod 的 CPU 集合先在调度器内存中确定；
真正 bind、kubelet 写入 `cpumanager_state`、numatopology 资源导出器把分配结果写回
`Numatopology` CRD 之间存在时间差。在这个窗口里，如果调度器只依赖 CRD，
就会把已决定但尚未落地的 CPU 再分给别的 pod，kubelet 侧报 `TopologyAffinityError`。

`UnassignedNumaPods` 就是这段时间差内的**预占表**：

> 记录"调度器已经把 pod 分配到本节点并决定其 NUMA 资源，但 Numatopology CRD
> 还没有确认这次分配"的信息。

一个 pod 的 NUMA 分配对调度器而言有三种状态：

| 状态 | 数据来源 | 是否需要 entry |
| --- | --- | --- |
| 未决定 | 无 | 否 |
| 已决定、CRD 未确认 | 插件内存 → `UnassignedNumaPods` | **是**（本字段的作用） |
| CRD 已确认 | `Numatopology.PodAllocations` + `NumaResMap` | 否（应由 Refresh 清除） |

这里的"已决定"必须严格限定为**已 dispatch、进入 bind 流程**的决策（`AddBindTask` 成功，
任务状态为 `Allocated`/`Binding`）。Pipeline 只是"提名/等待资源"（`actions/allocate/allocate.go:1264-1273`、
抢占 preemptor、reclaim），本会话不会 bind，后续真正被调度时会重新走 allocate 并重新写入，
因此 pipeline 阶段不应写入 entry；被 rollback 的决策同样不应写入。

### 1.2 格式

```go
// pkg/scheduler/api/node_info.go:75
UnassignedNumaPods map[PodMeta]ResNumaSets
```

- **key**：`PodMeta{UID, Name, Namespace}`，从 pod 对象派生，UID 是主身份。
- **value**：`ResNumaSets = map[resourceName]cpuset.CPUSet`，即该 pod 在本次决策中被分配的资源集合
  （目前只有 CPU）。
- **归属**：挂在某个 `NodeInfo` 上，因此**节点信息是关键的一部分**：同一条记录隐含
  "这个 pod 被分配到了这个节点"。字段本身没有独立的反向索引（UID → node）。

### 1.3 存储与作用域

- 纯内存态：只存在于 scheduler cache 的 `NodeInfo` 中，不持久化，不跨进程。
- 随节点存活：节点被删除/重建时条目一起消失；`NodeInfo.Clone()` 会深拷贝
  （`api/node_info.go:232-237`），session 快照能看到一致的值。
- 插件侧记录（`taskBindNodeMap` / `assignRes`）按 session 创建
  （`framework.go:75-81`），不跨 session 累积。
- 所有读写都在 `sc.Mutex` 保护下。

### 1.4 读写位置

| 动作 | 位置 |
| --- | --- |
| 写入 | `plugins/numaaware/numaaware.go:263-292` → `session.go:965-967` → `cache/cache.go:1176-1196` |
| 删除 | `cache/event_handlers.go:400-411`（`clearUnassignedNumaTask`），调用点：`DeletePod`（`605-637`）、`syncTask NotFound`（`312-339`） |
| 整体清空 | `api/node_info.go:205-208` |
| 读取/消费 | `api/node_info.go:187-216`（`RefreshNumaSchedulerInfoByCrd`，每次 `Snapshot` 时对 live cache 调用） |
| 判定"已分配" | `api/numa_info.go:213-223`（`CheckNumaPodAssigned`，匹配 CRD 的 `PodAllocations`；UID 优先，无 UID 的记录回退 namespace/name） |

## 2. 字段的不变量

以下是不变量（invariant）：任何时刻、任何代码路径都必须保证的性质。
按"正确性 / 维护性 / 并发与快照"三组列出，并给出当前实现是否满足。

### 2.1 正确性不变量

**INV-1（预占生效）**：map 中每条 entry 的 cpuset，必须在节点参与 predicate 的
`NumaSchedulerInfo` 中已经被扣除。
- 理由：否则同一批 CPU 会被重复分配，kubelet 报 `TopologyAffinityError`。
- 实现：`RefreshNumaSchedulerInfoByCrd` 的 else 分支对条目执行 `tmp.Allocate(resSets)`
  （`node_info.go:210-213`）。
- 当前状态：**满足**。

**INV-2（预占完备且不越界）**：entries 与"已 dispatch 进入 bind 流程、但 CRD 尚未确认"
的 pod 集合**一一对应**。
- 理由：漏写等于允许重复分配；多写（pipeline、已回滚、已删除的 pod）则会造成错误预占
  和无效条目。
- 实现：session 关闭时由插件汇总写入。
- 当前状态：**进程内满足前半（不漏）**；后半（不越界）**不满足**，pipeline 任务会被写入
  （见 4.4）；scheduler 重启后字段整体丢失，此时对"已 bind 但 CRD 未更新"的 pod
  无预占信息（当前设计的已知边界，本文不展开）。

**INV-3（节点正确且唯一）**：同一个 pod 最多只有一条 entry，且必须位于它将被/已被
bind 的那个节点。
- 理由：多条或错节点的 entry 会在错误的节点上永久占用 CPU。
- 实现：key 含 UID，写入按 node 拆分；但没有跨节点去重/迁移逻辑。
- 当前状态：**部分满足**，违反见 4.6、场景 E。

### 2.2 维护性不变量

**INV-4（无幽灵）**：每条 entry 对应的 pod 必须仍然存在于 scheduler cache 的
`Job.Tasks` 中（或正处于会被清理的终态）。
- 理由：pod 不存在了，预占就永远没有意义，只会泄漏资源。
- 当前状态：**不满足**，违反见第 4、5 节。

**INV-5（可回收）**：pod 被删除后，entry 必须在有限个事件内被删除；
删除路径不得依赖可能为空或易变的字段。
- 理由：保证字段不会无限增长、不会自锁。
- 实现：目前唯一按 key 删除的路径依赖 `TaskInfo.NodeName` 非空。
- 当前状态：**不满足**，违反见 4.1、4.2、3.4。

**INV-6（写入幂等与迁移）**：同一 `(PodMeta, node)` 重复写入只覆盖不新增；
pod 改到新节点时，旧节点上的条目必须被移除。
- 实现：`AddUnassignedNumaPods` 对同一 key 覆盖（`cache.go:1191`），
  但没有旧节点清理。
- 当前状态：**前半满足，后半不满足**（4.6）。

### 2.3 并发与快照不变量

**INV-7（互斥与快照一致）**：所有读写持有 `sc.Mutex`；Snapshot 产出时
`UnassignedNumaPods` 与 `NumaSchedulerInfo` 对应同一时刻的状态。
- 实现：`AddUnassignedNumaPods` 与 `DeletePod` 持锁；`Snapshot` 持锁调用 Refresh 后再 Clone。
- 当前状态：**满足**（注意 Refresh 会就地修改 live cache，见 4.8）。

**INV-8（保守清空）**：只有在所有 entry 都被权威源（Numatopology CRD）确认分配后，
才允许整体清空；不得因部分确认而丢弃未确认条目。
- 实现：`unassignedNumber == 0` 才置 nil（`node_info.go:205-208`）。
- 当前状态：**满足**。但 all-or-nothing 会放大 INV-4 的违例：一条幽灵会让整张 map 无法清空。

### 2.4 不变量总览

| 编号 | 不变量 | 当前状态 | 相关位置 |
| --- | --- | --- | --- |
| INV-1 | 预占生效 | 满足 | `node_info.go:210-213` |
| INV-2 | 预占完备且不越界 | 部分满足（漏写无，越界写 pipeline） | `numaaware.go:263-292` |
| INV-3 | 节点正确且唯一 | 部分满足 | `cache.go:1181-1193` |
| INV-4 | 无幽灵 | **不满足** | `event_handlers.go:400-411` |
| INV-5 | 可回收 | **不满足** | `event_handlers.go:400-411` |
| INV-6 | 写入幂等与迁移 | 部分满足 | `cache.go:1181-1193` |
| INV-7 | 互斥与快照一致 | 满足 | `cache.go:1631-1666` |
| INV-8 | 保守清空 | 满足 | `node_info.go:205-208` |

## 3. 当前实现

### 3.1 写入

**写入集合的规则（INV-2）**：只写入**本 session 已经把 bind 提交到某个节点**的 task，
即 `AddBindTask` 成功的 task。这里的"决定"必须理解为"bind 已提交"，
而不是"调度器选定了节点"：

- 只是 `Statement.Allocate` 选中节点、尚未 `Commit` 的决策 → 不写（可能被 Discard）；
- `Pipeline` 的提名节点只是候选，本会话不会 bind → 不写；
- rollback（`unallocate`/`unPipeline`）的决策已撤销 → 不写；
- 被抢占的 victim / evicted task 不是新分配 → 不写；
- `Allocated` 但 `AddBindTask` 失败的 task 没有进入 bind 流程 → 不写。

此外，写入时该 pod 仍应存在于 scheduler cache（未被删除）。如果把写入点放在
`AddBindTask` 内，这一点由它已经完成的 `findJobAndTask` + `node.AddTask` 天然保证。

写入侧的职责边界：

- 写入侧只负责"bind 提交时写入"；
- bind 之后的失效（失败、pod 删除、改绑）属于删除侧职责，写入侧不"预判"；
- 条目生命周期可以跨 session，直到 CRD 确认分配，而不是"只在本 session 有效"。

当前实现（`numaaware.go:87-118`、`263-292`）：

- `AllocateFunc` 同时服务 allocate 和 pipeline（`session.go:805-812`、`statement.go:298-311`
  与 `session.go:763-769`、`statement.go:191-203`），`taskBindNodeMap` **不区分操作类型**；
- `Statement.Commit` 对 Pipeline 是空实现（`statement.go:414-415`），不会触发任何清理，
  于是 pipeline 记录会一路保留到 `OnSessionClose` 并被写入；
- `OnSessionClose` 汇总时**不检查**任务状态，也**不检查** pod 是否还存在；
- 最终由 `cache.AddUnassignedNumaPods` 写入（`cache.go:1176-1196`），同样不做存活校验。

因此当前实现**越界写入了 pipeline 任务**（INV-2 后半不满足），这也是场景 A 的根因；
而"被删除的 pod 仍被写入"则对应场景 C。

#### 3.1.1 如何判定"已 dispatch"

候选信号及其局限：

| 信号 | 含义 | 局限 |
| --- | --- | --- |
| session job 中 task 的状态 | `Binding` = `AddBindTask` 成功；`Pipelined` = pipeline；`Pending` = 回滚 | session 副本不反映 bind 失败 |
| cache 中 task 的状态/存在性 | 与 cache 当前状态一致 | bind 失败后、resync 处理前仍是 `Binding` |
| `AddBindTask` 成功这一事实 | 精确的 dispatch 点 | 只有在该点写入才能利用 |

具体地：

- `Statement.Commit → s.allocate` 里 `AddBindTask` 成功后才把状态改为 `Binding`
  （`statement.go:329-345`）；`Pipeline` 为 `Pipelined`（`statement.go:165`）。
  因此在 `OnSessionClose` 里用 `Status == api.Binding` 可以排除 pipeline 和回滚。
- 更简单的做法：`AllocateFunc` 被调用时状态已经是 `Allocated`（allocate 路径）或
  `Pipelined`（pipeline 路径）（`statement.go:274`、`statement.go:165`），
  在插件里记录操作类型即可，不必在关闭时反查 session job。
- 但 **bind 是异步的**（3.2），可能在 session 关闭前就失败，而 session 副本不会被更新；
  此时 session 状态仍是 `Binding`。仅靠状态过滤，`OnSessionClose` 仍会把失败的任务写回。
- 因此最可靠的判定点是 **`AddBindTask` 本身**：在该点写入，而不是在 `OnSessionClose` 事后重建。

#### 3.1.2 bind 失败后的自愈

现有机制：

- `Bind()` 失败分支：`taskUnschedulable` + `PreBindRollBack`（对已注册的 prebinder）
  + `resyncTask`（`cache.go:1121-1138`）；
- `resyncTask → syncTask`：pod 仍存在时用 `NewTaskInfo(pod)` 重建为 Pending；
  pod 已删时走 NotFound 分支清理；
- 下一次成功分配会覆盖同节点条目；pod 删除时 `DeletePod` 清理。

缺口：

1. bind 失败后 pod 并不在目标节点，条目却继续在失败节点上预占；
2. 若失败发生在 `OnSessionClose` 之前，按状态过滤的写入会把条目**再次写回**
   （写回晚于回滚），仅靠状态过滤无法自愈；
3. resync 重建 task 会清空 `NodeName`（4.2），之后的删除清不掉。

确定性自愈方案（与写入点前移配合）：

1. **写入在 `AddBindTask` 内、`BindFlowChannel <- bindContext` 之前完成**
   （`cache.go:1517-1525`，此时持有 `sc.Mutex`）。这样 bind goroutine 只可能在写入之后
   看到该任务，不存在"失败处理早于写入、随后又被写回"的窗口。
   `AddBindTask` 的两处调用（`Statement.allocate`，`statement.go:331`；
   `Session.dispatch`，`session.go:829`）都会经过这里，不需要在调用点分别实现；
   写在调用点（`AddBindTask` 返回之后）反而太晚：任务已入队，bind 可能已失败并清理，
   调用点再写会重新加回；
2. **失败后的清理用 resync 对账，而不是无条件删除**。bind 失败会触发
   `resyncTask → syncTask`，此时 `syncTask` 通常还能从 cache 取到带 `NodeName`（即条目
   所在节点）的 task，按读到的 pod 状态对账：

   | `syncTask` 读到的 pod | 处理 |
   | --- | --- |
   | `NotFound` | 删除该 UID 的所有条目 |
   | `spec.nodeName == ""` | 删除该 UID 的所有条目 |
   | `spec.nodeName == 条目所在节点` | **保留**（pod 确实在该节点，等 CRD 确认） |
   | `spec.nodeName` 为其他节点 | 删除本节点的条目 |

   不无条件删除的原因：bind 失败可能是 "pod 已被另一个调度器绑到节点 X"，若 X 正是目标
   节点，pod 其实在该节点上，条目应保留到 CRD 确认，否则会短暂丢失预占；
3. 其余路径作为兜底：删除端 UID 清理（8.2）、Snapshot 存活 prune（8.3）、
   换节点迁移（8.4）。

**若暂时不移动写入点（增量方案）**：保留 `OnSessionClose` 写入，则在 cache 中维护一个
session 作用域的 `bindFailed` UID 集合：`Bind()` 失败时加入集合并删除已有条目；
`AddUnassignedNumaPods` 写入时跳过集合中的 UID。注意同一 pod 重新分配并成功
`AddBindTask` 后要移除标记，否则会误跳过新的合法条目。

实现路径（推荐 A）：

- **A. 通用预约扩展（推荐）**：`numa-aware` 实现
  `BindContextHandler.SetupBindContextExtension`（`framework/interface.go:53-55`，
  `CreateBindContext` 在 `s.allocate` / `ssn.dispatch` 中调用），把 `ResNumaSets` 放进
  `bindCtx.Extensions`；`AddBindTask` 在 `node.AddTask` 成功后、入队前消费实现某个通用
  接口的扩展并写入 `UnassignedNumaPods`；`Bind()` 失败分支统一按 UID 清理。
  cache 不需要认识 `numa-aware` 插件名；`AddBindTask` 失败时本来就不会写入，无需回滚。
- **B. 插件侧写入 + 预绑定回滚**：插件在 `SetupBindContextExtension` 中写入
  （早于入队，bind 失败的回滚顺序仍安全），并实现 `PreBinder.PreBindRollBack`
  （bind 失败时由 `cache.go:1130-1133` 调用）清理；需要给插件暴露按 UID 清理条目的
  cache 接口。由于 `numa-aware` 只处理 Guaranteed pod，而唯一直接调用
  `ssn.Allocate` 的 `backfill` 只处理 BestEffort pod（`actions/backfill/backfill.go:121`、
  `164-169`），`AddBindTask` 失败时缺失 `DeallocateFunc` 兜底的问题对当前范围不适用。

无论选哪种，`OnSessionClose` 都不应再承担"事后补写"，否则缺口 2 无法消除。

### 3.2 bind 的时机

`Statement.Commit` 执行 `s.allocate` 时调用 `cache.AddBindTask`
（`statement.go:329-345`），把 BindContext 送入 `BindFlowChannel`（`cache.go:1525`）；
后台 `processBindTask` 每 20ms 消费一次（`cache.go:995`、`1533-1557`），
默认 `BATCH_BIND_NUM=1`（`cache.go:491-497`）。
因此 **bind 与 session 剩余调度并发**，`OnSessionClose` 时被 allocate 的 pod
可能已经 bind 成功。`cache.OnSessionClose()` 本身不触发 bind
（`cache.go:2013-2018`，只处理 shard coordinator 状态）。

### 3.3 读取与整体清空

`Snapshot` 对 live cache 的每个 NodeInfo 调用 `RefreshNumaSchedulerInfoByCrd`
（`cache.go:1651-1655`）：

1. 遍历 `UnassignedNumaPods`，用 `CheckNumaPodAssigned` 统计"CRD 未确认"的条目数；
2. `unassignedNumber == 0` → `UnassignedNumaPods = nil`；
3. 否则对 **map 中所有条目**（包括 CRD 已确认的）执行 `tmp.Allocate(resSets)`。

第 3 点是刻意的保守策略（`api/node_info_test.go:438-478`），代价是混合状态下
已分配条目会被再次扣除，可能少报可用 CPU。对本文而言更重要的是：
**只要有一条永远不被 CRD 确认的幽灵条目，整张 map 就永远进不了第 2 步**。

### 3.4 `TaskInfo.NodeName` 的来源与不稳定性

删除路径依赖 `TaskInfo.NodeName`，但它的值并不稳定：

1. **`NodeInfo.AddTask` 回写**：成功挂到节点时执行 `task.NodeName = ni.Name`
   （`api/node_info.go:498-500`）。`AddBindTask` 在 bind **之前**就调用
   `node.AddTask(cacheTask)`（`cache.go:1517-1518`），所以被 allocate 的 pod
   在 bind 前就已经有 `NodeName`。
2. **informer 更新**：`updatePod → addPod → NewTaskInfo` 重建 task，
   `NodeName` 取 `pod.Spec.NodeName`（`api/job_info.go:239-242`）。
   `updatePod` 有守卫：cache task 处于 allocated 状态（含 `Binding`）且
   `spec.nodeName` 为空时忽略事件（`event_handlers.go:363-368`），
   因此一般不会清掉来源 1 写入的值。

会变空的情况：

- **pipeline 的 task**：只修改 session 侧副本，不调用 `AddBindTask`，
  cache task 保持 `Pending`，`NodeName` 为空；
- **bind 失败后的 resync**：pod 仍存在时，`syncTask` 用 `NewTaskInfo(pod)` 重建 task
  并替换缓存（`event_handlers.go:331-338`），`NodeName` 变为空；
- **task 已不在 `sc.Jobs`**：回退 `NewTaskInfo(pod)`（`event_handlers.go:443-454`），
  未绑定 pod 的 `spec.nodeName` 为空。

## 4. 不变量违例分析

### 4.1 删除以 NodeName 为唯一索引（违反 INV-5）

`clearUnassignedNumaTask` 要求 `len(ti.NodeName) != 0` 且 `sc.Nodes[NodeName]` 存在
（`event_handlers.go:400-411`）。但字段按节点存储，没有 `UID -> node` 反向索引，
一旦删除事件到达时 `NodeName` 为空（原因见 3.4），条目就无法被删除，
也没有任何兜底路径会发现它。

### 4.2 bind 失败后的 resync 会清空 NodeName（违反 INV-5）

`Bind()` 失败后调用 `resyncTask`（`cache.go:1114-1138`）。`resyncTask` 只保存
`job/uid` key（`cache.go:1353-1369`），`processResyncTask` 用 key 重新取 task
（`cache.go:1396-1435`）；`syncTask` 在 pod 仍存在时用 `NewTaskInfo(pod)` 替换
cache task，`AddBindTask` 写入的 `NodeName` 被清空。此后 pod 被删除，
`DeletePod` 看到空 `NodeName` → 清不掉。

（pod 已 `NotFound` 时用的是 cache task，通常还带 `NodeName`，多数情况能清掉；
"resync 一定清不掉"不成立，风险在成功 resync 之后的删除。）

### 4.3 写入晚于删除（违反 INV-4）

`DeletePod` 与 `AddUnassignedNumaPods` 都持 `sc.Mutex`，无数据竞争，但顺序可以反过来：
删除事件先处理（map 里还没有条目），session 关闭时插件内存中的记录又被写入。
详见场景 C。

### 4.4 写入范围越界：Pipeline 任务也被写入（违反 INV-2，场景 A 的根因）

Pipeline 的语义是"资源不够，等正在释放的资源"
（`actions/allocate/allocate.go:1264-1273`）、抢占 preemptor
（`actions/preempt/preempt.go:414/528`）、reclaim（`actions/reclaim/reclaim.go:253`）。
这些 pod 本会话不会被 dispatch：

- cache task 保持 `Pending`、`NodeName` 为空，看起来像"已决定但未 bind"，实际是"未决定"；
- 后续真正被调度时会重新走 allocate 并重新写入，提前写入没有收益；
- 决策可能被后续 session 推翻（换节点/换 NUMA 位置），旧条目会在错误节点上预占；
- 它们被删除/重建的概率也更高，而清理逻辑对它们完全失效。

一种可能的辩护是"pipeline 提前预占可以防止别人抢走等待中的 CPU"，但并不成立：
victim 退出前这些 CPU 仍被 victim 占用、在 NumaInfo 中本来不可用；
victim 退出后下一个 session 会重新决策并写入条目，提前预占没有收益。

因此正确的修复方向是从**源头**只写 dispatch 的 task（见 8.1），
而不是依赖删除路径兜底。这一条同时会消除场景 A。

### 4.5 清空是 all-or-nothing 且依赖 CRD（放大 INV-4）

`CheckNumaPodAssigned` 只看 Numatopology 的 `PodAllocations`（`numa_info.go:213-223`；
UID 优先匹配，无 UID 的记录回退 namespace/name）。
一个从未运行就被删除的 pod 永远不会出现在 CRD 中 → 永远判为未分配 →
CPU 被永久预占，且整张 map 永远无法清空（INV-8 本身正确，但被 INV-4 违例卡死）。
`node_info.go:211` 的日志对 else 分支中**所有条目**打印，
所以日志里出现已删除 pod 的名字，就等于该 `PodMeta` 仍是 map 的 key。

### 4.6 同一 pod 的多节点重复条目（违反 INV-3/INV-6）

`AddUnassignedNumaPods` 以 `(PodMeta, node)` 为粒度无条件写入（`cache.go:1181-1193`）。
pod 先在 N1 分配失败、后在 N2 成功时，N1 的旧条目不会被主动删除：

- 它可能被 `CheckNumaPodAssigned` 按 UID 判为"已分配"，不参与计数；
- 只有当 N1 上没有其他未分配条目时，才会随整张 map 一起被清掉；
- 否则一直保留并参与预占。

### 4.7 没有周期性 GC / 存活校验（违反 INV-4 的兜底）

除"整体置 nil"外，没有任何逻辑校验 `PodMeta` 是否还对应存活的 pod
（例如 `sc.Jobs[*].Tasks` 中是否还有该 UID）。scheduler 重启会清空字段，
这既是当前事实上的清理手段，也说明缺少兜底。可行的兜底是在 `Snapshot` 时按 cache
task 状态对账（见 8.3）。

### 4.8 Snapshot 直接修改 live cache（INV-7 的边界）

`Snapshot()` 持锁对 `sc.Nodes` 中的真实 `NodeInfo` 调用 Refresh（`cache.go:1651-1655`），
会就地修改 `UnassignedNumaPods` 和 `NumaSchedulerInfo`。
这不是残留的直接原因，但任何"只想在快照上生效"的清理逻辑都会影响后续所有 session。

### 4.9 updatePod 路径不清理（非对称，当前无害）

`updatePod` 走 `deletePod + addPod`（`event_handlers.go:363-398`），不调用
`clearUnassignedNumaPod`。对仍存活的 pod 是正确的，但与 Delete 路径不对称，
若将来 update 语义变化（UID 变化、pod 被替换）容易再次引入漏洞。

## 5. 残留场景

共同点：**删除事件到达时，cache task 的 `NodeName` 为空**（从未设置，或被 resync 重建为空）。
pipeline 属于确定性风险（可由 8.1 从源头消除），其余多为竞态或边界路径。

### 场景 A：Pipeline 的 pod 在 bind 前被删除 / 重建（确定性，根因见 4.4）

1. 会话中 `stmt.Pipeline(task, node)` → `AllocateFunc` 记录 task；
   Pipeline 不改 cache，cache task 一直 `Pending`、`NodeName` 为空；
2. `OnSessionClose` 写入 `UnassignedNumaPods[pod] = cpuset`（越界写入，见 4.4）；
3. pod 在等待 victim 退出期间被删除（job 删除、驱逐、控制器重建）；
4. `DeletePod` 时 `NodeName` 仍为空 → 清理 no-op；
5. 若此后没有成功的 bind + CRD 记录，条目永久残留。

注意：这条路径的修复点在**写入端**（8.1），只要不写 pipeline 任务，场景 A 就不存在；
删除端的 UID 清理（8.2）只是兜底。

### 场景 B：Allocate 成功但 bind 失败，随后 pod 被删除（resync 竞态）

1. pod 被 allocate，`Statement.Commit` 期间 bind 已提交，`AddBindTask → node.AddTask`
   给 cache task 写入了目标节点 `NodeName`；`OnSessionClose` 同时写入条目；
2. bind 失败，`resyncTask` 入队；
3. resync 处理时 pod 仍存在，`NewTaskInfo(pod)` 替换 cache task，`NodeName` 变空；
4. 之后 pod 被删除，`DeletePod` 拿到空 `NodeName` → 残留。

若 pod 在 resync 处理前就被删除，反而能拿到仍带 `NodeName` 的 task 并清理成功。

### 场景 C：pod 在 session 中途被删除（写入晚于删除，违反 INV-4）

1. session 尚未结束，pod 被删除，`DeletePod` 先执行；
2. 此时 map 中还没有条目，清理无效；
3. session 关闭时 `OnSessionClose` 把插件内存记录补写回来；
4. 之后再无事件，条目残留。

前提是插件内存中已有记录：
- pipeline 路径：Pipeline 已提交，记录一定保留；
- allocate 路径：需要 `AddBindTask` 已执行。若删除发生在 `Statement.Commit` 之前，
  Commit 的 `findJobAndTask` 失败会触发 `unallocate → DeallocateFunc` 清掉记录，不会写入。

### 场景 D：cache 中 task 已不存在（边界路径）

删除事件乱序/重复，或 task 已被先前的 `updatePod → deletePod` 移除时，
`clearUnassignedNumaPod` 回退 `NewTaskInfo(pod)`（`event_handlers.go:443-454`），
未绑定 pod 的 `pod.Spec.NodeName` 为空 → 清理 no-op。

### 场景 E：同一 pod 换节点重调度（违反 INV-3/INV-6）

N1 分配失败留下条目，N2 成功。N2 的条目会随 CRD 分配被清；N1 的旧条目
只在 N1 没有其他未分配条目时才顺带清掉，否则长期保留。

### 场景 F：调度器重启（清除手段，非残留来源）

字段是内存态，重启即清空。可用于确诊，也说明缺少持久化与兜底。

### 不属于残留的情形

- 删除时 cache task 仍带 `NodeName`（已 allocate 且未被 resync 重建为空）：
  `DeletePod` 能正常清除；
- `DeletedFinalStateUnknown` 已由 `DeletePod` 显式处理（`event_handlers.go:611-617`）；
- 节点被删除/重建：条目随 `NodeInfo` 消失；
- 正常运行 pod 因 CRD 尚未更新而预占：设计意图，不算残留。

## 6. 后果

一条幽灵条目同时造成：

1. **CPU 永久预占**：Refresh 每次都把它的 cpuset 从 `NumaSchedulerInfo` 扣除；
2. **整张 map 无法清空**：`unassignedNumber > 0` 恒成立；
3. **predicate 失败**：新 Guaranteed pod 报
   `container xxx cannot be assigned by numa` / `cpu allocatable map is empty`
   （`numaaware.go:120-171`、`198-236`），表现为"NUMA 条件不满足，pod 调不下去"。

日志上每个调度周期都会打印
`because of the presence of unassigned N pods, pod <已删除的 pod> still pre-occupies resources ...`，
且 N 长期不下降。

## 7. 测试现状（修复前）

- `pkg/scheduler/cache/event_handlers_test.go:2154-2169`
  `TestClearUnassignedNumaTask_NoOpWhenTaskHasNoNode`：把"没有 NodeName 就不清理"
  固化为预期。
- `event_handlers_test.go:2137-2245`：覆盖按节点清理、job task 优先、回退 pod 信息，
  但全部是"pod 已有 NodeName"的形态。
- `pkg/scheduler/api/node_info_test.go:370-505`：覆盖 Refresh 的保留/清空/混合语义。
- `pkg/scheduler/plugins/numaaware/numaaware_test.go:182-349`：覆盖写入与 PodMeta 形态。

修复前的测试没有覆盖"写入后 `NodeName` 不可用时删除"的时序。修复后已补齐：
UID 清理、bind 失败对账、Snapshot 按状态与节点对账、以及 hermetic 集成测试。

## 8. 修复方案（已实施）

### 8.1 写入端修复：只写 dispatch 的 task（覆盖 INV-2）

分两步，建议一起做：

**第一步（最小改动，可先行）**：在 `numaaware.OnSessionClose` 汇总时按 session 中 task
的状态过滤，只保留 `Status == api.Binding` 的 task：

```go
// 伪代码
for taskID, nodeName := range pp.taskBindNodeMap {
    task := taskFromSession(ssn, taskID)
    if task == nil || task.Status != api.Binding {
        continue // pipeline（Pipelined）/ 已回滚（Pending）/ 未提交（Allocated）不写入
    }
    // ...收集 resSets
}
```

这能排除 pipeline 和回滚，**消除场景 A**；但它看不到 session 期间已经发生的 bind 失败
（session 副本仍是 `Binding`），因此仍可能把失败任务写回。

**第二步（推荐，彻底解决）**：把写入点前移到 `AddBindTask` 成功后、任务入队前；
失败后由 resync 对账清理（不无条件删除）。顺序保证为：

```text
AddBindTask（持 sc.Mutex）
  ├─ node.AddTask 成功
  ├─ 写入 UnassignedNumaPods          ← 写入
  └─ BindFlowChannel <- bindContext   ← 入队
                                        ↓
                              bind goroutine 取到任务并执行
                                        ↓ 失败
                              resyncTask → syncTask 读取 pod 对账
                                        （晚于写入，见 3.1.2 的表格）
```

这样：

- 只写真正进入 bind 流程的 task（pipeline/回滚/AddBindTask 失败都不会写）；
- bind 失败一定在写入之后被发现，不存在"回滚后又被写回"；
- 对账能区分"pod 未绑定/已删"（删除条目）和"pod 已绑到同节点"（保留条目）；
- pod 在 session 中途被删除时，bind 失败同样会清理；即使 bind 成功，
  后续 `DeletePod`（8.2）也会清理。

实现方式见 3.1.2 的方案 A（BindContext 扩展）或方案 B（插件写入 + `PreBindRollBack`）。

### 8.2 删除不再依赖 NodeName（覆盖 INV-4/INV-5，兜底 B/D/E）

在 `DeletePod` 与 `syncTask NotFound` 路径中，按 `PodMeta.UID` 遍历所有节点删除：

```go
// 伪代码
func (sc *SchedulerCache) clearUnassignedNumaPod(pod *v1.Pod) {
    meta := schedulingapi.PodMeta{UID: pod.UID, Name: pod.Name, Namespace: pod.Namespace}
    for _, node := range sc.Nodes {
        delete(node.UnassignedNumaPods, meta)
    }
}
```

单张 map 中最多一条记录，遍历成本可忽略；覆盖场景 B、D、E。
若担心性能，可维护 `UID -> nodeName` 反向索引。

场景 C（删除早于写入）无法靠删除路径解决，需要 8.3。

### 8.3 加固：Snapshot 时按 cache 状态对账（覆盖场景 A/B/C 与历史遗留）

在 `Snapshot` 中、调用 `RefreshNumaSchedulerInfoByCrd` 之前，先按 cache 中的 task 状态
剔除条目：

- 条目 UID 不在任何 `sc.Jobs[*].Tasks` 中 → 剔除（pod 已删除 / 历史遗留）；
- 对应 task 处于**明确不再需要预占**的状态 → 剔除：`Pending`（从未 dispatch /
  pipeline / bind 失败 resync 后）、`Pipelined`（session 专用，新写入路径下不应有条目）、
  `Succeeded`/`Failed`（已终止）。其余状态（`Allocated`/`Binding`/`Bound`/`Running`/
  `Releasing`/`Unknown` 以及未来新增状态）一律**保留**。
  采用 deny-list 而不是 allow-list：`Unknown`（kubelet 失联）的 pod 可能仍在节点上
  占用资源，不能当幽灵删除；未来新增状态默认保留，避免提前释放预占。
- 条目所在节点必须与 task 当前 `NodeName` 一致 → 不一致则剔除。`AddBindTask` 会把目标
  节点回写到 task，因此 in-flight/已绑定 task 的 `NodeName` 是权威的；这条能清掉
  "task 已改绑、旧节点条目未对上账"的残留。

```go
// Snapshot 内，持 sc.Mutex
inflight := make(map[types.UID]string) // UID -> task 当前所在节点
for _, job := range sc.Jobs {
    for uid, task := range job.Tasks {
        if keepNumaReservation(task.Status) {
            inflight[types.UID(uid)] = task.NodeName
        }
    }
}
for _, node := range sc.Nodes {
    for meta := range node.UnassignedNumaPods {
        if nodeName, ok := inflight[meta.UID]; ok && nodeName == node.Name {
            continue
        }
        delete(node.UnassignedNumaPods, meta)
    }
    node.RefreshNumaSchedulerInfoByCrd()
}
```

要点：

- **必须使用 cache task 状态，不能使用 pod phase**：已 dispatch、bind 在途的 pod，
  phase 同样是 `Pending`、`spec.nodeName` 同样为空，但对应 task 状态是 `Binding`，
  必须保留——这正是字段要保护的核心窗口；
- 放在 `Snapshot`（能访问 `sc.Jobs`），不要放进 api 层的
  `RefreshNumaSchedulerInfoByCrd`；
- 这是兜底对账：条目可能在状态变化后的下一个 Snapshot 才被清理，期间保持保守预占。

效果：

- pipeline 条目（cache 中为 `Pending`）在下一个 Snapshot 被剔除 → 场景 A；
- bind 失败且 resync 已重建为 `Pending` 的条目被剔除 → 场景 B；
- 删除早于写入产生的条目（task 已不在 `sc.Jobs`）被剔除 → 场景 C；
- 剔除发生在 Refresh 统计 `unassignedNumber` 之前，幽灵条目不会阻止整表清空。

### 8.4 加固：按 UID 写入 / 迁移（覆盖 INV-3/INV-6）

`AddUnassignedNumaPods` 写入前，先删除该 `PodMeta` 在其他节点上的旧条目。

### 8.5 修复顺序与测试

1. 写入端过滤 pipeline/rollback（8.1 第一步）；
2. 写入点前移 + bind 失败按 UID 回滚（8.1 第二步）；
3. 修改 `clearUnassignedNumaTask/Pod` 的删除方式（8.2）；
4. 调整 `TestClearUnassignedNumaTask_NoOpWhenTaskHasNoNode` 的预期；
5. 新增用例：
   - "pipeline 任务不写入 UnassignedNumaPods"；
   - "bind 失败的 task 不写入，或写入后在失败分支被清理"；
   - "条目写入后 pod 被删除（cache task 无 NodeName）→ 条目被清理"；
   - "pod 在 session 中途被删除，session close 后不得重新写入"；
   - "同一 pod 在 N1 分配失败、N2 成功 → N1 的旧条目被清除"；
6. 补充 8.3 的 Snapshot 状态对账及对应用例（含"in-flight Binding 条目不得被剔除"）。

## 9. 线上排查清单

1. V(3) 日志搜 `reserved resources .* before bind` 与 `deleted unassigned pod` /
   `released unassigned pod` / `pruned unassigned numa pod`，对比同一 `PodMeta`
   是否"有加无删"；
2. 搜 `because of the presence of unassigned`，观察数字是否长期不降；
3. 用日志中的 pod 名/UID 查 apiserver：已 `NotFound`，且历史中 `spec.nodeName`
   始终为空或绑定失败 → 基本可确诊；
4. 确认 pod 是否来自 pipeline、bind 失败后 resync、或 session 中途删除；
5. 临时规避：重启 scheduler 或删除并重新添加对应节点；
6. 长期修复：按第 8 节改动并补测试。

## 10. 写入侧实现（Phase 2，已落地）

### 10.1 目标与约束

- 只有 `AddBindTask` 成功（bind 已提交）的 task 才写入；
- 写入必须发生在 bind goroutine 看到任务之前，保证"失败清理晚于写入"；
- 插件不直接操作 cache，cache 也不需要认识具体插件名；
- 不改变现有 predicate / 资源决策逻辑。

**为什么写在 `AddBindTask` 更清晰**：

1. 写入点与状态转移重合：条目的语义是"bind 已提交、CRD 未确认"，而 `AddBindTask`
   正是 cache task 置为 `Binding` 的地方；
2. 单一事实来源：cache 已拥有节点/task/bind 状态与该字段，不需要插件维护
   `taskBindNodeMap` 这类影子状态，避免"影子状态与真实状态漂移"；
3. 写入集合天然正确：pipeline、未 Commit、rollback 从构造上被排除，无需按状态启发式过滤；
4. 顺序确定：写入在入队前，失败清理必然晚于写入；
5. 存活条件天然满足：`findJobAndTask` + `node.AddTask` 已保证 task/pod 存在；
6. 简化插件：`OnSessionClose`、`taskBindNodeMap`、`taskPodMetaMap` 都可删除。

**本方案不解决的问题**：

- 删除侧仍需 UID 清理与 Snapshot 对账（8.2、8.3）；
- 失败清理必须异步（交给 `processResyncTask`），不能在 `Bind()` 失败循环里同步加锁；
- `RefreshNumaSchedulerInfoByCrd` 的 all-or-nothing 语义不变，只是幽灵来源大幅减少。

### 10.2 数据流

```text
numa-aware.predicateFn
   └─ assignRes[taskUID][node] = ResNumaSets

Statement.Commit → s.allocate / Session.dispatch
   └─ CreateBindContext(task)
        └─ numa-aware.SetupBindContextExtension(state, bindCtx)
             bindCtx.Extensions[PluginName] = numaReservation{resSets}

cache.AddBindTask（持 sc.Mutex）
   ├─ findJobAndTask / 节点存在
   ├─ 状态置 Binding、SetPodResourceDecision、Clone NumaInfo
   ├─ node.AddTask(task)
   ├─ reserveBindNumaResources(node, task, bindCtx)   ← 写入
   └─ BindFlowChannel <- bindCtx                  ← 之后 bind 才可能开始
```

### 10.3 代码改动

**cache 侧**（`pkg/scheduler/cache/cache.go`）

```go
// 与 BindContext 放在一起
type BindNumaReservation interface {
    ReservedNumaSets() schedulingapi.ResNumaSets
}

// AddBindTask 内，node.AddTask 成功之后、BindFlowChannel 发送之前：
sc.reserveBindNumaResources(node, task, bindContext)
sc.BindFlowChannel <- bindContext
```

```go
// 假定已持有 sc.Mutex
func (sc *SchedulerCache) reserveBindNumaResources(node *schedulingapi.NodeInfo,
    task *schedulingapi.TaskInfo, bindCtx *BindContext) {
    for _, ext := range bindCtx.Extensions {
        reservation, ok := ext.(BindNumaReservation)
        if !ok {
            continue
        }
        resSets := reservation.ReservedNumaSets()
        if len(resSets) == 0 {
            continue
        }
        if node.UnassignedNumaPods == nil {
            node.UnassignedNumaPods = make(map[schedulingapi.PodMeta]schedulingapi.ResNumaSets)
        }
        node.UnassignedNumaPods[schedulingapi.PodMeta{
            UID: task.Pod.UID, Name: task.Pod.Name, Namespace: task.Pod.Namespace,
        }] = resSets
    }
}
```

**numa-aware 侧**（`pkg/scheduler/plugins/numaaware/numaaware.go`）

```go
func (pp *numaPlugin) SetupBindContextExtension(_ *k8sframework.CycleState,
    bindCtx *cache.BindContext) {
    task := bindCtx.TaskInfo
    if task == nil || task.NodeName == "" {
        return
    }
    pp.Lock()
    defer pp.Unlock()
    resSets, ok := pp.assignRes[task.UID][task.NodeName]
    if !ok {
        return // 非 Guaranteed / 未做 NUMA 决策，不写
    }
    bindCtx.Extensions[pp.Name()] = &numaReservation{resSets: resSets.Clone()}
}

type numaReservation struct{ resSets api.ResNumaSets }

func (r *numaReservation) ReservedNumaSets() api.ResNumaSets { return r.resSets }
```

注意：实现该接口会让 `numaaware` 直接 import `pkg/scheduler/cache`（`predicates` 已有同样先例）。
插件只是往 `bindCtx.Extensions` 放入一个不透明值，不读写 cache 内部状态；
`UnassignedNumaPods` 的读写仍完全由 cache 拥有。若想弱化这层类型依赖，可以把
`BindNumaReservation` 与携带的数据类型放到 `pkg/scheduler/api`。

同时删除：

- `numaPlugin.taskBindNodeMap` / `taskPodMetaMap` 字段及 `AllocateFunc` 中的赋值
  （`numaaware.go:48-57`、`100-101`）；
- `OnSessionClose` 的汇总写入逻辑（`numaaware.go:263-292`），保留空实现以满足接口；
- `Session.AddUnassignedNumaPods`（`session.go:965-967`）与
  `cache.AddUnassignedNumaPods`（`cache.go:1176-1196`、`interface.go:80`），
  避免出现第二条写入路径。

**失败路径**（`pkg/scheduler/cache/event_handlers.go` 的 `syncTask`）

写入点前移后，bind 失败必然晚于写入；由 resync 对账（见 3.1.2 的表格），
不在 `Bind()` 失败循环里直接加锁清理，避免和 `processBindTask` 的同步调用链互相等待。

### 10.4 边界条件

- 非 Guaranteed、NUMA policy 为 none/空、节点无 NumaInfo：`assignRes` 无记录，
  扩展不会出现，行为与现在一致；
- `CreateBindContext` 只填充 bindCtx，不产生副作用，因此 `AddBindTask` 失败不会留下条目；
- 同一 task 重复 dispatch：同 key 覆盖，幂等；
- 多个扩展同时实现接口：按资源取并集，或明确约定只有一个；
- `bindCtx.Extensions` 由 `CreateBindContext` 初始化为非 nil map。

### 10.5 并发与顺序

- 写入在 `sc.Mutex` 内、入队前完成；
- bind goroutine 只能在入队后看到任务；
- 失败对账在 `processResyncTask` 独立 goroutine 中加锁执行，不与 `processBindTask` 形成等待环；
- 因此"写入 → 失败 → 清理"的顺序有保证。

### 10.6 测试计划

- cache：带扩展的 `AddBindTask` 写入条目；无扩展不写；`node.AddTask` 失败不写；
- numaaware：predicate + allocate 后 `CreateBindContext` 生成的扩展内容正确；
  pipeline 不产生 `AddBindTask`，因此不写；
- `syncTask` 对账：pod 未绑定/已删 → 删除；pod 绑到同节点 → 保留；绑到其他节点 → 删除；
- 回归：移除 `OnSessionClose` 写入后，原有 numaaware / cache 用例更新。

### 10.7 风险与回滚

- 最大风险是"漏写"：必须确认所有 bind 都经过 `AddBindTask`（`BindFlowChannel` 的唯一生产者），
  且插件在 `CreateBindContext` 时 `assignRes` 已经就绪（predicate 先于 allocate）；
- 写入逻辑与删除旧写入路径应在同一个提交内完成，避免出现"两边都不写"的中间态；
- 该改动可以独立于 Phase 1 回滚：回滚后条目改回 `OnSessionClose` 汇总写入。

## 11. 删除侧实现（已落地）

写入点前移后，条目的语义是"bind 已提交，结果未定或已成功但 CRD 未确认"。
删除侧需要保证：在下列任一事件之后的有限时间内，条目被移除。

| 事件 | 位置 | 动作 |
| --- | --- | --- |
| pod 被删除 | `DeletePod`（`event_handlers.go:605-637`） | 按 UID 全局删除 |
| bind 失败，resync 发现 pod `NotFound` | `syncTask`（`event_handlers.go:315-327`） | 按 UID 全局删除 |
| bind 失败，resync 发现 pod 仍存在 | `syncTask`（`event_handlers.go:331-338`） | 按 `pod.Spec.NodeName` 对账 |
| 每个 Snapshot | `Snapshot`（`cache.go:1631-1666`），Refresh 之前 | 按 cache task 状态对账 |
| 分配被 CRD 确认 | `RefreshNumaSchedulerInfoByCrd`（`node_info.go:205-208`） | 整体清空（现有逻辑保留） |

### 11.1 按 UID 全局删除

当前 `clearUnassignedNumaTask` 依赖 `TaskInfo.NodeName`（`event_handlers.go:400-411`）。
改为按 `PodMeta.UID` 遍历所有节点删除（假定调用方已持有 `sc.Mutex`）：

```go
func (sc *SchedulerCache) clearUnassignedNumaPodByUID(uid types.UID) {
    for _, node := range sc.Nodes {
        for meta := range node.UnassignedNumaPods {
            if meta.UID == uid {
                delete(node.UnassignedNumaPods, meta)
            }
        }
    }
}
```

`DeletePod` 直接用它（不再需要从 `sc.Jobs` 反查 task）；`syncTask` 的 `NotFound`
分支同样用它。单节点条目数很少，O(nodes) 扫描可接受；后续如有性能需要再引入
`UID -> node` 反向索引。

### 11.2 bind 失败后的对账

`syncTask` 在 pod 存在时，按读到的 `pod.Spec.NodeName` 对账：

- `spec.nodeName == ""`：pod 未绑定 → 按 UID 删除；
- `spec.nodeName == 某个节点`：只保留该节点上的条目，删除其他节点上的同 UID 条目；
- `NotFound`：按 UID 删除。

**为什么 bind 失败时 pod 仍可能已有 `spec.nodeName`**：`DefaultBinder.Bind`
把 `Pods.Bind()` 的任何 error 都当作失败（`cache.go:251-261`），但报错不等于没绑上：

1. **假失败**：binding 已写入，响应超时/中断/5xx 丢失。pod 实际就在目标节点，
   条目必须保留；
2. **已被别人绑定**：多 scheduler/多实例、用户直接设置 `spec.nodeName`、或本实例
   上一轮实际成功但未观察到，第二次 bind 返回 `Conflict`。可能绑在目标节点，也可能
   绑在其他节点；
3. **resync 滞后**：`resyncTask` 是限速队列 + 独立 goroutine，执行时 pod 可能已被
   **后续 session** 重新调度并绑到另一个节点。

因此对账必须以 pod **当前**的 `spec.nodeName` 为准：保留该节点条目、删除其他节点的同
UID 条目。**不能**写成"`newPod.Spec.NodeName != oldTask.NodeName` 就按 UID 全删"——
一个过期的 resync 会把后续 session 刚写入的新条目误删，造成真实的预占丢失。

实现放在 `syncTask`（`processResyncTask` 是独立 goroutine），**不要**放进
`Bind()` 的失败循环：那里同步获取 `sc.Mutex` 可能与满队列阻塞的 `AddBindTask`
形成等待环。

### 11.3 Snapshot 兜底对账

见 8.3：在 `Snapshot` 中、`RefreshNumaSchedulerInfoByCrd` 之前，剔除

- UID 不在任何 `sc.Jobs[*].Tasks` 中的条目；
- 对应 cache task 处于 `Pending`、`Pipelined`、`Succeeded`、`Failed` 的条目
  （`keepNumaReservation` 返回 false）；
- 条目所在节点与 task 当前 `NodeName` 不一致的条目（task 已改绑，旧节点未对上账）。

它不依赖单个事件是否正确送达或处理，周期性按 cache 真实状态收敛，覆盖
事件驱动路径未能清理的情况（删除早于写入、handler 提前返回、informer 中断/迟到、
resync 对账漏跑）、历史遗留和未来新增写入路径的漏洞；deny-list 的判定和节点一致性
校验保证 in-flight 的 `Binding`、仍在终止的 `Releasing` 以及 `Unknown` 的 pod
都不会被误删。

### 11.4 写入时的跨节点清理（可选）

`reserveBindNumaResources` 写入前，先删除同一 UID 在其他节点上的旧条目，
这样"N1 失败、N2 成功"不会在 N1 留下陈旧条目。不实现也可以，由 11.2/11.3 兜底。

### 11.5 删除侧明确不做的事

- 不在 `Bind()` 失败循环里同步清理；
- 不使用 pod phase 判断（in-flight pod 也是 Pending）；
- 暂不改变 `RefreshNumaSchedulerInfoByCrd` 的 all-or-nothing 语义；
- 暂不引入反向索引，先扫描 `sc.Nodes`。

### 11.6 测试计划

- `DeletePod`：cache task 无 `NodeName` 时也能删除条目；
- `syncTask`：NotFound / 未绑定 / 绑到同节点 / 绑到其他节点 四种情况；
- `Snapshot` 对账：`Binding` 保留，`Pending` 与 task 缺失被清除；
- 多节点旧条目在重新绑定后被清除；
- 回归：`TestClearUnassignedNumaTask_NoOpWhenTaskHasNoNode` 的预期需要反转。

### 11.7 实施顺序

1. 11.1 + 11.3：独立、低风险，直接解决当前残留问题；
2. 11.2：与写入侧一起做，缩短失败后的预占释放时间；
3. 11.4：按需补充。
