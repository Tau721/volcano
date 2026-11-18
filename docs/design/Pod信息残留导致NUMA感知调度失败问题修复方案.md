# Pod 信息残留导致 NUMA 感知调度失败问题修复方案

> 状态：已实施。代码见 `pkg/scheduler/cache`、`pkg/scheduler/api/numa_info.go`、
> `pkg/scheduler/plugins/numaaware`；单测与 hermetic 集成测试已覆盖。

## 1. 问题描述

启用 `numa-aware` 插件后，Guaranteed pod 无法调度，predicate 报 NUMA 条件不满足
（`container xxx cannot be assigned by numa` / `cpu allocatable map is empty`）。
scheduler 日志持续出现：

```text
because of the presence of unassigned N pods, pod <已经删除的 pod> still pre-occupies resources ...
```

日志中的 pod 早已被删除，但它的 `PodMeta` 仍留在节点的 `UnassignedNumaPods` 中，
其 CPU 集合被永久预占，并且让整张表无法清空。

## 2. 根因分析

### 2.1 字段语义

`NodeInfo.UnassignedNumaPods`（`pkg/scheduler/api/node_info.go:75`）记录：

> 调度器已为某个 pod 在本节点确定 NUMA 资源分配，但 Numatopology CRD 尚未确认。

它用于填补"调度决策 → kubelet 分配 → 资源导出器写回 CRD"之间的时间差，
防止同一批 CPU 被重复分配导致 `TopologyAffinityError`。

### 2.2 修复前的维护方式

| 动作 | 位置 |
| --- | --- |
| 记录 | `numaaware.AllocateFunc`（`numaaware.go:87-118`）记录插件内存，`OnSessionClose`（`263-292`）汇总写入 |
| 删除 | `DeletePod` / `syncTask NotFound` → `clearUnassignedNumaTask`（`event_handlers.go:400-411`），依赖 `TaskInfo.NodeName` |
| 整体清空 | `RefreshNumaSchedulerInfoByCrd`（`node_info.go:205-208`），要求所有条目都被 CRD 确认 |
| 判定已分配 | `CheckNumaPodAssigned`（`numa_info.go:213-223`），通过 CRD 的 `PodAllocations` 判断；有 UID 的记录按 UID 匹配，无 UID 的记录回退到 namespace/name。修复前是 UID 或名字任一命中，同名重建的 pod 可能被旧记录误判为已分配 |

### 2.3 写入侧缺陷

1. **不区分操作类型**：`AllocateFunc` 同时服务 allocate 和 pipeline
   （`statement.go:191-203/298-311`、`session.go:763-769/805-812`），
   `taskBindNodeMap` 不记录类型。pipeline 只是提名/等待资源，本会话不会 bind，
   却被写入。
2. **不感知 bind 结果**：bind 异步且与 session 并发（`AddBindTask` 在
   `Statement.Commit` 中入队，`cache.go:1491-1531`），失败后 session 副本仍是
   `Binding`，`OnSessionClose` 照常写入。
3. **不检查 pod 存活**：session 期间 pod 被删除后，插件记录仍在，关闭时照常写入。
4. **写入时机固定**：只在 session 关闭时汇总，无法表达"已提交待 bind"这一状态，
   也无法在失败后撤销。

### 2.4 删除侧缺陷

1. **依赖易变字段**：`clearUnassignedNumaTask` 要求 `ti.NodeName != ""`，
   而 pipeline 的 cache task 没有 `NodeName`、bind 失败 resync 重建的 task
   会被清空 `NodeName`、task 不在 cache 时回退的 pod 也未绑定。
2. **删除早于写入**：pod 在 session 中途被删除时，`DeletePod` 先执行（无条目），
   `OnSessionClose` 随后写入，之后再无事件清理。
3. **无跨节点清理 / 无周期 GC**：换节点重调度留下的旧条目，以及事件驱动路径
   未能清理的情况（删除早于写入、handler 提前返回、informer 中断/迟到），
   都没有兜底路径清理。

### 2.5 为什么残留会自锁

`RefreshNumaSchedulerInfoByCrd` 只有在 `unassignedNumber == 0` 时把 map 置 nil；
一条从未真正运行就被删除的 pod 永远不会出现在 CRD 的 `PodAllocations` 中，
于是永远被判为"未分配"：

- 其 CPU 集合每次 Snapshot 都被扣除；
- 整张 map 永远无法清空（其他本可清理的条目也被一起保留）；
- 节点 NUMA 可用资源永久减少，新 pod predicate 失败。

## 3. 修复目标

### 3.1 写入集合的精确定义

> 只写入**本 session 已经把 bind 提交到某个节点**的 task，即 `AddBindTask` 成功的 task。

"决定"必须理解为"bind 已提交"，以下都不应写入：

- 只是 `Statement.Allocate` 选中节点、尚未 `Commit` 的决策；
- `Pipeline` 的提名节点（只是候选，本会话不会 bind）；
- rollback（`unallocate`/`unPipeline`）的决策；
- 被抢占的 victim / evicted task；
- `Allocated` 但 `AddBindTask` 失败的 task。

### 3.2 需要保证的不变量

| 编号 | 不变量 |
| --- | --- |
| INV-1 | 每条 entry 的 cpuset 必须已从节点 `NumaSchedulerInfo` 扣除 |
| INV-2 | entries 与"已提交 bind、CRD 未确认"的 pod 集合一一对应（不漏、不越界） |
| INV-3 | 同一 pod 最多一条 entry，且位于其 bind 的节点 |
| INV-4 | 每条 entry 对应的 pod 必须仍存在于 scheduler cache |
| INV-5 | pod 删除/失败/改绑后，条目必须在有限事件内被删除，且不依赖易变字段 |
| INV-6 | 重复写入幂等；换节点时旧条目被移除 |
| INV-7 | 所有读写持有 `sc.Mutex`，写入与失败清理顺序确定 |
| INV-8 | 只有全部条目被 CRD 确认后才整体清空 |

### 3.3 总体思路

```text
写入：状态转移时记录（AddBindTask 成功 → 写入）
删除：状态失效时清理（pod 删除 / bind 失败 / 改绑 / 兜底对账）
判定：CRD 全量确认 → 整体清空（现有逻辑保留）
```

## 4. 写入侧设计

### 4.1 数据流

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

### 4.2 cache 侧：定义扩展契约并在 AddBindTask 消费

在 `BindContext` 旁定义通用接口（`pkg/scheduler/cache/cache.go`）：

```go
// BindNumaReservation 由 BindContext extension 实现：
// bind 提交时由 cache 为其预占资源。
type BindNumaReservation interface {
    ReservedNumaSets() schedulingapi.ResNumaSets
}
```

在 `AddBindTask`（`cache.go:1517-1525`）中，`node.AddTask` 成功之后、
`sc.BindFlowChannel <- bindContext` 之前插入写入：

```go
if err := node.AddTask(task); err != nil {
    job.UpdateTaskStatus(task, originalStatus)
    return err
}

// bind 提交前完成预占，保证之后的失败清理一定晚于写入。
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

### 4.3 插件侧：提供扩展

`numa-aware` 实现 `framework.BindContextHandler`
（`pkg/scheduler/framework/interface.go:53-55`，`CreateBindContext` 在
`session.go:845-860` 调用）：

```go
func (pp *numaPlugin) SetupBindContextExtension(
    _ *k8sframework.CycleState, bindCtx *cache.BindContext) {
    task := bindCtx.TaskInfo
    if task == nil || task.NodeName == "" {
        return
    }
    pp.Lock()
    defer pp.Unlock()
    resSets, ok := pp.assignRes[task.UID][task.NodeName]
    if !ok {
        return // 非 Guaranteed / 未做 NUMA 决策 / policy 为 none
    }
    bindCtx.Extensions[pp.Name()] = &numaReservation{resSets: resSets.Clone()}
}

type numaReservation struct{ resSets api.ResNumaSets }

func (r *numaReservation) ReservedNumaSets() api.ResNumaSets { return r.resSets }
```

说明：实现该接口会让 `numaaware` 直接 import `pkg/scheduler/cache`
（`predicates` 已有同样先例）。插件只是向 `bindCtx.Extensions` 放入不透明数据，
不读写 cache 内部状态；`UnassignedNumaPods` 的读写仍完全由 cache 拥有。

### 4.4 删除旧的写入路径

- 删除 `numaPlugin.taskBindNodeMap` / `taskPodMetaMap` 字段及 `AllocateFunc` 中的赋值
  （`numaaware.go:48-57`、`100-101`）；
- 删除 `OnSessionClose` 的汇总逻辑（`numaaware.go:263-292`），保留空实现以满足接口；
- 删除 `Session.AddUnassignedNumaPods`（`session.go:965-967`）、
  `cache.AddUnassignedNumaPods`（`cache.go:1176-1196`）及接口声明
  （`interface.go:80`），避免出现第二条写入路径。

保留 `assignRes`：predicate 写、`SetupBindContextExtension` 读、`DeallocateFunc`
在 rollback 时清理。

### 4.5 顺序保证

- 写入在 `sc.Mutex` 内、入队前完成；
- bind goroutine 只能在入队后看到任务；
- 失败对账在 `processResyncTask` 独立 goroutine 中加锁执行；
- 因此"写入 → 失败 → 清理"顺序确定，不存在"回滚后又被写回"。

### 4.6 边界条件

- 非 Guaranteed、policy 为 none/空、节点无 NumaInfo：`assignRes` 无记录，扩展不出现；
- `CreateBindContext` 只填充 bindCtx，无副作用，`AddBindTask` 失败不会留下条目；
- 同一 task 重复 dispatch：同 key 覆盖，幂等；
- `bindCtx.Extensions` 由 `CreateBindContext` 初始化为非 nil map。

## 5. 删除侧设计

写入点前移后，条目的语义是"bind 已提交，结果未定或已成功但 CRD 未确认"。
删除侧需要覆盖四类事件：

| 事件 | 位置 | 动作 |
| --- | --- | --- |
| pod 被删除 | `DeletePod`（`event_handlers.go:605-637`） | 按 UID 全局删除 |
| bind 失败，resync 发现 pod `NotFound` | `syncTask`（`event_handlers.go:315-327`） | 按 UID 全局删除 |
| bind 失败，resync 发现 pod 仍存在 | `syncTask`（`event_handlers.go:331-338`） | 按 `pod.Spec.NodeName` 对账 |
| 每个 Snapshot | `Snapshot`（`cache.go:1631-1666`），Refresh 之前 | 按 cache task 状态对账 |
| CRD 确认分配 | `RefreshNumaSchedulerInfoByCrd`（`node_info.go:205-208`） | 整体清空（保留） |

### 5.1 按 UID 全局删除

修复前 `clearUnassignedNumaTask` 依赖 `TaskInfo.NodeName`（`event_handlers.go:400-411`）。
现改为按 `PodMeta.UID` 遍历所有节点删除（调用方已持有 `sc.Mutex`）：

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

`DeletePod` 直接使用它（不再需要从 `sc.Jobs` 反查 task）；`syncTask` 的
`NotFound` 分支同样使用。每节点条目数很少，O(nodes) 扫描可接受；
后续如有性能需要再引入 `UID -> node` 反向索引。

### 5.2 bind 失败后的对账

`syncTask` 在 pod 存在时，按读到的 `pod.Spec.NodeName` 对账：

- `spec.nodeName == ""`：pod 未绑定 → 按 UID 删除；
- `spec.nodeName == 某个节点`：只保留该节点上的条目，删除其他节点上的同 UID 条目；
- `NotFound`：按 UID 删除。

**为什么 bind 失败时 pod 仍可能已有 `spec.nodeName`**：
`DefaultBinder.Bind` 把 `Pods.Bind()` 的任何 error 都当作失败（`cache.go:251-261`），
但报错不等于没绑上：

1. **假失败**：binding 已写入，响应超时/中断/5xx 丢失。pod 实际就在目标节点，
   条目必须保留；
2. **已被别人绑定**：多 scheduler/多实例、用户直接设置 `spec.nodeName`、或本实例
   上一轮实际成功但未观察到，第二次 bind 返回 `Conflict`。可能绑在目标节点，
   也可能绑在其他节点；
3. **resync 滞后**：`resyncTask` 是限速队列 + 独立 goroutine，执行时 pod 可能已被
   **后续 session** 重新调度并绑到另一个节点。

因此对账必须以 pod **当前**的 `spec.nodeName` 为准：保留该节点条目、删除其他节点的
同 UID 条目。**不能**写成"`newPod.Spec.NodeName != oldTask.NodeName` 就按 UID 全删"——
一个过期的 resync 会把后续 session 刚写入的新条目误删，造成真实的预占丢失。

实现放在 `syncTask`（`processResyncTask` 是独立 goroutine），**不要**放进
`Bind()` 的失败循环：那里同步获取 `sc.Mutex` 可能与满队列阻塞的 `AddBindTask`
形成等待环。

### 5.3 Snapshot 兜底对账

在 `Snapshot` 中、调用 `RefreshNumaSchedulerInfoByCrd` 之前，剔除：

- UID 不在任何 `sc.Jobs[*].Tasks` 中的条目；
- 对应 cache task 处于 `Pending`、`Pipelined`、`Succeeded` 或 `Failed` 的条目
  （`keepNumaReservation` 返回 false）。其余状态——包括 `Releasing` 和 `Unknown`——
  一律保留。`Releasing` 的 pod 仍在节点上终止；`Unknown`（kubelet 失联）不能假设资源已释放。
  采用 deny-list 而非 allow-list，未来新增状态默认保留，避免提前释放预占；
- 条目所在节点与 task 当前 `NodeName` 不一致的条目（task 已改绑、旧节点未对上账）。

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

- **必须使用 cache task 状态，不能使用 pod phase**：已 dispatch、bind 在途的 pod
  phase 也是 `Pending`，但对应 task 状态是 `Binding`，必须保留；
- 剔除发生在 Refresh 统计 `unassignedNumber` 之前，幽灵条目不会阻止整表清空。

它不依赖任何单个事件是否正确送达或处理，而是周期性按 cache 的真实状态收敛，覆盖：

- **删除早于写入**：事件正常送达，但清理时条目尚不存在，随后被写入；
- **事件处理未命中清理路径**：`DeletePod` 类型转换失败提前返回、
  `clearUnassignedNumaTask` 找不到节点、`syncTask` 的 `parseErrTaskKey`
  找不到 task 后 `Forget` 等；
- **informer 中断/迟到**：watch 重连后的 relist、合并事件、以及 `updatePod`
  中 "allocated 且 nodeName 为空则忽略" 的守卫（`event_handlers.go:363-368`），
  都可能让某些状态变化没有进入清理路径；
- 历史版本遗留条目以及未来新增写入路径的漏洞。

### 5.4 写入时跨节点清理（未实施）

`reserveBindNumaResources` 写入前，先删除同一 UID 在其他节点上的旧条目，
可处理"N1 失败、N2 成功"的跨节点残留。当前未实施，由 5.2/5.3 兜底。

### 5.5 删除侧明确不做的事

- 不在 `Bind()` 失败循环里同步清理（锁与队列的等待环风险）；
- 不使用 pod phase 判断（in-flight pod 也是 Pending）；
- 暂不改变 `RefreshNumaSchedulerInfoByCrd` 的 all-or-nothing 语义；
- 暂不引入反向索引，先扫描 `sc.Nodes`。

## 6. 代码改动清单

| # | 文件 | 改动 | 阶段 | 状态 |
| --- | --- | --- | --- | --- |
| 1 | `pkg/scheduler/cache/cache.go` | 定义 `BindNumaReservation`；`AddBindTask` 中新增 `reserveBindNumaResources` | 写入侧 | 已完成 |
| 2 | `pkg/scheduler/plugins/numaaware/numaaware.go` | 实现 `SetupBindContextExtension`；删除 `taskBindNodeMap`/`taskPodMetaMap`、`OnSessionClose` 写入 | 写入侧 | 已完成 |
| 3 | `pkg/scheduler/cache/interface.go`、`cache.go`、`framework/session.go` | 删除 `AddUnassignedNumaPods` 相关接口与实现 | 写入侧 | 已完成 |
| 4 | `pkg/scheduler/cache/event_handlers.go` | `clearUnassignedNumaTask` 改为按 UID 全局删除；`DeletePod`/`syncTask NotFound` 适配 | 删除侧 Phase 1 | 已完成 |
| 5 | `pkg/scheduler/cache/cache.go` | `Snapshot` 中新增状态对账（`keepNumaReservation` + 节点一致性） | 删除侧 Phase 1 | 已完成 |
| 6 | `pkg/scheduler/cache/event_handlers.go` | `syncTask` 中新增按 `spec.nodeName` 对账 | 删除侧 Phase 2 | 已完成 |
| 7 | `pkg/scheduler/api/numa_info.go` | `CheckNumaPodAssigned` 改为 UID 优先，避免同名重建误判 | 加固 | 已完成 |
| 8 | 测试 | 单元测试 + `numaaware/integration_test.go` | 测试 | 已完成 |
| 9 | `pkg/scheduler/cache/cache.go` | （可选）`reserveBindNumaResources` 中清理旧节点条目 | 可选 | 未实施，由 5.2/5.3 兜底 |

## 7. 测试覆盖

### 7.1 写入侧

- `TestReserveBindNumaResources*`：扩展写入、忽略非预留扩展、空集合、无 pod、覆盖写、多 pod 累积；
- `TestAddBindTask_ReservesNumaResources`：`AddBindTask` 在入队前写入；
- `TestNuma_SetupBindContextExtension*`：只携带目标节点的决策，且为副本；
- `TestIntegration_AllocateWritesNumaReservation`：predicate → 扩展 → `AddBindTask` 全链路；
- `TestIntegration_PipelineDoesNotWriteNumaReservation`、`TestIntegration_NonGuaranteedPodDoesNotWriteNumaReservation`：不该写的场景不写。

### 7.2 删除侧

- `TestClearUnassignedNuma*`：无 `NodeName`、无 job、多节点残留都能清理；
- `TestSyncTask_ReconcilesUnassignedNumaPods`：`NotFound` / 未绑定 / 绑到同节点 / 绑到其他节点；
- `TestReconcileUnassignedNumaPod`、`TestPruneUnassignedNumaPods`：节点一致性、deny-list 状态、`Unknown` 保留；
- `TestIntegration_DeletePodClearsNumaReservation`、`TestIntegration_SnapshotPrunesByTaskState`：真实 cache + 插件链路。

### 7.3 未覆盖项

bind 失败 → resync 对账由于需要驱动 cache 私有的 `errTasks` 队列，未做端到端集成测试，由
`TestSyncTask_ReconcilesUnassignedNumaPods` 直接覆盖四种 pod 状态。

## 8. 实施顺序、风险与回滚

以下顺序已按计划执行完毕，保留作为回滚与后续维护参考。

### 8.1 实施顺序

1. **删除侧 Phase 1**（改动 4、5）：独立、低风险，直接解决当前残留问题；
2. **删除侧 Phase 2**（改动 6）：与写入侧一起做，缩短失败后的预占释放时间；
3. **写入侧**（改动 1、2、3）：语义收敛，消除越界写入和时序窗口；
4. **可选**（改动 7）：跨节点清理。

### 8.2 风险

- **漏写**是写入侧最大风险：必须确认所有 bind 都经过 `AddBindTask`
  （`BindFlowChannel` 的唯一生产者），且 `CreateBindContext` 时 `assignRes` 已就绪
  （predicate 先于 allocate）；
- 写入逻辑与新写入路径应在同一个提交内完成，避免"两边都不写"的中间态；
- 删除侧 UID 扫描为 O(nodes)/次，当前规模可接受，需关注超大规模节点数场景。

### 8.3 回滚

- 删除侧改动可独立回滚，回滚后行为回到"依赖 NodeName 清理"；
- 写入侧改动需整体回滚：恢复 `OnSessionClose` 汇总写入，移除 `AddBindTask`
  中的写入调用，避免出现"两边都写"的重复。

## 9. 附：线上排查与临时规避

1. V(3) 日志搜 `reserved resources .* before bind` 与 `deleted unassigned pod` /
   `released unassigned pod` / `pruned unassigned numa pod`，对比同一 `PodMeta`
   是否"有加无删"；
2. 搜 `because of the presence of unassigned`，观察数字是否长期不降；
3. 用日志中的 pod 名/UID 查 apiserver：已 `NotFound`，且历史中 `spec.nodeName`
   始终为空或绑定失败 → 基本可确诊；
4. 临时规避：重启 scheduler（内存态清空）或删除并重新添加对应节点；
5. 长期修复：已按本方案实施。

## 10. 相关文档

- [UnassignedNumaPods 字段维护分析](./numa-unassigned-pods-maintenance-analysis.md)
- [UnassignedNumaPods 写入与删除现状分析](./numa-unassigned-pods-write-delete-analysis.md)
