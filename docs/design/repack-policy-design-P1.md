# RepackPolicy 实现方案设计 （P1阶段）

> 状态：草稿（待审阅） · 2026-08-15
>
> **Author**: caotuo721 <caotuo721@yeah.net>
>
> **关联文档**：
> - [repack-policy-design.md](./repack-policy-design.md) — Repack 平台治理完整设计（推演与取舍记录）
> - [repack-runtime-defragmentation.md](./repack-runtime-defragmentation.md) — Repack 运行期碎片整理设计提案
> - [gpu-defragmentation-requirements.md](./gpu-defragmentation-requirements.md) — 碎片整理总体需求与 FR/NFR
> - [how_to_use_repack.md](../user-guide/how_to_use_repack.md) — Repack 用户指南
>
> **本文定位**：在已完成的 P0 `RepackRun` 基础上，给出 P1 `RepackPolicy` 的具体实现方案。不重复展开设计推演（详见 `repack-policy-design.md`），只聚焦实现层面的技术决策、文件变更和模块拆解。

---

## 1. 摘要

Volcano P0 已交付 `RepackRun` CRD——一种一次性、spec 不可变的碎片整理工单。用户手动 `kubectl create` 发起 DryRun（模拟）或 Execute（真实驱逐）。

**P1 目标**：引入 `RepackPolicy` CRD，实现**定时、周期性、阈值触发**的碎片整理。RepackPolicy 遵循 **CronJob→Job 模式**——内嵌一份 `RepackRunSpec` 作模板，按触发生成 RepackRun。引擎（`volcano-repack-engine`）**只读 `RepackRun.spec`，从不读 Policy**，因此引擎行为不变。

| 阶段 | 交付内容 | 触发方式 |
|------|---------|---------|
| **P0（已实现）** | `RepackRun` — 一次性碎片整理工单 | 仅手动 `kubectl create` |
| **P1（本方案）** | `RepackPolicy` — 按触发生成 RepackRun | `cronSchedule`（定时）+ `onFragAbovePercent`（碎片率超阈值） |
| **P2+** | 多资源/Run、多 Policy 合并、补充 `onPendingBlocked` 触发 | `onPendingBlocked`（排队受阻） |

---

## 2. 当前 P0 实现

### 2.1 RepackRun CRD

**源码**：`staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/repackrun_types.go`

关键特征：

- **Cluster-scoped**，一次性工单（跑完即终态）
- **spec 不可变**：CEL transition rule `self == oldSelf`，要改就新建
- **准入全在 apiserver**：CEL/marker 完成校验（mode 枚举、`goals≤1`、资源名含 `/` 等），无控制器 Admit
- 两种 mode：`DryRun`（模拟出方案）和 `Execute`（真实驱逐）

**spec 字段**：

| 字段 | 说明 |
|------|------|
| `mode` | `DryRun` / `Execute`（必填） |
| `scope.podGroups` | 候选被搬迁的 PodGroup，include/exclude（label selector + 点名） |
| `scope.nodes` | 限定/排除参与节点 |
| `goals` | 单资源碎片目标（至多一条，如 `nvidia.com/gpu`） |
| `maxPerRun` | 单轮规模封顶（podGroups 数 + 逐资源卡数） |
| `eviction.gracePeriodSeconds` | 驱逐宽限期覆盖值 |
| `ttlSecondsAfterFinished` | 终态后自动 DELETE |

**status 结构**：

```
status (RepackRunStatus)
├── phase (Pending/Running/Succeeded/Failed) + conditions (Progressing/Complete/Failed)
├── plan (不可变计划 → RepackPlan)
│   ├── summary (fragBeforePercent, fragAfterPercent, freedNodeCount, movedCardCount, resolvedScope)
│   ├── moves[] (逐 PodGroup：namespace, podGroupName, owner, cards, pods[{name, fromNode, toNode, cards}])
│   └── freedNodes[] (计划腾空节点名列表)
├── result (Execute 独有 → RepackResult: 实际 fragAfterPercent, freedNodeCount, freedNodes, movedCardCount, metricsVerified)
└── relocations[] (Execute 独有 → PodRelocationStatus: namespace, podGroupName, victimPodName/UID, plannedNodeName, eviction{phase,message}, placement{phase,selectedNodeName,...})
```

### 2.2 架构组件

```
volcano-controller-manager
  └── repack controller (pkg/controllers/repack/repack.go)
        ├── RepackRun 生命周期控制器 —— TTL RunGC + cooldown 保留
        └── Nomination reconciler —— replacement Pod 落点引导

volcano-repack-engine (独立 Pod, cmd/volcano-repack-engine/)
  ├── 复用 scheduler cache + tiers/plugins + framework.OpenSession
  ├── Execute K=1 全局串行 + cooldown
  ├── 崩溃孤儿回收
  └── 只读 RepackRun.spec

volcano-scheduler (不碰 Repack CR)
  └── 常规调度 allocate，接收 repack 释放的容量
```

### 2.3 staging 模块

`staging/src/volcano.sh/repack-controller/` 是独立的、框架无关的模块：

| 文件 | 职责 |
|------|------|
| `pkg/controller.go` | RepackRun 生命周期控制器（纯 client-go informer + workqueue） |
| `pkg/nominate.go` | Replacement Pod 的 soft nomination 引导 |
| `pkg/placement/` | Replacement 匹配与调度要求哈希 |
| `pkg/state/state.go` | 纯函数：phase 推导、gate 判定、TTL/GC 决策 |

---

## 3. RepackPolicy 设计

### 3.1 设计原则

1. **纯模板生成（CronJob→Job 式）**：Policy 只负责「按触发生成 RepackRun」，不承担集群级默认/硬护栏
2. **引擎不变**：`volcano-repack-engine` 只读 `RepackRun.spec`，Policy 引入后引擎零改动
3. **准入 = CEL（apiserver），无控制器 Admit、无继承补全**：对齐 `batch/v1 CronJob`——CronJob 的校验由 apiserver schema validation 完成，不走 webhook/admission controller；CronJob 生成 Job 时直接 DeepCopy `jobTemplate.spec`，默认值由 apiserver 在 Job CREATE 时填充。Policy 同理——校验由 CRD 上的 CEL/marker 完成，生成的 Run 直接 DeepCopy `runTemplate.spec`，默认值由 RepackRun CRD schema 在 CREATE 时自行填充
4. **归属走 K8s 惯例**：生成的 Run 带 `ownerReferences → Policy`（级联删除）
5. **并发策略**：控制器默认「上一派生 Run 未结束则不新建」
6. **历史限制**：扁平 `successfulRunsHistoryLimit` / `failedRunsHistoryLimit`（对齐 CronJob）
7. **反应式触发评估周期**：控制器级启动 flag（`--repack-policy-eval-period`，对齐 CronJob 的 `--cronjob-sync-period`），不进 CRD

### 3.2 CRD Schema

```yaml
apiVersion: repack.volcano.sh/v1alpha1
kind: RepackPolicy
metadata:
  name: a100-auto                  # Cluster-scoped
spec:
  trigger:                         # 两种触发源，命中任一即触发
    cronSchedule: "0 */6 * * *"    # 定时 cron
    onFragAbovePercent: 35           # 碎片率超阈值

  suspend: false                   # 暂停触发

  successfulRunsHistoryLimit: 3
  failedRunsHistoryLimit: 3

  runTemplate:                     # ← 内嵌一份 RepackRun 模板
    spec:                          # = RepackRunSpec（单一事实来源，零 schema 漂移）
      mode: DryRun
      goals:
        - resource: nvidia.com/gpu
          minFragImprovementPercent: 5
      scope:
        nodes:
          include:
            selector:
              matchLabels:
                volcano.sh/node-pool: a100
      maxPerRun:
        podGroups: 10
        resources:
          nvidia.com/gpu: 64
      eviction:
        gracePeriodSeconds: 30
      ttlSecondsAfterFinished: 86400

status:
  conditions:
    - type: Healthy
      status: "True"
      reason: ReconcileSucceeded
      message: "Frag rate 28% below threshold 35%, next cron at 2026-08-17T06:00:00Z, last trigger 2026-08-16T12:00:00Z"
      lastTransitionTime: "2026-08-16T14:00:00Z"
      observedGeneration: 3
  inProgress:
    - kind: RepackRun
      apiVersion: repack.volcano.sh/v1alpha1
      name: "a100-auto-20260816120000"
      namespace: ""
  lastTriggerTime: "2026-08-16T12:00:00Z"
  lastSuccessfulTime: "2026-08-15T12:30:00Z"
  lastEvaluationTime: "2026-08-16T14:00:00Z"
```

### 3.3 Go 类型定义

**新建文件**：`staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/repackpolicy_types.go`

```go
// RepackPolicy is a template-based RepackRun generator (P1, CronJob→Job pattern).
// It is cluster-scoped and user-mutable.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=repackpolicies,scope=Cluster,shortName=rpp;repackpolicy
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="SUSPEND",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="STATUS",type=string,JSONPath=`.status.conditions[?(@.type=="Healthy")].reason`,description="Healthy condition reason"
// +kubebuilder:printcolumn:name="LAST-TRIGGER",type=date,JSONPath=`.status.lastTriggerTime`
// +kubebuilder:printcolumn:name="LAST-EVAL",type=date,JSONPath=`.status.lastEvaluationTime`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type RepackPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   RepackPolicySpec   `json:"spec"`
    Status RepackPolicyStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type RepackPolicyList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []RepackPolicy `json:"items"`
}

type RepackPolicySpec struct {
    // Trigger 何时触发（两种触发源，命中任一即触发）。
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:XValidation:rule="has(self.cronSchedule) || has(self.onFragAbovePercent)",message="trigger must set at least one of cronSchedule/onFragAbovePercent"
    Trigger RepackRunTrigger `json:"trigger"`

    // RunTemplate 派生 RepackRun 的模板（复用 RepackRunSpec）。
    // 生成的 Run 是 DryRun 还是 Execute 完全由 runTemplate.spec.mode 决定。
    // +kubebuilder:validation:Required
    RunTemplate RepackRunTemplateSpec `json:"runTemplate"`

    // Suspend 暂停触发（不影响已生成的 Run）。默认 false。
    // +optional
    // +kubebuilder:default=false
    Suspend *bool `json:"suspend,omitempty"`

    // SuccessfulRunsHistoryLimit 保留最近多少个成功的派生 Run（扁平，对齐 CronJob，默认 3）。
    // +optional
    // +kubebuilder:validation:Minimum=0
    SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`

    // FailedRunsHistoryLimit 保留最近多少个失败的派生 Run（扁平，对齐 CronJob，默认 3）。
    // +optional
    // +kubebuilder:validation:Minimum=0
    FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`
}

// RepackRunTrigger 两种触发源，配了哪个就启用哪个，命中任一即触发。
// 反应式条件的评估周期是控制器级配置
// （启动 flag，全局一份，性质同 Execute 冷静期），不在本 CRD 内。
type RepackRunTrigger struct {
    // CronSchedule 定时触发：标准 5 字段 cron 表达式。
    //
    // 格式：分钟 小时 日 月 星期（空格分隔）
    //
    //   字段     允许值（允许 , - * / ）
    //   分钟     0-59
    //   小时     0-23
    //   日       1-31
    //   月       1-12
    //   星期     0-6 (0=Sun) 或 SUN-SAT
    //
    // 示例：
    //   "0 */6 * * *"   每 6 小时一次
    //   "0 2 * * *"     每天凌晨 2 点
    //   "0 2 * * 1-5"   工作日凌晨 2 点
    //   "*/30 * * * *"  每 30 分钟一次
    //
    // 不设表示不启用定时触发。
    // 参考：https://en.wikipedia.org/wiki/Cron
    // +optional
    CronSchedule *string `json:"cronSchedule,omitempty"`

    // OnFragAbovePercent 碎片率高于此百分比（0–100 整数）触发（反应式）。
    // 不设或为 0 表示不启用碎片率触发。
    // +optional
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=100
    OnFragAbovePercent *int32 `json:"onFragAbovePercent,omitempty"`
}

// RepackRunTemplateSpec 派生 RepackRun 的模板。
type RepackRunTemplateSpec struct {
    // ObjectMeta 派生 Run 的 labels/annotations。
    // +optional
    ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

    // Spec 内嵌 RepackRun 的 spec 本体（单一事实来源，零 schema 漂移）。
    // +kubebuilder:validation:Required
    Spec RepackRunSpec `json:"spec"`
}

// RepackPolicyStatus 对齐 CronJob 的 status 结构。
// 与 CronJob 不同：新增 conditions 表达 Policy 自身的健康状态（CronJob 纯
// 靠 active[] + lastScheduleTime），以及 lastEvaluationTime 记录反应式触发评估。
type RepackPolicyStatus struct {
    // InProgress 尚未终态的派生 Run（Pending 或 Running）。
    // 一旦终态（Succeeded/Failed）即从此列表移除。
    // +optional
    InProgress []v1.ObjectReference `json:"inProgress,omitempty"`

    // LastTriggerTime 最近一次触发时间（因触发源多样，未沿用 lastScheduleTime）。
    // +optional
    LastTriggerTime *metav1.Time `json:"lastTriggerTime,omitempty"`

    // LastSuccessfulTime 最近一次派生 Run 成功完成的时间（Succeeded）。
    // +optional
    LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

    // LastEvaluationTime 最近一次反应式条件评估时间（onFragAbovePercent）。
    // +optional
    LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`

    // Conditions are standard Kubernetes conditions. RepackPolicy uses a single
    // condition type "Healthy" to express whether the last reconcile succeeded.
    //
    // Healthy=True, Reason=ReconcileSucceeded — reconcile completed as expected
    //     (suspend, no trigger, or trigger+Run creation all count as success)
    // Healthy=False, Reason=ReconcileFailed — reconcile encountered an error
    //     (e.g. Run creation API error); operator attention needed
    //
    // +optional
    // +patchMergeKey=type
    // +patchStrategy=merge
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Labels for generated Runs.
const (
    // RepackPolicyLabel 标识派生此 Run 的 Policy 名，用于历史 GC 和并发门控的列表查询。
    RepackPolicyLabel = "repack.volcano.sh/repack-policy"
    // RepackTriggerLabel 记录触发方式：cronSchedule 或 onFragAbovePercent，
    // 便于事后统计和排查。
    RepackTriggerLabel = "repack.volcano.sh/repack-trigger"
)

// Condition type for RepackPolicy.
const (
    // CondHealthy expresses whether the last reconcile succeeded.
    // Healthy=True means the reconcile completed as expected.
    // Healthy=False means the reconcile encountered an error; see reason and message.
    CondHealthy = "Healthy"
)

// Healthy condition reasons — binary: reconcile succeeded or failed.
const (
    // ReasonReconcileSucceeded means the reconcile completed as expected.
    // Covers all expected outcomes: suspend, no trigger hit, or trigger+Run creation.
    ReasonReconcileSucceeded = "ReconcileSucceeded"
    // ReasonReconcileFailed means the reconcile encountered an error that needs attention.
    // Example: trigger matched but Run creation API call failed.
    ReasonReconcileFailed = "ReconcileFailed"
)
```

### 3.4 Scheme 注册

**修改文件**：`staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/register.go`

```go
func addKnownTypes(scheme *runtime.Scheme) error {
    scheme.AddKnownTypes(SchemeGroupVersion,
        &RepackRun{},
        &RepackRunList{},
        &RepackPolicy{},
        &RepackPolicyList{},
    )
    metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
    return nil
}
```

### 3.5 触发机制详解

| 触发类型 | 评估方式 | 默认评估周期 |
|---------|---------|------------|
| `cronSchedule` | 解析 cron 表达式，计算 `Next()` 时间，`workQueue.AddAfter` 精确调度 | 由 cron 表达式决定 |
| `onFragAbovePercent` | 实时计算集群碎片率，超过 `onFragAbovePercent` 则触发 | 控制器 flag `--repack-policy-eval-period`（默认 10min，对齐 Execute 冷静期） |

**碎片率计算**（与引擎一致）：

```
FragRate(R) = (occupiedNodes - minPossibleNodes) / totalNodes × 100%
```

- `totalNodes` = `Allocatable[R] > 0` 的节点数
- `occupiedNodes` = 正在使用资源 R 的节点数
- `minPossibleNodes` = 保持当前资源请求不变、紧凑装箱所需的最少节点数

> **设计说明**：碎片率在 Policy 控制器内部实时计算，**不读取历史 Run 的 `fragAfterPercent`**（集群碎片率持续变化，旧值不准）。
>
> 计算所需数据均来自标准 K8s API：
> - **Node** `status.allocatable[R]` → 得到 `ProvidingNodeCount`（哪些节点提供该资源、各节点容量多少）。K8s Node 对象 **只携带 allocatable，不记录当前已用量**。
> - **Pod** `spec.containers[*].resources.requests[R]` → 按 nodeName 聚合得到 `OccupiedNodeCount`（哪些节点被占用），同时收集逐 Pod 请求量作为 `resourceRequests[]`，输入 bin-packing 下界算法求得 `OptimalOccupiedNodeCount`。
>
> **为什么两个 informer 缺一不可**：碎片率公式的三个变量中，Node 只能提供 `ProvidingNodeCount`；`OccupiedNodeCount`（哪些节点有 Pod 占用该资源）和 `OptimalOccupiedNodeCount`（紧凑装箱最少需要多少节点）都必须从 Pod requests 聚合得到。只靠 Node informer 算不出碎片率。
>
> Policy 控制器通过 `NodeInformer` + `PodInformer` 获取这些数据，无需依赖 scheduler cache。`OptimalOccupiedNodeCount` 的计算逻辑与引擎的 `MeasureResourceFragmentation` 等价（同构集群使用闭式公式精确计算，异构集群按节点容量降序贪心求下界）。参见 `pkg/repackengine/api/fragmentation.go`。

> **eval-period 与 Execute 冷静期的关系**：`--repack-policy-eval-period` 默认 10min，对齐 Execute 模式的冷静期（`--repack-execute-cooldown`，默认 10min）。若 eval period < cooldown，会在冷静期窗口内生成引擎暂时无法执行的 Run（卡在 Pending 状态等待冷静期结束），造成无意义的排队。保持两者默认值一致可避免此问题。运维调整时应确保 eval period ≥ cooldown。

---

## 4. 实现步骤

### 阶段 1：API 类型与代码生成

**1.1 创建类型文件**

新建 `staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/repackpolicy_types.go`，定义 §3.3 中所有类型。

关键 kubebuilder 标记：

- `+genclient:nonNamespaced` — Cluster-scoped
- `+kubebuilder:resource:path=repackpolicies,scope=Cluster,shortName=rpp;repackpolicy`
- `+kubebuilder:subresource:status`
- `+kubebuilder:validation:XValidation` — CEL 校验规则

**1.2 修改 register.go**

在 `addKnownTypes()` 中注册 `RepackPolicy` 和 `RepackPolicyList`。

**1.3 运行代码生成**

```bash
cd staging/src/volcano.sh/apis
./hack/update-codegen.sh
```

生成内容：
- `zz_generated.deepcopy.go`（新增 Policy 类型的 DeepCopy 方法）
- `pkg/client/clientset/versioned/typed/repack/v1alpha1/repackpolicies.go` — 新 typed client
- `pkg/client/informers/externalversions/repack/v1alpha1/repackpolicy.go` — 新 informer
- `pkg/client/listers/repack/v1alpha1/repackpolicy.go` — 新 lister
- `pkg/client/applyconfiguration/repack/v1alpha1/` — 新 applyconfigurations

### 阶段 2：CRD YAML 与 CEL 校验

**2.1 生成 CRD**

```bash
make manifests
```

生成 `config/crd/volcano/bases/repack.volcano.sh_repackpolicies.yaml`（当前该文件是占位符，需替换为完整 CRD schema）。

**2.2 CEL 校验规则**

在 RepackPolicySpec 上通过 kubebuilder marker 添加校验：

```
// runTemplate 必填——由 +kubebuilder:validation:Required 保证，无需额外 CEL
// mode 枚举、goals≤1、数值范围等继承自 RepackRunSpec 的 marker

// 至少配置一个 trigger（P1 仅两种触发源，onPendingBlocked 推迟至 P2/12 月 30 日）
// CEL rule: "has(self.trigger) && (has(self.trigger.cronSchedule) || has(self.trigger.onFragAbovePercent))"

// goaps 至多一条（继承自 RepackRunSpec.Goals 的 +kubebuilder:validation:MaxItems=1）
// mode 枚举（继承自 RepackMode 的 +kubebuilder:validation:Enum）
// 数值范围（继承自各字段的 +kubebuilder:validation:Minimum/Maximum）
```

### 阶段 3：RepackPolicy 控制器

**3.1 新建 Policy 控制器包**

目录：`staging/src/volcano.sh/repack-controller/pkg/policy/`（新建）

文件：

| 文件 | 职责 |
|------|------|
| `policy_controller.go` | 主控制器：Reconcile 循环 + 触发评估 + Run 生成 |
| `policy_run.go` | `constructRunFromTemplate()` — 从模板构建 RepackRun |
| `policy_gc.go` | `gcHistory()` — 按 historyLimit 清理旧 Run |
| `policy_state.go` | 纯函数：`hasInProgressRun()`、`nextCronFire()` 等 |
| `policy_frag.go` | 碎片率实时计算：从 Node + Pod informer 数据计算 `FragRate(R)`（复用与引擎一致的算法） |
| `policy_controller_test.go` | 单元测试 |

**Controller 结构**：

```go
type Controller struct {
    volcanoClient        vcclientset.Interface
    policyLister         repacklisters.RepackPolicyLister
    policyInformerSynced cache.InformerSynced
    runLister            repacklisters.RepackRunLister
    runInformerSynced    cache.InformerSynced
    nodeInformer         coreinformers.NodeInformer  // onFragAbovePercent 碎片率实时计算
    podInformer          coreinformers.PodInformer   // onFragAbovePercent 碎片率实时计算
    workQueue            workqueue.TypedRateLimitingInterface[string]

    informerFactory      vcinformers.SharedInformerFactory
    workers              int
    evalCycle            time.Duration   // 反应式触发评估周期
    defaultSuccessLimit  int32           // 默认值 3
    defaultFailedLimit   int32           // 默认值 3（对齐 CronJob）
    now                  func() time.Time
}
```

**工作队列事件源**：Policy 控制器只 reconcile 自己的 RepackPolicy 对象，不对单个 Run 的变化做 reconcile（与 CronJob controller 设计一致——CronJob controller 不对 Job 事件做 queue.Add）。Run informer 仅用于 GC 和 inProgress 清理的只读查询（通过 lister），不注册 event handler。

触发时机：

1. **RepackPolicy Add/Update**：每次 Policy 变化时入队 reconcile
2. **Cron 到期**：由 controller 内的 `workQueue.AddAfter` 精确调度（在 reconcile 中根据 cron 表达式计算 nextFire，对每个 Policy 独立 requeue）
3. **反应式评估周期**：由 controller 内的独立定时器（`time.Ticker`）驱动，每隔 `evalCycle` 遍历全部 Policy 的 lister，对每个配置了 `onFragAbovePercent` 的 Policy 入队 reconcile。定时器不预判碎片率——它是纯唤醒机制，确保仅配了反应式触发（无 cron）的 Policy 也能被周期性评估。碎片率判断统一在 reconcile 中完成。

**Cron requeue 策略**：

- 每次 reconcile 结束时计算该 Policy 的下次 cron fire 时间，用 `workQueue.AddAfter(key, nextFire.Sub(now))` 精确调度
- 若 Policy 被删除，`AddAfter` 的到期触发会因 lister.Get 返回 NotFound 而安全结束
- 若 Policy 的 cronSchedule 被更新，新 reconcile 会重新计算 nextFire 并 `AddAfter`，但 K8s workqueue **不去重**——旧的 `AddAfter` 到期时仍会触发一次多余的 reconcile。这无法避免，与 CronJob controller 行为一致。多余的 reconcile 靠以下方式无害化：
  - **确定性 Run 命名**：`{policy-name}-{YYYYMMDDHHmmss}`，同一时刻只会生成同名 Run，API 层 AlreadyExists 自动拦截
  - **`lastTriggerTime` 去重**：若当前 cron 时间点已在 `lastTriggerTime` 记录过，跳过创建

**Reconcile 核心逻辑**：

1. 从 lister 获取 Policy（NotFound → 结束）
2. **清理 inProgress[]**：扫描 `inProgress[]` 中每个 Run 的状态——若 Run 已 Succeeded 则更新 `lastSuccessfulTime`（取最新值），将已终态（Succeeded/Failed）的 Run 从列表中移除。先清理状态再做后续判断，保证并发门控看到的是最新状态
3. 若 `spec.suspend == true`：更新 condition 为 `Healthy=True, reason=ReconcileSucceeded, message="Suspended"`，跳到步骤 8（历史 GC）
4. **全局 Execute 门控**（仅 `runTemplate.spec.mode == Execute` 时生效）：通过 `runLister` 扫描全部 RepackRun，若存在任一非终态的 Execute 模式 Run，更新 condition message 为 "Skipped: another Execute Run active or in cooldown"，跳到步骤 8。DryRun 模式不受此限制
5. 评估两种触发条件（reconcile 无法区分入队来源，必须独立评估全部条件）：
   - **cronSchedule**：以 `max(lastTriggerTime, creationTimestamp)` 为基准，用 cron 库计算 `Next()` 时间。若当前 ≥ nextFire 则命中；若已命中则检查 `lastTriggerTime` 是否已记录该时间点（幂等去重）
   - **onFragAbovePercent**：通过 Node + Pod informer 实时计算碎片率（详见 §3.5），≥ 阈值则命中
   - 更新 `status.lastEvaluationTime`。若两种触发均未命中，更新 condition message 为当前碎片率/下次 cron 等评估结果，跳到步骤 8
6. **本 Policy 并发门控**：若 `status.inProgress[]` 仍非空（上一个 Run 尚未终态），跳过创建，跳到步骤 8
   - **注**：对 Execute 模式，此步冗余于第四步的全局门控；但对 **DryRun 模式，这是唯一门控**——全局门控豁免 DryRun，若无此步，DryRun 会在每个 eval period 无节制地创建新 Run（即使碎片率未变化、上一个 DryRun 刚结束）
7. **创建 RepackRun**：
   - 命名：`{policy-name}-{YYYYMMDDHHmmss}`
   - DeepCopy `runTemplate.spec` 到新 Run（含 mode、scope、goals 等全部字段）
   - 合入 `runTemplate.metadata.labels/annotations`
   - 设置 `metadata.ownerReferences` → Policy（`controller=true, blockOwnerDeletion=false`）
   - 设置 labels：
     - `repack.volcano.sh/repack-policy: {policyName}`（用于历史 GC）
     - `repack.volcano.sh/repack-trigger: {cronSchedule|onFragAbovePercent}`（记录触发来源）
   - CREATE 到 API
   - 成功：`inProgress[]` append 新 Run 的 ObjectReference，`lastTriggerTime` = now，condition 更新为 `Healthy=True, reason=ReconcileSucceeded`，message 包含触发摘要
   - 失败：condition 更新为 `Healthy=False, reason=ReconcileFailed`，message 包含 API 错误详情
8. **历史 GC**：
   - 按 label `repack.volcano.sh/repack-policy={policyName}` 列出所有派生 Run
   - 对 Succeeded/Failed Run 分别按 `creationTimestamp` 降序排列
   - 超出 `historyLimit` 的最旧 Run DELETE

**3.2 集成到 controller-manager**

**修改文件**：`pkg/controllers/repack/repack.go`

```go
type repackController struct {
    runCtrl         *rc.Controller
    nominator       *rc.Nominator
    policyCtrl      *rcpolicy.Controller  // 新增
    workers         int
    policyEvalCycle time.Duration         // 新增
}

// 实现 FlagProvider 接口，注册控制器专属启动参数
func (c *repackController) AddFlags(fs *pflag.FlagSet) {
    fs.DurationVar(&c.policyEvalCycle, "repack-policy-eval-period",
        10*time.Minute,
        "onFragAbovePercent 碎片率触发条件的重新评估周期（应 ≥ Execute 冷静期，避免在冷静期窗口内生成无法立即执行的 Run）")
}

func (c *repackController) Initialize(opt *framework.ControllerOption) error {
    // ... 现有 run controller + nominator 初始化 ...
    c.policyCtrl = rcpolicy.New(opt.VolcanoClient, opt.VCSharedInformerFactory,
        opt.SharedInformerFactory, rcpolicy.Options{
            Workers:               c.workers,
            EvalCycle:             c.policyEvalCycle,
            DefaultSuccessHistoryLimit: 3,
            DefaultFailedHistoryLimit:  3,
        })
    return nil
}

func (c *repackController) Run(stopCh <-chan struct{}) {
    ctx, cancel := context.WithCancel(context.Background())
    go func() { <-stopCh; cancel() }()

    go func() { c.runCtrl.Run(ctx) }()
    go func() { c.nominator.Run(ctx) }()
    go func() {
        if err := c.policyCtrl.Run(ctx); err != nil {
            klog.ErrorS(err, "RepackPolicy controller stopped")
        }
    }()
}
```

`FlagProvider` 接口定义于 `pkg/controllers/framework/interface.go`，controller-manager 在 `Initialize` 之前对其调用 `AddFlags`。

**3.3 Policy 控制器的依赖**

Policy 控制器需要以下 informer：

| Informer | 用途 | 来源 |
|----------|------|------|
| `RepackPolicy` | 被 reconcile 的对象 | `VCSharedInformerFactory.Repack().V1alpha1().RepackPolicies()` |
| `RepackRun` | 并发门控（检查 inProgress[] 中的 Run 状态）+ 历史 GC | 同上 |
| `Node` | onFragAbovePercent 触发检测（计算碎片率：判断 totalNodes、节点容量） | `SharedInformerFactory.Core().V1().Nodes()` |
| `Pod` | onFragAbovePercent 触发检测（计算碎片率：判断 occupiedNodes、收集各 Pod 请求量） | `SharedInformerFactory.Core().V1().Pods()` |

这些 informer 在 controller-manager 中已全部可用。

### 阶段 4：RunGC 增强

**设计选择**：Policy 历史限制由 Policy 控制器自行处理，**不修改现有 RunGC**。

理由：
- TTL（`ttlSecondsAfterFinished`）是 Run 级别的生命周期字段，由 RunGC 处理
- `successfulRunsHistoryLimit` / `failedRunsHistoryLimit` 是 Policy 级别的字段，由 Policy 控制器处理
- 两者独立共存，互不冲突，先触达者先删

Policy 控制器的 reconcile 循环中执行历史 GC：
1. `runLister.List(labels.SelectorFromSet({"repack.volcano.sh/repack-policy": policy.Name}))`
2. 按 phase 分组（Succeeded / Failed），每组按 `creationTimestamp` 降序
3. 超出 limit 的最旧 Run DELETE
4. 清理 `status.inProgress[]` 中已终态的 Run 引用

### 阶段 5：Helm Chart 与部署

**修改文件**：`installer/helm/chart/volcano/`

- 新增 `custom.repack_policy_enable` flag（默认 `false`）
- 开启时：
  - 部署 RepackPolicy ClusterRole（CREATE/GET/LIST/WATCH/UPDATE/PATCH/DELETE on `repackpolicies` + `repackruns`；LIST/WATCH on `nodes` 和 `pods`，用于 onFragAbovePercent 碎片率实时计算和 Run 历史 GC）
  - controller-manager 启动参数添加 `--repack-policy-eval-period=10m`

### 阶段 6：单元测试

| 测试文件 | 覆盖内容 |
|---------|---------|
| `repackpolicy_types_test.go` | DeepCopy 正确性、YAML round-trip |
| `policy_controller_test.go` | suspend 不生成 Run、cron 触发时机、并发门控、inProgress 列表管理、Run 元数据验证 |
| `policy_gc_test.go` | 历史限制裁剪逻辑（Succeeded=3, Failed=3 默认值） |
| `policy_state_test.go` | `hasInProgressRun()`、`nextCronFire()` 等纯函数 |
| `policy_frag_test.go` | 碎片率计算正确性（同构/异构集群场景） |

> E2E 测试推迟至实现完成后补充。

---

## 5. 文件变更清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/repackpolicy_types.go` | **新建** | RepackPolicy 类型定义 |
| `staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/register.go` | 修改 | 注册 Policy 类型到 Scheme |
| `staging/src/volcano.sh/apis/pkg/apis/repack/v1alpha1/zz_generated.deepcopy.go` | 重新生成 | DeepCopy 方法 |
| `staging/src/volcano.sh/apis/pkg/client/` | 重新生成 | clientset / informer / lister / applyconfiguration |
| `config/crd/volcano/bases/repack.volcano.sh_repackpolicies.yaml` | 重新生成 | 完整 CRD YAML（替换占位符） |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_controller.go` | **新建** | Policy 控制器主逻辑 |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_run.go` | **新建** | 从模板构造 Run 的辅助函数 |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_gc.go` | **新建** | 历史限制裁剪 |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_state.go` | **新建** | 纯函数：终端态判断、cron 时间计算 |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_frag.go` | **新建** | 碎片率实时计算（Node+Pod informer 数据→FragRate） |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_controller_test.go` | **新建** | 控制器单元测试 |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_state_test.go` | **新建** | 纯函数单元测试 |
| `staging/src/volcano.sh/repack-controller/pkg/policy/policy_frag_test.go` | **新建** | 碎片率计算单元测试 |
| `pkg/controllers/repack/repack.go` | 修改 | 集成 Policy 控制器 + FlagProvider |
| `installer/helm/chart/volcano/` | 修改 | Helm flag + RBAC |

---

## 6. 不在范围（P2+）

- 单 Run 多资源整理（`goals` 多条，多资源合成）
- 多 Policy 合并与跨 Policy 冲突仲裁
- `RepackConfig` 系统级配置对象
- 集群级默认护栏/跨 Run 强制保护（另议——CEL `ValidatingAdmissionPolicy` 或后续单开 CRD）
- 按 scope 维度分别计冷静期
- per-Policy 自定义并发上限（如 scope 不相交时并行）

---

## 7. 验证方式

```bash
# 代码生成
make generate-code              # DeepCopy + clientset/informer/lister
make manifests                  # CRD YAML

# 构建
make vc-controller-manager      # 编译验证
make vc-repack-engine           # 引擎不变，确保无退化
make images                     # 所有镜像

# 单元测试
make unit-test                  # 全量单测
```