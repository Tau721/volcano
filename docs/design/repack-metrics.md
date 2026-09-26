# Volcano Repack 指标

## 1. 文档定位

本文记录 Repack 的指标现状与本期计划：**第 3 节是已实现事实**，以 `pkg/repackengine/metrics/metrics.go` 为唯一事实来源；**第 5 节是本期新增的指标**（定义与发射已落地在 `metrics/metrics.go`，指标面由 `metrics/metrics_test.go` 锁定；正文仍按"要新增"的口气写，便于回看当时的取舍）；暂不实现但已评估过的候选集中在第 7 节，避免重复讨论。

相关文档：

- 指标口径的设计契约见 [Repack 技术设计](./repack-design.md) §12.2 Events 与 Metrics；
- 其余组件的指标清单见 [Volcano Metrics](./metrics.md)（本文补齐其中缺失的 repack 一节）；
- 用户视角的开启方式与 CR 字段见 [Repack 用户指南](../user-guide/how_to_use_repack.md)。

## 2. 命名与暴露位置

- 前缀固定为 `volcano_repack_`：`promauto` 注册，`Subsystem = "volcano_repack"`，全部落在默认 registry。
- **指标分属两个进程，两个 `/metrics` endpoint**：

| 进程 | 代码位置 | endpoint | 当前 repack 指标 |
|---|---|---|---|
| vc-repack-engine | `pkg/repackengine/` | `opt.EnableMetrics` + `opt.ListenAddress`，`cmd/volcano-repack-engine/app/server.go:76` | 第 3 节 9 个 + 第 5 节 6 个，共 15 个 |
| vc-controller-manager | `pkg/controllers/repack/`（policy 触发、nomination、placement 收敛） | `cmd/controller-manager/app/server.go:59-62` | **无** |

两者的 `--enable-metrics` 都默认关闭，需显式开启；抓取两个 endpoint 才能看到完整的 repack 链路，只看 engine 会漏掉「谁产生了 Run」和「替身落到了哪里」。第 5 节的新增指标全部落在 engine 侧，无需改动 controller-manager。

- 标签必须是封闭有界词表（`mode`、`outcome`、`result`、`reason`、`resource`）。**禁止**把 Run 名、Pod 名、PodGroup 名、节点名挂成标签——这些明细由 CR `status` 承载。

## 3. 已实现指标

| 指标 | 类型 | 标签 | 含义（一句话） | 发射点 |
|---|---|---|---|---|
| `volcano_repack_runs_total` | Counter | `mode`=DryRun/Execute，`outcome` | 累计**收尾**的 RepackRun 数，按模式与终态原因拆分 | `internal/engine/status_persistence.go:124` |
| `volcano_repack_evictions_total` | Counter | `resource`，`result`=evicted/rejected/indirectly_removed | Execute 计划要移走的 Pod 各自的下场 | `internal/engine/eviction_reconcile.go:461-462` |
| `volcano_repack_eviction_retry_batches_total` | Counter | — | 驱逐"还没有结论"的轮次累计数 | `internal/engine/eviction_reconcile.go:238` |
| `volcano_repack_eviction_retry_pods_total` | Counter | — | 上述每一轮里卡住的 Pod 数之和 | 同上 |
| `volcano_repack_cycle_duration_seconds` | Histogram | `mode` | 引擎单次 reconcile 的墙钟时间，`prometheus.DefBuckets` | `actions/repack/repack.go:77` |
| `volcano_repack_gate_rejections_total` | Counter | `reason`=AnotherRunActive/ExecuteCooldownActive | Execute 串行门推迟 Run 的次数 | `internal/engine/reconcile.go:197` |
| `volcano_repack_planner_candidates_evaluated` | Histogram | `mode` | 一次规划 pass 评估过的候选数（便宜路径）；桶 `1…25000` | `planner/drain/drain.go:142` |
| `volcano_repack_planner_feasibility_simulations` | Histogram | `mode` | 一次规划 pass 进入调度器保真模拟的候选数（贵路径）；桶 `0…1000` | 同上 |
| `volcano_repack_planner_candidates_pruned_total` | Counter | `mode`，`reason` | 规划各阶段淘汰的候选数，按原因拆分 | 同上 |

### 3.1 逐条说明

**`volcano_repack_runs_total{mode, outcome}`** — 有多少 RepackRun 走到了终态，以及各自以什么结论结束。`mode` 区分 DryRun（只模拟）与 Execute（真驱逐）；`outcome` 是 Complete/Failed condition 的 reason，即"为什么结束"。读法：`sum by (outcome)` 看失败构成，是判断整理健康度的第一入口。注意：**只统计终态**，Pending/Running 中的 Run 不在内，不能用它数"当前有几个 Run 在跑"；`outcome="Unknown"` 表示没匹配到 condition，出现即终态写回有缺陷。

**`volcano_repack_evictions_total{resource, result}`** — Execute 期间"计划要被移走的 Pod"最终各自的下场，三种结果互斥且穷尽：

- `evicted`：本 Run 成功驱逐。含"驱逐意图已持久化、随后发现该 Pod 已消失"的情形，按成功处理（`ErrVictimNotFound`）。
- `indirectly_removed`：同 PodGroup 内其它驱逐被接受后该 Pod 自行消失，本 Run 没收到它自己的接受响应。
- `rejected`：本 Run 确定没移走它——非重试性失败，**以及执行截止时仍无结论的受害者**（终态归并：截断时把还停在 `Pending`/`InProgress` 的一律记为 rejected，`execution_timeout.go:83`）。另有 `eviction_reconcile.go:277` 的一处补数：被拒的 relocation 在耐久屏障之后会被丢弃，若此后的 placement 写回被重试，则从不可变的 plan 里把差数补回 `rejected`。

读法：三者之和 = 计划受害者总量；只看 `evicted` 会低估实际干扰面。`resource` 是**该 Run 的目标资源**（`goals[0].resource`，省略时取配置默认值），不是被驱逐 Pod 自己的资源——它回答"这次整理为的是哪种卡"。注意：**PDB 阻塞不落在 `rejected`**。Eviction API 的 PDB 拒绝返回 429，被归为可重试（`isRetryableEvictionError`），受害者相位停在 `InProgress` 并走重试批次；只有拖到截止仍未成，才在终态折入 `rejected`。零值不发射。

**`volcano_repack_eviction_retry_batches_total`** — 驱逐"仍在进行中"的轮次累计数：每次 reconcile 结束时若还有受害者停在 `Pending`/`InProgress`，就记一轮。读法：rate 抬升说明驱逐在反复被瞬时因素挡住，PDB 是最常见的那个。注意：口径是**轮次**，一次 Execute 会经历多轮 reconcile，所以单个 Run 可能贡献多个批次。

**`volcano_repack_eviction_retry_pods_total`** — 上述每一轮里"还没有结论"的受害者 Pod 数之和。读法：与上一条相除 = 平均每轮卡住几个 Pod，区分"一个 Pod 反复卡"和"一次卡一片"。注意：**同一 Pod 跨多轮被重复计入**，它不是"被阻塞过的不同 Pod 数"，衡量的是重试压力而非受影响规模——规模看 `evictions_total`。

**`volcano_repack_cycle_duration_seconds{mode}`** — 引擎**单次 reconcile**（规划 + 本次要做的驱逐）的墙钟时间。读法：配 `planner_*` 三条判断慢在搜索还是慢在驱逐。注意：是"每轮"不是"每 Run"——一个 Execute Run 会经历多轮 reconcile（驱逐重试、替身收敛都要重新排队）；也**不含**纯规划耗时（规划与执行未拆）；单 Run 从开跑到终态的总时长目前没有指标，属第 7 节候选。

**`volcano_repack_gate_rejections_total{reason}`** — Execute 串行门把 Run **推迟**的次数。`AnotherRunActive`=已有 Execute 在跑，`ExecuteCooldownActive`=上一个刚结束还在冷却窗口。读法：判断 K=1 串行是否成为整理吞吐的瓶颈（需与"当前排队数"配对才有结论，该指标目前不存在）。注意：是"推迟"不是"拒绝"——被挡的 Run 会按 `RequeueAfter` 重试，不算失败；DryRun 不参与串行、不计入；没有反向信号，看不出"此刻槽位是否被占用"。

**`volcano_repack_planner_candidates_evaluated{mode}`** — 一次规划 pass 中评估过的 drain 候选单元总数（跨该 pass 所有 step 累加）。它度量的是**便宜的搜索宽度**，与昂贵的可行性模拟、墙钟时间解耦。读法：它涨而模拟数不涨，说明预过滤在有效工作。注意：单位是"候选单元"不是"节点数"。

**`volcano_repack_planner_feasibility_simulations{mode}`** — 一次规划 pass 中真正进入**调度器保真可行性模拟**（贵路径）的候选数。读法：与上一条相除 = 昂贵模拟的触发率，是规划成本的主要解释变量；该比值飙升而最终没有更多 move，说明预过滤失效、模拟在被浪费。注意：桶含 0（一个都没进模拟是正常且值得看见的结果）。

**`volcano_repack_planner_candidates_pruned_total{mode, reason}`** — 规划各阶段淘汰掉的候选累计数，按原因拆分（词表见 3.4）。读法：某个 reason 占比突变通常对应配置或集群状态变化——`max_resource`/`max_pod_groups` 抬头说明 `maxPerRun` 成了瓶颈，`insufficient_receiver_resource` 抬头说明接收侧装不下。注意：它是**反事实**信号，只说"什么被扔了"，不说"扔了是否可惜"。

后三条共同构成一次规划的**搜索漏斗**：`evaluated` → `simulations` → （接受的 move 数，**目前无指标**，见第 7 节）。前两条按 pass 观测、第三条按原因累计，三者 label 不同但都带 `mode`，可交叉定位。

### 3.2 四条读图约定

1. 上表全是 Counter/Histogram，**没有任何 Gauge**。集群「当前有多碎」只能去读某个 RepackRun 的 CR `status`——没有 Run 时该量在指标里不存在。第 5 节新增的六条沿用这一点，本期不引入 Gauge（理由见第 7 节）。
2. **零值不发射只适用于 Counter**：`ObserveEvictions`、`ObserveIndirectRemovals`、`ObserveEvictionRetryBatch`，连 `ObservePlanner` 里的裁剪计数与 5.1 的 `addMovedCards`，都做了 `<= 0` 早退，所以这些 Counter 序列不会出现零值样本，`rate()` 上不会看到"归零"的假信号。**别把这条推给直方图**——`ObservePlanner` 的两条直方图与 `ObserveCycle` 都**无条件 observe**，0 是它们的正常样本（"一个候选都没进可行性模拟"正是值得看见的结果，见 3.1）；第 5 节的五条直方图同样刻意 observe 0，理由见 5.6。
3. **Counter 只给累计总量，Histogram 给分布（不给逐次取值）。** 前者的样本被永久压缩成一个数；后者暴露 `_sum`（总量）、`_count`（样本数）与各量级区间的 `_bucket`，但同桶内的个体不可区分，且桶在注册时固定、事后无法细分。要单个 Run 的精确值请读 CR `status.result`。**注意别把这条读成"直方图连总量都有损"**——`_sum`/`_count` 是精确的，只有分位数是插值估计，详见 5.5。
4. **带标签的指标在首次观测前完全不存在**，连 `# HELP`/`# TYPE` 行都没有（`MetricVec.Collect` 委托给 `metricMap.Collect`，而后者遍历的是尚未装入任何子项的 map，一条样本都产出不了，`vendor/github.com/prometheus/client_golang/prometheus/vec.go:331`）。所以 `curl .../metrics | grep runs_total` 在集群还没跑过 Run 时**什么都搜不到，这是正常的**，不代表指标没注册；反例是不带标签的 `eviction_retry_batches_total`，它注册即上报 0。排查"指标丢了"时先确认是否有过对应的事件，再怀疑代码。

### 3.3 `outcome` 词表

取自 Complete/Failed condition 的 `reason`（`pkg/controllers/repack/state/state.go:57-75`，由 `status/projection.go:40 TerminalOutcome` 投影）。未匹配到 condition 时为 `Unknown`——**出现 `Unknown` 即表示终态写回路径有缺陷，值得单独告警**。

成功侧：`RepackRecommended`（DryRun 建议整理）、`ExecutionCompleted`、`ExecutionCompletedWithAlternativePlacement`（替身被调度到非计划节点）。
无收益侧：`NoFragmentation`、`InsufficientImprovement`、`RequiredNodeBlocksNotMet`。
失败侧：`InvalidConfiguration`、`ScopeResolutionFailed`、`ExecutionPreparationFailed`、`EvictionFailed`、`ExecutionTimedOut`、`PlacementTimedOut`、`ResultVerificationFailed`、`BenefitNotRealized`、`ExecutionInterrupted`、`ReconcileFailed`。

### 3.4 `planner_candidates_pruned_total` 的 `reason` 词表

由规划器内部产生（`planner/drain/drain.go`）：`source_node_not_partially_occupied`（:247）、`insufficient_receiver_resource`（:318、:368）、`scheduler_infeasible`（:383）；另有插件经 `CandidateFilterResult` 注入的 `max_pod_groups`、`max_resource`（`plugins/repackbudget/repack_budget.go:45,48`）。新增插件裁剪原因会直接进入该标签，需要同步维护词表。

## 4. 收益类数据的现状与缺口

[Repack 技术设计](./repack-design.md) §12.2 要求指标至少覆盖：*Run 数量、终态原因、规划耗时、候选裁剪原因、迁移量、实际释放节点、placement 漂移*。逐项核对，并标注本期取舍：

| 契约项 | 状态 | 本期 |
|---|---|---|
| Run 数量 | ✅ `runs_total` | — |
| 终态原因 | ✅ `runs_total{outcome}` | — |
| 候选裁剪原因 | ✅ `planner_candidates_pruned_total{reason}` | — |
| 规划耗时 | ⚠️ 只有 plan+evict 合成的 `cycle_duration_seconds` | 暂不 |
| 迁移量 | ❌ `result.movedCardCount`（卡）与 `status.relocations`（Pod）已算出，只写 CR | **本期**（5.1 卡数 Counter / 5.2 Pod 数直方图 ×3） |
| 碎片率改善 | ❌ `plan.summary.fragBeforePercent - result.fragAfterPercent` 已算出，只写 CR | **本期**（5.3） |
| 实际释放节点 | ❌ `result.freedNodeCount` / `freedNodes` 已算出，只写 CR | **本期**（5.4，与迁移量同源同发射点） |
| 当前碎片率水平 | ❌ 引擎不测量，controller 侧只在配了 `onFragAbovePercent` 时算 | 不做（第 7 节，需 engine 新增周期测量） |
| placement 漂移 | ❌ `PlacementOutcomeCounts`（`status/message.go:262`，区分 selected/alternative/timed_out）已算出，只在 `internal/engine/placement_result.go:97-103` 打日志 | 暂不 |

关键事实：**5.1–5.4 要加的量全部已经算好**，每个终态 Execute Run 都会写进 CR `status.result`，缺口纯粹在发射环节，不涉及新的计算逻辑——因此本期改动可以集中在「加定义 + 加一次发射调用」，不动事件循环。表里唯一做不到这一点的是"当前碎片率水平"，它是一条引擎根本不测量的水平值，本期不做，理由见第 7 节。

## 5. 本期要新增的指标

命名沿用第 2 节约定：六条指标（5.2 是同一量纲的三条：一条合计、两条分量）带同一个 `resource` 标签、在 Run 终态的同一个发射点写出，其中五条是 Histogram（每 Run 观测一次），5.1 是 Counter（累计总量）。

| # | 指标 | 类型 | 桶 |
|---|---|---|---|
| 5.1 | `volcano_repack_moved_cards_total` | Counter | — |
| 5.2 | `volcano_repack_affected_pods` / `volcano_repack_evicted_pods` / `volcano_repack_indirectly_removed_pods` | Histogram ×3 | 各 25 桶（含 0） |
| 5.3 | `volcano_repack_fragmentation_improvement_percent` | Histogram | 24 桶（含负侧） |
| 5.4 | `volcano_repack_freed_nodes` | Histogram | 17 桶（含 0） |

### 5.1 `volcano_repack_moved_cards_total`

| 项 | 内容 |
|---|---|
| 类型 | Counter |
| 标签 | `resource`（目标加速资源，`goals[0].resource`，省略时取配置默认值，`conf.ResolveResource`） |
| 语义 | **所有终态 Execute Run 累计实际移动的加速卡数**，每 Run `Add` 一次 |
| 取值 | `status.result.movedCardCount`（**整卡**，`api.Cards` 内部已除以 1000，与 milli 内部单位区分） |
| 发射条件 | `Result != nil`（**不要求** `metricsVerified`）且值 `> 0` |

口径注意：只统计 `PodEvictionAccepted` 的 Pod（`initializeExecuteResultFromStatus` 只把 Accepted 的 relocation 收进计数集合，`internal/engine/eviction_journal.go:144`）；`IndirectlyRemoved` 的 Pod 不是本 Run 逐张移走的，其卡数不计入。因此准确读法是"被接受的驱逐所移动的卡"，不是"换了节点的卡"。

**为什么是 Counter 而不是 Histogram**：本条要回答的问题是"一共移动了多少"，而这个量**两种类型都给得准**——Counter 的值就是它，直方图的 `_sum` 也精确（超出最高桶的观测照样累加进 `_sum`，只是分布上归入 `+Inf`）。所以选 Counter 不是因为总量更准，而是这条**没有一个说得出口的"每 Run 形状"问题**：Histogram 会为此多出每条 `resource` 二十多条序列，以及一套要长期维护、且 Run 规模变化后就会失真的桶阶梯。5.2–5.4 都各自有形状问题（分因扰动的长尾、改善量的正负、收益门槛的分布，见 5.5），这正是它们与本条的区别。零值早退（`addMovedCards` 的 `cards <= 0`）沿用既有 Counter 的惯例，避免给"一张卡都没动"的资源凭空建出零值序列。

代价要写清楚：**每 Run 的卡数分布没了**（`volcano_repack_moved_cards_total` 没有 `_count` 这一说，Counter 只有值本身）。要单次 Run 的卡数读 CR `status.result.movedCardCount`（见 5.5）；要长尾分布，5.2 的三条承载了同源的分因分布（见 5.2）。

还有一个零值早退的直接后果：**"什么都没做的 Execute" 只进另一头，不进这一条**。规划下来没有值得做的动作时，Execute 走 `InitializeNoopExecuteResult`（`actions/repack/repack.go:132`）收尾——`metricsVerified=true`、卡数 0。此时 5.1 因 `cards <= 0` 不发射，而 5.2 的三条会各如实 observe 一个 0。于是两边的"样本数"含义不同：5.2 三条的 `_count` = **留下 `status.result` 的**终态 Execute Run 数（`MarkExecuteNotPerformed` 把 `Result` 置空的那些——驱逐屏障之前的准备失败、lease 丢失——同样不计入），5.1 的增量次数 = 其中真的移动过卡的 Run 数。**别把这两个数当同一个分母**（5.2 已有的恒等式也不受影响，它比的是三条 `_sum` 与 `evictions_total`，与 5.1 无关）。

### 5.2 `volcano_repack_affected_pods` / `volcano_repack_evicted_pods` / `volcano_repack_indirectly_removed_pods`

同一个量（本次整理真的动过的 Pod 数）发射三条：一条**合计**、两条按成因拆分的**分量**。三条的桶、标签、发射点、`_count` 分母全部相同，只有取值不同，都是每 Run observe 一次。

| 项 | `volcano_repack_affected_pods` | `volcano_repack_evicted_pods` | `volcano_repack_indirectly_removed_pods` |
|---|---|---|---|
| 类型 | Histogram | Histogram | Histogram |
| 标签 | `resource` | `resource` | `resource` |
| 语义 | 本 Run **真的扰动过**的 Pod 数（两种成因之和） | 本 Run **自己发出驱逐请求、并被接受**的 Pod 数 | 本 Run **没有对它发请求**、但因同 PodGroup 内兄弟被接受而自己消失的 Pod 数 |
| 取值 | `accepted + IndirectlyRemoved` | `eviction.phase == Accepted` 的条数 | 其中 `== IndirectlyRemoved` 的条数 |
| 桶 | `0,1,2,3,4,5,6,7,8,12,16,24,32,48,64,96,128,192,256,384,512,768,1024,1536,2048`（25 显式桶 = 每条序列 28 条：25 + 隐式 `+Inf` + `_sum` + `_count`） | 同左 | 同左 |
| 发射条件 | `Result != nil`（不要求 `metricsVerified`） | 同左 | 同左 |

**为什么是三条独立指标，而不是一条带 `result` 标签**：把"合计"当成同一个族的第三个标签值（`result=all`）会让任何忘记过滤的聚合**静默翻倍**——`sum(rate(..._sum[1h]))` 不做过滤会拿到 `all + evicted + indirectly_removed` = 2× 真值，Prometheus 不会报错。三条独立族没有这个陷阱：跨族相加只在**总量层面**（`_sum`）才有意义，而在分布层面根本不是加出来的（见下）。

**为什么合计要单独占一条，而不是让分量去还原它**：分量**相加不能复原合计**。`sum by (le)(evicted_pods_bucket) + sum by (le)(indirectly_removed_pods_bucket)` 得到的是**把两批观测汇成一池**的分布（`_count` 还会翻倍），不是"每 Run 两个分量之和"的分布；而两个分量强相关（踢得多、兄弟消失得也多），偏差是系统性的、不是噪声。所以"**一次整理最多同时踢掉多少业务 Pod**"——维修窗口要的那个上界——**只能由合计那条回答**：`histogram_quantile(0.95, sum by (le)(rate(affected_pods_bucket[1d])))`。用 `p95(evicted) + p95(indirect)` 去粗略替代是把两个不同 Run 的分位数拼在一起，说不清是估计。

代价是**同一批事件写进三条族**：合计与两条分量在 `_sum` 上恒等（见下），是刻意的冗余，换来的是"合计形状"与"分量形状"各自可读。这也是为什么**不要写 `sum(rate(volcano_repack_.*_pods_sum[...]))`** 这类通配聚合——它会把三条一起加上，得到 2× 真值；PromQL 不会拦你。

桶的分段理由：0–8 逐点，一个节点的 Pod 数通常就在这个量级（8 也是"主流 8 卡/节点"的整机量）；12–64 覆盖"腾空 1~8 个节点的整机量"；192 以上按约 ×1.5 阶梯到 2048。**上界取 2048 而非 CRD 理论上限**（`relocations` 上限 4096），因为单次扰动两千个 Pod 已属极端，此时只关心"很大"而非"多大"；超出者落入隐式 `+Inf`，`_count`/`_sum` 仍精确。

**第一个边界是 `0`**（与 5.4 同一条约定）：三条都刻意 observe 0（见 5.6，0 是真实观测而非缺失）。对 `indirectly_removed_pods` 尤其关键——**0 是它的常态**，"这一趟有没有牵连到别人"与"牵连了 1 个"在 `le="0"` 这一格上分开，纯牵连的整理次数才算得出来（见下）。对 `affected_pods` 与 `evicted_pods` 则分开了两条"没动 Pod"的路径：(a) 规划下来无事可做（`InitializeNoopExecuteResult`，`actions/repack/repack.go:132`），(b) 计划了驱逐但**全部被拒**（`accepted=0`，`eviction_reconcile.go:284` 先建 `result`，`:294` 才标 unverified）。若桶从 `1` 起，"试过、没动成"与"本来就没活"就读不出来了。

与既有指标的关系：**累计口径已被覆盖，且按族恒等**——三条取自同一份 `status.relocations`（`disruptedPods` 按 `eviction.phase` 计数），同一批事件、每 Run 各发射一次（驱逐阶段收尾处早返回的重试批次走不到发射点，因此不会重复计数），且合计在代码里由两个分量相加得出（`observeDisruptions`），不是调用方另外传进来的数，所以

```
sum(affected_pods_sum)  ≡  sum(evicted_pods_sum) + sum(indirectly_removed_pods_sum)
evicted_pods_sum             ≡  evictions_total{result="evicted"}              （逐 resource 相等）
indirectly_removed_pods_sum  ≡  evictions_total{result="indirectly_removed"}   （逐 resource 相等）
```

两边算出来相等是**正常的**，别当成"指标算错了"或"某一边丢数"；`evictions_total` 带了 `resource`（见 3.1）之后，后两条是逐序列相等，不需要聚合。

不过后两条的相等**只对终态成立，而且靠约定、不靠构造**：

- **发射时刻不同**。三条直方图在 `status.result` 落盘的终态写里发射（`status_persistence.go` 的 `updateStatusTerminal`），`evictions_total` 在更早的驱逐屏障处就发射了（`eviction_reconcile.go` 收尾，移交 placement 之前）。所以对一趟还在 placement 里跑的 Run，`evictions_total` 会**合理地领先**——直方图那一侧还没有样本。别拿运行中的查询结果当"漏数"。
- **两个独立读取器**。引擎侧的 `summarizeEvictions`（事件文案也要这份计数）与指标侧的 `disruptedPods` 各自按 `eviction.phase` 数同一份 `status.relocations`。相等只是因为它俩当下口径相同，改一边不会自动改另一边；`metrics_emission_test.go` 里有一条专门盯这件事的用例。
- 因此累计差额有一条可读的含义：某 Run 若在屏障之后、终态写之前消失（进程重启、placement 阶段崩溃），它的扰动会留在 `evictions_total` 里而不进直方图——两侧累计之差就是这类"跑了一半掉队"的 Run 的 Pod 数，不是算错。

第一行（合计 = 两条分量之和）没有这个问题：同一个 `observeDisruptions` 在同一瞬间把两条相加，是构造保证，任何时刻都成立。这三条不可替代的价值是三件事：**(1) 每 Run 形状**——Counter 只给累计值，给不出"单次整理最多扰动多少 Pod"这个上界（只有合计那条给得出），也给不出"单次直接驱逐的 p95"或"单趟爆发式牵连的尾巴"；**(2) `_count` 差出来的纯牵连次数**——`indirectly_removed_pods` 的 `_count` 减去 `evicted_pods` 的 `_count`，就是"这一趟自己一个都没踢、却有人因为兄弟走了而消失"的整理次数，这个数只有分量计数才给得出；**(3) 每 Run 的分母**——`_count` 是"留下 `status.result` 的终态 Execute Run 数"，Counter 数的是 Pod 不是 Run，给不出这个数（见 5.1）。

`rejected` **不属于这三条**：被拒的 victim **原地没动**，不是"被扰动"的 Pod，塞进来三条的名字都假了（它会让"合计"变成"计划量"）。它是"计划未落地"的机制信号，归 `evictions_total{result="rejected"}`（按 resource 可归因，累计口径已够用）；它**仍然没有分布**，作为候选记在第 7 节。

### 5.3 `volcano_repack_fragmentation_improvement_percent`

| 项 | 内容 |
|---|---|
| 类型 | Histogram |
| 标签 | `resource` |
| 语义 | **每个终态 Execute Run 的碎片率实际改善量（百分点）**，每 Run observe 一次 |
| 取值 | `status.plan.summary.fragBeforePercent` − `status.result.fragAfterPercent`（int32 百分点，可负） |
| 桶 | `-100,-60,-40,-20,-10,-5,-2,-1,0,1,2,3,4,5,6,8,10,15,20,30,40,60,80,100`（24 桶） |
| 发射条件 | `Result != nil` **且** `metricsVerified == true` |

四个必须遵守的口径：

1. **两个 `fragAfterPercent` 别搞混**：`plan.summary.fragAfterPercent` 是**规划时的预测值**，本节要用的是 `result.fragAfterPercent`（**实测**）；`plan.summary.fragBeforePercent` 才是可用的"前"值。所以算式读作"实测前 − 实测后"，而 `plan.summary.fragAfterPercent` 在本节**不参与计算**（它只在 DryRun 建议里被读）。
2. **只在 `metricsVerified == true` 时发射。** 未验证时 `result.fragAfterPercent` 被赋值为 `plan.summary.fragBeforePercent`（`MarkBenefitUnverified`，`executor/placement/decision.go:195`），算出来的改善**恒为 0**。无条件发射就是往直方图里灌伪造样本，把"没验证"混成"确实没改善"。被跳过的 Run 通过 `runs_total{outcome}` 侧观察（`PlacementTimedOut`、`BenefitNotRealized`、`ResultVerificationFailed` 等）。
3. **桶必须含负侧与 0。** 改善可能为负（实际因替身落点漂移而回退），0 是"跑了但没改善"这一有意义的结果。负侧与正侧**对称**（-100…-1 ／ 1…100），以便区分"轻微回退 -1（可能只是快照抖动）"与"严重回退 -40"。
4. **0 附近逐点加密**（1–6）。门槛判定（`MinFragImprovementPercent`）就发生在个位数百分点，"改善 1 个点"和"改善 2 个点"是定性不同的结果。

### 5.4 `volcano_repack_freed_nodes`

| 项 | 内容 |
|---|---|
| 类型 | Histogram |
| 标签 | `resource` |
| 语义 | **每个终态 Execute Run 实际腾空的节点数**，每 Run observe 一次 |
| 取值 | `status.result.freedNodeCount` |
| 桶 | `0,1,2,3,4,5,6,7,8,12,16,24,32,48,64,96,128`（17 桶，**含 0**） |
| 发射条件 | `Result != nil` **且** `metricsVerified == true` |

与 5.3 同理：未验证时 `MarkBenefitUnverified` 把它**置 0**，无条件发射会把"没测出来"记成"一个节点都没腾空"。

桶用与 5.2 相同的阶梯形状但**上界低一档**（128 而非 2048）：低段逐点对应个位数腾空量（收益门槛默认就是 1 个节点），腾空 128 个节点已属大范围整理。与卡数可对读（8 节点 ≈ 64 卡）。

**第一个边界是 `0`，与 5.2 同一条约定**：`0` 是一个真实且常见的已验证结果（计划腾空的节点在执行后仍被占用，`placement_result.go:182-184` 先测量再无条件置 `metricsVerified=true`），而收益门槛恰好就卡在 `0` 与 `1` 之间——`le="1"` 里若同时装着 0 和 1，"门槛期望 ≥1、实际拿到 0"这一格就永远读不出来，而那正是校准门槛要的那一格。`le="0"` 即"验证过但一个节点都没腾空"，`_count - le="0"` 即"真的腾空了 ≥1 个"。

已知口径差（读图时别当等式）：门槛比对的是 `plan.Benefit()`（**加权**收益，只有在没有任何 `FreedUnits` 时才退化成节点数，`api/plan.go:69-81`），而本条观测的是 `result.freedNodeCount`（**整节点数**）。所以本条给的是"门槛期望的事后分布"，不是门槛输入的直读。

### 5.5 直方图的精确与有损

五条直方图里，"总量"和"分布"的精度**不一样**，这是最容易被误读的地方：

| 你想问的 | 精度 |
|---|---|
| 累计总量（这个月一共扰动了多少 Pod） | `increase(..._sum[30d])` → **精确** |
| 平均每次（`_sum / _count`） | **精确** |
| 有多少次 Run 贡献了样本（`_count`） | **精确** |
| 典型的一次是多少（p50 / p95） | 桶内线性插值**估计** |
| 某一次 Run 的确切值 | 直方图**做不到** |

`_sum` 精确是有依据的：`Observe(v)` 的实现是 `atomicAddFloat(&hc.sumBits, v)`（`histogram.go:659`），直接累加原始值，没有任何桶量化。只有分位数是有损的——`histogram_quantile()` 假设桶内均匀分布做线性插值，p50 落在 `le="4"` 桶里时可能返回 3.4，而真值是整数。

`moved_cards_total` 不在这张表里：它只回答"累计总量"，而 Counter 的值本身就是精确的。代价是表里"平均每次"与"典型的一次"两行它给不出——那正是 5.1 主动放弃的分布。

**而这几条的量全是整数**（Pod 数、节点数、百分点都是 int）。因此只要桶边界取**连续整数**，落在该区间内的观测就是**完全精确**的——整数不可能落在 4 和 5 之间。所谓"有损"只发生在刻意设成粗粒度的长尾区间，是**桶设计的选择，不是 Histogram 的固有属性**。这也解释了为什么上面几条要在低段逐点设桶：不是在浪费桶，而是在买精度。

**"直方图相加"恢复不出"每 Run 的合计"** —— 这是本组指标最容易被"优化"掉的一处，值得单独钉住。把某个每 Run 总量拆成几个直方图（做成几个 label 值、或几个族，对 PromQL 的桶相加是一回事）之后，`sum by (le) (a_bucket) + sum by (le) (b_bucket)` 给出的是**把两批观测汇成一池**的分布，**不是**"每 Run 两个分量之和"的分布。反例：A = (8 直接驱逐, 2 间接消失)、B = (2, 8)，两次真实合计都是 10；桶相加读出来却是"两次都 ≤ 8"（池成了 `{2,2,8,8}`），且 `_count` 从 2 翻成 4。根因是**配对关系在相加时被丢弃**，而两个分量通常强相关（驱逐多的 Run 间接消失也多），所以偏差是系统性的、不是噪声。

推论：**要"每 Run 合计"的形状，就必须让合计自己进直方图**——分量再怎么加也换不回来。当前设计正是把两者**各发一条**：合计 `affected_pods`（记 `Accepted + IndirectlyRemoved`），分量 `evicted_pods` / `indirectly_removed_pods`，三条在 `_sum` 层面自洽且由代码保证（合计在 `observeDisruptions` 里由分量相加得出），可校验：

```
sum(affected_pods_sum)  ==  sum(evicted_pods_sum) + sum(indirectly_removed_pods_sum)
                        ==  sum(evictions_total{result="evicted"} + evictions_total{result="indirectly_removed"})
```

**注意这条恒等式只在 `_sum`/累计层面成立**，桶层面不成立（上式第一行右边的桶相加会重复计数，见上）。别把等式读成"三条可以随意互推"。

`rejected` 不属于 `affected_pods`：被拒绝的 victim **原地没动**，不是"被扰动"的 Pod，塞进来名字就假了。它是"计划未落地"的机制信号，归 `evictions_total{result="rejected"}`（现已带 `resource`，按池可归因）。同理，`moved_cards_total` 只统计 `Accepted`（见 5.1），用它除以 `affected_pods_sum` 当"每 Pod 几卡"会因 population 不一致而偏小（见 5.2）。

真取不到的只有两件事，且都不该由指标承担：**单次 Run 的精确值**去读 CR `status.result`（含日志 `internal/engine/placement_result.go:186-193`）；**对单次 Run 精确值的告警**（如"任何一次整理移动超过 500 张卡就报警"）应在引擎里判断并发 Event（现成的 `recordRunEvent` 即可），因为指标做的是聚合与趋势，不做逐次判定。硬要用指标实现就得挂 `run` 标签，回到基数爆炸那条路。

### 5.6 共同实现约定

- **六条指标的单一发射点**：`internal/engine/status_persistence.go:125`，紧邻现有 `metrics.ObserveRun(...)` 的一次 `metrics.ObserveRunBenefit(run, e.config.DefaultResource)`。此处 `run.Status.Plan` / `run.Status.Result` 均已就绪，且保证每个终态 Run 恰好走一次。读取 Run 状态、决定"发不发、发哪些"的分派逻辑全在 `metrics.ObserveRunBenefit` 内（`metrics/metrics.go`）：先发 `addMovedCards` + `observeDisruptions`（卡数、受扰动的 Pod 数——合计与两条分量由这一个函数一次写完，合计在函数内由分量相加得出），验证通过后再发 `observeRunVerifiedBenefit`（腾空节点数、碎片改善）。六条共用这一个发射点，这也是它们唯一的写入者；引擎侧只保留"何时调用"。
- **"零值要发"只适用于五条直方图**：五条直方图**不做**现有 Counter 那样的零值早退——`ObserveEvictions` 那类 `<= 0` 早退是为了避免 Counter 被 `Add(0)` 凭空创建零值序列，而 `Observe(0)` 是真实观测（"跑了但一个 Pod 都没扰动"），恰恰最该被看见。**"被看见"要靠桶边界兑现**：5.2 三条与 5.4 的第一道边界都是 `0`，否则那个 0 会与 1 共处 `le="1"`，意图就又落空了。`moved_cards_total` 是 Counter，**遵循**惯例在 `cards <= 0` 时早退（见 5.1）。挡掉"根本没执行"的 Run 一律靠 `Result != nil` 条件，不是靠零值判断。
- **DryRun 不发射**：DryRun 没有 `status.result`，这六条全部是"实际"口径。计划收益（`plan.summary`）如果也要观测，需另立带 `phase=planned` 标签的一组，本期不做。
- **不加 `mode` 标签**：实际收益只可能由 Execute 产生，`mode` 恒为 Execute，加了是纯基数浪费。
- **"读 Run 状态得出观测值"这层映射与指标定义同在 `metrics/metrics.go`**，与其余 `Observe*` 是同一类函数（只是入参从标量换成 `*RepackRun`）——否则会出现"改了定义忘了改发射"。解析 goal、判断是否为 DryRun/未验证、按相位计数全在这里完成，引擎侧只剩"何时调用这一行"（`status_persistence.go:125`）。
- **不用 Run 名做标签**：单 Run 明细看 CR `status.result`，指标只承载分布与总量。Run 名标签的基数与 Run 数同阶，会压垮 Prometheus。
- **`_percent` 后缀是有意的偏离**：Prometheus 惯例倾向 0–1 比例，但 repack 的整个配置面都按百分点表达（`MinFragImprovementPercent`、`onFragAbovePercent`、`FragBeforePercent`），用百分点可免去 `/100` 心算并能与 CR 字段直接对读。勿当笔误改掉。

## 6. 落地注意

1. **进程归属**：六条全部落在 vc-repack-engine 的默认 registry，不新增 endpoint；engine 的 `--enable-metrics` 默认关闭，交付文档需写明开启方式（Helm 模板已带 `--enable-metrics=true --listen-address=:8081`）。
2. **基数守护**：`resource` 是一组有限的扩展资源名，可接受；Run/Pod/PodGroup/节点名一律不挂。六条指标都是每 `resource` 一条序列族，总量可控——最重的是 5.2 的三条直方图，每条 28 条序列（25 显式桶 + 隐式 `+Inf` + `_sum` + `_count`），合计每 `resource` 84 条。
3. **本期不动事件循环**：六条全部落在既有终态发射点，没有新协程、新 ticker、新旗标，也不新增任何对 scheduler cache 的并发访问。这是砍掉"当前碎片率水平"之后的直接收益，别再顺手加回去（第 7 节记了被砍的原因与代价）。
4. **词表同步**：`outcome`、prune `reason`、gate `reason` 都由代码中的字符串常量决定，新增原因时指标侧无需改代码、但文档第 3.3/3.4 节需同步，否则看板会出现无法解释的标签值。
5. **指标面已由快照测试锁定**：`pkg/repackengine/metrics/metrics_test.go` 的 `TestMetricSurfaceSnapshot` 逐条断言 15 个族的名称、标签集、类型与桶边界。桶阶梯在测试里**字面重抄**、不引用包内变量——引用包内变量的话，改桶时测试会跟着一起改，永远绿。桶边界比名字更该锁死：桶一旦上线就不能改，改桶等于让历史数据换口径。类型也一并锁（`assertMetricKind`）：Counter 与 Histogram 互换时名字、标签、样本形态都还像模像样，只有 `_total` 后缀这一条惯例会露馅，而惯例不是保证。新增、重命名或改类型指标时同步更新这张表（重命名会让族从 `/metrics` 消失，测试会红；纯新增不会）。**但快照测试只管声明，管不了"引擎到底调没调"**：把 `status_persistence.go:125` 那次调用整行删掉，快照测试照样全绿。这条接线与 5.2 的跨族恒等式各有一条例会用例盯着，在 `pkg/repackengine/internal/engine/metrics_emission_test.go`。
6. **同步 `docs/design/metrics.md`**：该文档目前没有 repack 一节，本文第 3 节可直接作为该节内容。

## 7. 暂不实现的候选

已评估、本轮不做的方向，连同当时的理由一并记录，避免下次重新论证：

| 候选 | 一句话理由 |
|---|---|
| `planner_duration_seconds` / `execute_duration_seconds` | 拆开 `cycle_duration_seconds` 的合成量；需同时处理看板兼容，收益偏诊断 |
| `placement_outcomes_total{result}` | placement 漂移（`alternative_node`）最直接反映调度器是否尊重计划，但需要先确认漂移是否常态 |
| `benefit_unverified_total{cause}` | `metricsVerified=false` 的三条成因分开计数；本期先用 5.3 的跳过规则 + `runs_total{outcome}` 侧观察 |
| `rejected_pods`（Histogram） | 被拒 victim 的每 Run **分布**。按 resource 的累计口径已由 `evictions_total{resource, result="rejected"}` 覆盖（见 3.1），缺的只是形状；被拒的 Pod 原地没动，所以不属于 5.2 那三条（见 5.2 末段）。但"一趟里有多少计划被挡下"正是零中断 PDB 并发压力的形状，先确认运维真要这个形状再补第四条 |
| `planner_moves_total{mode}` | 补搜索漏斗末节，算「评估/move」效率比 |
| `run_duration_seconds{mode}` | 单 Run 从 start 到终态的墙钟时间 |
| `gate_active` / `runs_pending` | K=1 槽位占用与排队长度，判断 repack 是被饿着还是在空转 |
| `recovery_total{kind}` | 崩溃/重启自愈路径计数，区分「在自愈」与「在崩溃循环」 |
| `terminal_status_write_failures_total` | 终态写回重试耗尽，是 Run 收益静默丢失的唯一出口 |
| `pdb_blocked_tasks` | 被零中断 PDB 静态排除的任务数，解释「为什么整理不动」 |
| controller 侧 policy/nomination 指标 | policy 评估结果与派生 Run 数在 vc-controller-manager，需另注册 endpoint |
| `cluster_fragmentation_percent`（Gauge，集群当前碎片率） | 已设计并实现，**本期撤下**：它是唯一需要新协程 + 新旗标 + 对 scheduler cache 并发开会话的一条，影响面超出"补发射"，详见下节 |

**`cluster_fragmentation_percent` 曾按完整方案实现，随后为收窄影响面撤下**，结论留在这里，免得下次重新推导：

- **不能沿用"Run 终态写一次"**：vc-repack-engine 是**纯事件驱动**的（单 worker 跑 workqueue，全仓无 ticker）。集群空闲时引擎不会醒，Gauge 会永远停在最后一次整理之后的旧值——可能几天前，而 Prometheus 分辨不出"陈旧"与"刚测出"，于是这个量既不能告警也不能被人信任。所以写入者必须是周期任务。
- **替代路径已经够用**：要"当前有多碎"，读 CR `status.plan.summary.fragBeforePercent`（每次 Run 都会写，且注释明确"scope limits actions, not this cluster health metric"），或看 controller 侧 `FragEvalCycle`（`policy/frag.go:104 measureFrag`，默认 10 分钟）——它的局限是没有 policy 就不评估，对指标是缺陷，对巡检够用。
- **若将来重做，这几条是必须的**：独立协程；只开只读会话（`schedframework.CloseSessionReadOnly`）；**不得触碰 workqueue**（Execute 的 K=1 串行化依赖单 worker）；`FragmentationRate()` 返回的是 **0–1 比例**，必须经 `enginestatus.PercentagePoints` 换算，手写 `×100` 会漏掉舍入与钳位、也会与同源 CR 字段漂移；多副本不必加锁——`OnStoppedLeading`（`cmd/volcano-repack-engine/app/server.go:150`）是 `klog.Fatalf`，刷新协程放进 `engine.Run` 就天然只跑在 leader 上，失去 leadership 时进程连同 `/metrics` 一起消失，不会留下冻结序列供人误读。
- **最容易被低估的成本**：它要求引擎**并发开两个会话**，而上游 scheduler 从不这么做（`pkg/scheduler/scheduler.go:155` 是它非测试代码里唯一的会话开启点；repack 自己那一个是 `pkg/repackengine/cache/cache.go:82`）。按 `SchedulerCache.Snapshot()` 的克隆语义（全程持写锁 + 逐对象 `Clone()`）推导是安全的，但这条推理只能由 e2e 上以 `-race` 编译的引擎钉住：引擎现有测试没有任何一个构造过真实的 `SchedulerCache`，伪造一个只会把要验证的克隆语义一起伪造掉。这正是本期撤下它的主要原因。

以下内容不适合进 Prometheus，应继续由 CR `status` 或结构化日志承载：

- **单 Run / 单 Pod 明细**（每条 relocation、每个受害 Pod）：基数与 Run 数同阶，CR `status.relocations`、`status.plan.moves` 已承载。
- **单节点碎片率**：同上，仅在排障时按需从 CR 与日志取。
- **计划评分的各分量**（`gangBreaches`、`movedPods`、`nodeBlockProgress` 等插件权重得分）：属调试细节，走 V4/V5 日志，不是运维信号。
