# Volcano Repack：运行时异构资源碎片整理 Proposal

## Summary

本提案为 Volcano 引入 Repack：一套面向 NPU、GPU 等扩展资源的运行时碎片整理能力。Repack 通过 `RepackRun` 描述一次整理任务，在 Volcano Scheduler 的真实调度语义下评估 Pod 迁移可行性，并提供两种相互独立的执行模式：

- `DryRun`：生成整理计划，不驱逐 Pod；
- `Execute`：重新基于实时快照规划，通过 Kubernetes Eviction API 执行迁移并跟踪结果。

Repack 当前以释放完整目标资源节点为直接收益，以 Scope 和 `maxPerRun` 控制影响范围，以 PodGroup/Gang 语义评估工作负载中断影响。重建 Pod 的落点通过 `nominatedNodeName` 进行软性牵引，最终调度和绑定仍由 Volcano Scheduler 完成，不锁定或预留资源。

![Repack 端到端流程](images/repack/repack-end-to-end.svg)

## Motivation

### 运行中的集群会持续产生资源碎片

装箱调度能够优化新 Pod 的初始放置，却无法避免集群在任务结束、扩缩容、滚动升级、失败重建和优先级变化后逐步偏离紧凑布局。对于加速卡资源，空闲设备总量相同并不代表能够承载相同的工作负载：任务可能要求固定的单 Pod 卡数、完整节点或多个 Pod 同时可用，分散在不同节点上的空闲卡无法直接组合。

结果是集群仍有较多空闲卡，大规格训练或推理扩容却因找不到满足请求的节点组合而持续 Pending。继续扩容硬件会掩盖布局问题并增加成本，人工迁移则难以同时判断工作负载影响、接收节点可行性和最终收益。

### AI 工作负载使迁移决策更复杂

碎片整理不是简单地选择低利用率节点并驱逐 Pod。一个候选方案至少需要回答：

1. 迁移后能否释放业务可使用的完整容量，而不是把碎片转移到其他节点；
2. 所有被迁移 Pod 是否都能满足原有调度约束并获得接收位置；
3. 会影响多少训练或推理工作负载，是否使 PodGroup 低于 `MinAvailable`；
4. 单个 Pod 驱逐是否可能被上层控制器放大为多个 Pod 重建；
5. 在规划与执行并发发生时，如何记录计划、实际落点和最终收益。

因此，Repack 需要复用 Volcano 的调度语义，同时在调度器之外提供可审计、可限制、可独立执行的一次性资源整理流程。

### 与现有能力的关系

- Repack 与 `binpack` 等装箱策略互补：装箱优化新任务放置，Repack 治理已经形成的存量碎片；
- Repack 不是通用 Descheduler 的替代品，其目标是恢复目标扩展资源的可调度容量，并在迁移前验证完整落点；
- Repack 不依赖已经停止演进的 `rescheduling` 路径，也不把整理逻辑嵌入常规调度周期；
- Repack 复用 Volcano Scheduler 的缓存、Session、Predicate 和调度配置，避免维护另一套可行性语义。

## Goals

本阶段目标如下：

- 提供集群级、一次性的 `RepackRun` API，支持 `DryRun` 和 `Execute`；
- 支持按扩展资源名称整理 NPU/GPU 资源，当前一次 Run 只处理一种资源；
- 以完整目标资源节点释放和碎片率改善作为收益判定；
- 通过工作负载与节点 Scope 限定允许驱逐的业务和允许腾空的节点；
- 通过 `maxPerRun` 限制单次影响的工作负载数量和目标资源迁移量；
- 复用 Volcano Scheduler 的完整调度过滤语义验证每个计划落点；
- 在规划阶段过滤新鲜状态已确认的零中断 PDB，避免产生确定无法执行的计划；
- 感知 PodGroup/Gang 中断边界，减少受影响工作负载，并降低突破 `MinAvailable` 的概率；
- 记录计划、逐 Pod 驱逐、替身 Pod 识别、建议节点、实际节点和实际收益；
- 保持 Action 主流程稳定，通过 Plugin 扩展 Scope、预算、Domain、评分和装箱策略；
- 在大规模集群中采用有界的启发式搜索，避免对全部候选执行全量调度模拟。

## Non-Goals

本阶段不包含以下能力：

- 不承诺求解数学意义上的全局最优布局；
- 不为迁移或后续工作负载预留资源，也不强制绑定重建 Pod；
- 不自动监听 Pending 作业并触发整理；
- 不执行抢占、节点 `cordon` 或通过污点独占释放容量；
- 不修改工作负载的资源请求、并行度或拓扑约束；
- 不计算 PDB 动态剩余额度，也不搜索绕开 PDB 的替代驱逐组合；规划期仅过滤确定性零中断 PDB；
- 不对上层控制器的级联重建成本进行通用建模；
- 当前不实现 HyperNode Domain、多节点联合腾空或多跳交换；这些能力通过 Domain Plugin 和后续状态模型演进。

## User Stories

### 周期性治理

平台运维人员定期观察目标节点池的碎片率。当碎片持续升高时，先创建小范围 `DryRun`，确认预计收益和受影响工作负载，再在低峰期创建独立的 `Execute`。

### 大规格任务提交前准备容量

用户计划提交需要多个完整节点的训练任务。平台在任务提交前运行 Repack，将零散占用收敛到部分节点，增加能够直接承载该规格任务的完整节点数量。

### 计划性推理扩容

模型服务预计在业务高峰前扩容。平台按业务标签限定允许迁移的工作负载，并通过执行预算控制本轮影响范围，提前整理所需容量。

### 节点池内受控整理

集群包含多个资源池。平台通过节点标签限定一个节点池，通过工作负载标签声明允许驱逐的业务，确保整理不越过运维边界。

## Proposal

### API Overview

新增 cluster-scoped、创建后 `spec` 不可变的 `RepackRun`：

```yaml
apiVersion: repack.volcano.sh/v1alpha1
kind: RepackRun
metadata:
  name: ascend-pool-dryrun
spec:
  mode: DryRun
  scope:
    podGroups:
      include:
        selector:
          matchLabels:
            repack.example.com/allowed: "true"
    nodes:
      include:
        selector:
          matchLabels:
            accelerator-pool: ascend-a
  goals:
    - resource: huawei.com/ascend-1980
      minFragImprovementPercent: 5
  maxPerRun:
    podGroups: 2
    resources:
      huawei.com/ascend-1980: 16
  ttlSecondsAfterFinished: 86400
```

核心字段分工：

- `mode` 决定只规划还是实际执行；
- `scope.podGroups` 是允许被驱逐的工作负载边界，内部按 PodGroup 聚合；
- `scope.nodes` 是允许作为待腾空目标的节点范围；
- `goals` 声明目标扩展资源和最低碎片改善阈值；
- `maxPerRun` 限制单次影响范围；
- `eviction` 只控制 Eviction API 的提交参数；
- `status.plan` 保存不可变的计划快照；
- `status.result` 和 `status.relocations` 保存 Execute 的实际结果和逐 Pod 过程。

完整字段定义、状态机和写入所有权由[统一技术设计](./repack-design.md)维护，用户配置示例由[用户指南](../user-guide/how_to_use_repack.md)维护，本 Proposal 不重复展开。

### Architecture Overview

Repack 由既有 Kubernetes/Volcano 组件和一个独立 Engine 协作完成：

![Repack Engine 架构与扩展点](images/repack/repack-engine-architecture.svg)

- API Server 保存 `RepackRun`、PodGroup、Pod 和 Node 状态，并通过 CRD/CEL 校验不可变性和字段边界；
- `volcano-repack-engine` 监听 Run，复用 scheduler cache 和 scheduler 配置完成规划，Execute 时调用 Eviction API；
- Volcano Controller Manager 内置 Repack controller，负责 TTL、替身 Pod 认领、placement gate、提名和绑定结果观察；
- Volcano Scheduler 仍是唯一调度与绑定组件，按照实时集群状态处理重建 Pod；
- 工作负载控制器负责在 Pod 被驱逐后创建替身 Pod，Repack 不直接创建业务 Pod。

组件之间仅通过 Kubernetes API 对象协作，不建立私有 RPC。

Engine 的 Reconcile 负责对象读取、Execute 串行门禁和 ActionResult 重试；配置的 `repack` Action 是单次 RepackRun 的业务入口。新建 Run、Eviction journal 恢复、replacement placement 跟踪和终态清理都进入同一 Action，再由 Action 调用 Planner、Plugin 和 Runtime 执行原语，避免正常路径与恢复路径形成两套流程。Runtime 不直接操作 workqueue，只返回重试意图；Action 在进入持久化 Execute 屏障前同步持有执行槽，异常恢复不会提前释放执行槽或启动 cooldown。

### Planning Overview

一次规划采用“低成本裁剪、候选评分、惰性完整校验”的增量流程：

1. 从 scheduler snapshot 解析目标资源、Scope、PodGroup、PDB 和节点状态；
2. 排除空目标资源节点、满卡节点和包含 Scope/确定性零中断 PDB 所阻止的目标资源 Pod 的节点；
3. Domain Plugin 生成可腾空 Unit，当前实现为单节点；
4. 在评分前检查执行预算和接收端目标资源总容量；
5. 从工作负载影响、Gang 破坏、受损资源、迁移资源和 Pod 数等维度计算候选分数；
6. 按总分从高到低依次对候选执行完整 Scheduler 可行性模拟；
7. 提交第一个完整可行的候选，增量更新容量与累计影响后继续下一轮；
8. 计划必须达到完整节点释放和碎片改善门槛，否则不推荐执行。

每个评分策略的得分为 `0～100` 的整数，分越高表示候选越优；综合分为策略得分与整数权重的加权和。评分仅用于同一规划轮次内的相对排序。

### Gang-Aware Disruption

Repack 以 PodGroup 作为工作负载影响统计单元。对于每个候选计划，系统比较计划迁移 Pod 数与 `Running-MinAvailable`：

- 未突破 `MinAvailable` 时，受影响资源按实际迁移量计算；
- 突破 `MinAvailable` 时，该 PodGroup 被记为 Gang breach，并按整个 PodGroup 的目标资源规模计算受损资源。

该模型不把破坏 Gang 作为绝对禁止条件，而是让系统在多个可行候选之间优先选择影响工作负载更少、Gang 中断成本更低的方案。明确不可中断的业务应使用 Scope 排除；如果工作负载已配置并收敛为零中断 PDB，`pdbconstraint` 也会在规划期阻止对应 Pod。

### Static PDB Constraint

Repack 复用 Scheduler Cache 的 PDB informer，在每个 Planning Session 打开时只读取一次。当 PDB status 已观测当前 generation、`ExpectedPods > 0`、`DesiredHealthy >= ExpectedPods` 且 Controller 未报告 `SyncFailed` 时，将其视为配置上不允许任何健康 Pod 主动中断的确定性约束。匹配该 PDB 的目标资源 Pod 不可移动，因此所在节点不进入评分和 Scheduler simulation。

Pod 级判断与 Eviction API 对齐：Pending、终态或删除中的 Pod 跳过 PDB；未 Ready Pod 遵循 `unhealthyPodEvictionPolicy`，默认 `IfHealthyBudget` 还会结合 `CurrentHealthy` 与 `DesiredHealthy` 判断。`DisruptionsAllowed == 0` 可能只是工作负载暂时不健康或 replacement 尚未 Ready，不用作静态过滤条件。这类非零中断 PDB 仍允许进入计划，由 Execute 时的 Eviction API 和现有批次退避重试逐步完成。PDB 缓存、Controller 同步或 selector 异常时 fail-open，Eviction API 仍作为最终权威校验。

![Gang 中断成本模型](images/repack/gang-damage-stepfn.svg)

### DryRun and Execute

`DryRun` 和 `Execute` 是两种独立模式。`Execute` 不复用某个历史 DryRun 的计划，而是基于实时快照重新规划，避免执行过期方案。两种模式使用相同的 planner 和收益语义，因此输出的 `status.plan` 结构一致。

Execute 在驱逐前持久化完整 plan 和逐 Pod relocation journal，形成 prepare barrier；随后调用 Eviction API。替身 Pod 由工作负载控制器创建，经 placement gate 暂停后，由 controller 认领并请求 Engine 基于实时 Scheduler Session 选择接收节点。controller 写入 `nominatedNodeName` 并释放 gate，Scheduler 决定最终绑定位置。

这一过程保留两类事实：

- `selectedNodeName`：Repack 在实时选择时建议的接收节点；
- `actualNodeName`：Scheduler 最终绑定的节点。

二者不同表示发生了替代调度，并不一定代表整理失败；最终是否释放计划节点由终态快照验证。

### Safety Boundaries

- Scope 和 `maxPerRun` 是硬约束，候选评分不能突破；
- 新鲜的确定性零中断 PDB 在 DryRun 和 Execute 的共享规划阶段过滤；
- 每个计划迁移都必须通过完整 Scheduler 可行性检查；
- Execute 通过 Eviction API 按实时状态最终校验 PDB；
- 单集群 Execute 采用 K=1 串行门控并带冷却时间；
- 计划不预留资源，运行时竞争可能使实际结果偏离计划；
- 计划与实际结果分离保存，部分执行失败不会覆盖原始决策；
- Run、PodGroup lease、placement gate 和 relocation journal 支持组件重启后的恢复和幂等清理；Execute 准备未完成时保留全 `Pending` journal 作为恢复依据，外部清理完成后才清除，已发起 Eviction 的 journal 则保留用于审计；
- Engine 使用根 context 停止主循环、缓存、工作队列和进行中的 Eviction 请求，并在退出时关闭 Engine 与 leader-election 的 event broadcaster。

## Design and Implementation

本 Proposal 只维护社区评审所需的稳定边界。以下实现细节统一收敛到[Repack 技术设计](./repack-design.md)：

- 组件读写矩阵和完整时序；
- RepackRun API、状态机和字段所有权；
- Action、Plugin、Session、Planner 与 Snapshot Adapter；
- 节点分类、候选评分、接收节点排序和调度模拟；
- Eviction、替身匹配、placement lease、gate 与 nomination；
- 并发、恢复、可观测性、性能和测试策略。

## Alternatives Considered

### 直接使用 Kubernetes Descheduler

Descheduler 擅长按策略驱逐 Pod，但 Repack 需要基于 Volcano 的 PodGroup、Gang 和调度插件验证完整迁移计划。独立维护一套调度可行性逻辑容易与真实 Scheduler 漂移，因此不采用。

### 在 Volcano Scheduler 周期内执行整理

整理包含用户 Scope、执行预算、DryRun 报告、Eviction 和长生命周期结果跟踪，持续时间和副作用都明显长于一次调度周期。将其放入 Scheduler 会扩大调度关键路径和故障边界，因此采用独立 Engine，并只复用调度框架。

### 为每次迁移预留或锁定资源

资源预留可以降低计划漂移，但需要解决超时回收、死锁、队列公平性和与正常调度竞争等问题。本阶段使用 nomination 软性牵引，保持 Scheduler 为唯一资源分配者。

### 求解全局最优布局

完整搜索需要联合考虑候选节点组合、Pod—Node 映射、Gang、预算和 Scheduler 插件，复杂度随集群规模快速增长；在线状态也会使长时间求得的最优解迅速过期。本阶段采用每步有直接收益的启发式方法，优先保证时延和可解释性。

## Risks and Mitigations

- **计划漂移**：不锁定快照；通过 Execute 重新规划、实时选点和终态验证降低影响；
- **业务中断扩大**：通过 Scope、预算和 Gang 成本降低风险，同时明确上层控制器级联重建属于用户需评估的边界；
- **大集群规划耗时**：通过节点预分类、容量预检、惰性模拟和缓存控制 Scheduler Filter 调用量；
- **组件重启**：通过持久化 relocation journal、lease owner 和幂等状态转换恢复；重复清理只移除当前 Run 拥有的 lease，不依赖额外完成标记；
- **错误配置**：CRD/CEL 和严格的 Engine 配置解析在启动或创建阶段快速失败。

## Rollout Plan

Repack 默认关闭，通过 Helm 显式开启。建议按以下顺序逐步放量：

1. 在测试集群验证目标设备、工作负载控制器和 Scheduler 配置；
2. 在生产集群只运行限定节点池和少量工作负载的 DryRun；
3. 设置严格的 Scope 与 `maxPerRun`，在低峰期执行小规模 Execute；
4. 对比 `status.plan`、`status.result`、业务就绪状态和碎片指标；
5. 根据实际恢复时间逐步扩大范围。

## Future Work

- HyperNode Domain 和按训练 TP/EP、推理 Prefill/Decode role 所需卡数倍数定义的拓扑收益；
- 更完整的 PDB 动态额度、组合与驱逐顺序建模，以及更多工作负载中断成本信号；
- 自动触发和策略化周期运行；
- 多资源联合整理；
- 在保持规划时延边界的前提下改进全局计划质量。

## References

- [Repack 技术设计](./repack-design.md)
- [Repack 用户指南](../user-guide/how_to_use_repack.md)
- [RepackRun API 类型](../../staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/repackrun_types.go)
