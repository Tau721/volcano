# Repack E2E coverage and diagnostics

The Repack suite mutates node capacity, node taints, and the singleton
`volcano-repack-engine` Deployment. All top-level containers are therefore
`Serial`; running individual specs in parallel is unsupported.

## Core coverage

| Area | E2E coverage |
|---|---|
| DryRun and status | fragmented, clean, below-threshold, default/explicit/absent resource, deterministic complete plan |
| Planning | nine consolidation layouts, lower-disruption migration direction, whole-PodGroup movement with `minAvailable=1`, static zero-disruption PDB full/partial filtering and relaxation, informer synchronization, empty-node exclusion |
| Scheduler fidelity | taints, required node affinity, receiver ordering, immediately-idle capacity |
| Scope and limits | node exclusion, exact PodGroup name, automatic PodGroup labels, PodGroup/card limits |
| Execute result | success, verified no-op, static PDB filtering, dynamic PDB rejections and plan/result separation, exact planned/actual freed-node mismatch |
| Workloads | successful vcjob and Deployment Execute; Deployment and StatefulSet replacement admission; full Execute plus repeated workload-level PodGroup recreation with different PG/Pod names |
| Placement protocol | synchronous gate, engine and controller-manager restart checkpoints, scale-out ambiguity, capacity expiry, binding drift |
| Lifecycle | Execute cooldown, TTL garbage collection, status conditions/message/completion time |
| Observability | RepackRun milestone Events and replacement Pod placement Events |

Unit tests remain the primary coverage for deterministic failure injection:
API conflicts/retries, panic recovery, active Execute serialization, stale lease
ownership, exact/hash/homogeneous-PodGroup matching, eviction grace periods, and planner
performance bounds.

The suite intentionally does not claim coverage for real accelerator device
plugins, controller-manager leader failover, or arbitrary external workload
churn. It uses a fake extended resource and deterministic concurrent-change
checkpoints instead.

## Reliability rules

- Workloads are deleted before fake node resources are removed.
- Schedulable node names are sorted before fixture selection.
- Polling tolerates transient API GET failures until the test deadline.
- Scope assertions require a non-empty plan; an empty loop must never pass a
  selection test.
- Synthetic placement checkpoints populate both `status.plan` and
  `status.result`, matching the production state machine.
- `--ginkgo.dry-run` works without a kubeconfig and reports the exact spec count.

On failure, the suite attaches the following Ginkgo report entries before
cleanup:

- RepackRun, PodGroup, namespace Pod, and Event snapshots;
- the last 200 log lines from repack-engine, controller-manager, admission, and
  scheduler Pods.

## Operator signals

RepackRun Events describe the global lifecycle:

`PlanComputed` → `ExecutePrepared` → `EvictionsIssued` →
`ReconcilingPlacements` → `PlacementSelected` / `WaitingForNodeSelection` →
terminal condition reason. `PlacementTimedOut`, `ResultVerificationFailed`, and
`BenefitNotRealized` are Warnings. `ExecutionCompletedWithAlternativePlacement` is successful
when every replacement is scheduled and the exact planned freed-node set is
verified.

Replacement Pod Events describe the concrete placement:

`RepackReplacementGated` → `RepackPlacementNominated` →
`RepackPlacementSucceeded`, or `RepackAlternativePlacement` /
`RepackPlacementReleased`. `RepackPlacementRecovered` reports that a new Pod
took over a stale claim from a deleted replacement; `RepackPlacementNotMatched`
reports that an unrelated scale-out Pod was released with a single event.

Log levels follow this contract:

- V(3): operator narrative and important aggregate results;
- V(4): reconcile attempts, lease changes, individual evictions, receiver
  candidates/selections, Pod identity matching, and cleanup details.

## Running

```bash
# Compile and enumerate without a cluster.
go test -c ./test/e2e/repack -o /tmp/volcano-repack-e2e.test
/tmp/volcano-repack-e2e.test --ginkgo.dry-run --ginkgo.v

# Full kind suite.
make e2e-test-repack
```
