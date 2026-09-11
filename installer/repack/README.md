# Repack (runtime defragmentation) — deployment

The Repack documentation is organized around three sources of truth:

- [Proposal](../../docs/design/repack-runtime-defragmentation.md): motivation, goals, architecture, and community-level design.
- [Technical design](../../docs/design/repack-design.md): APIs, component interactions, algorithms, execution, recovery, performance, and testing.
- [User guide](../../docs/user-guide/how_to_use_repack.md): prerequisites, constraints, feature walkthroughs, examples, status interpretation, and FAQ.

Repack ships in two pieces:

- **RepackRun controller + nomination reconciler** — manages the `RepackRun`
  lifecycle (admission, the Execute K=1 gate + cooldown, phase/conditions, TTL GC)
  and steers replacement pods. Runs **inside volcano-controller-manager**.
- **volcano-repack-engine** — a standalone Deployment that reuses the scheduler
  cache + the same `scheduler-conf`, plans the defrag, and (Execute) evicts.

## 1. Install the CRD

The `RepackRun` CRD is generated from the kubebuilder markers on the API types:

```bash
make manifests   # → config/crd/.../repack.volcano.sh_repackruns.yaml
kubectl apply -f config/crd/<...>/repack.volcano.sh_repackruns.yaml
```

## 2. Install the controller

The controller is compiled into volcano-controller-manager and registered as
`repack-controller`. Its repack verbs are part of the controller-manager's own
ClusterRole, so there is nothing extra to apply. It is **disabled by default** —
the default list is `--controllers=*,-sharding-controller,-repack-controller` —
so enable it explicitly:

```bash
# `--controllers` is an all-or-nothing override: `+repack-controller` on its own
# would switch off every other default controller, so keep the `*` term.
#   --controllers=*,-sharding-controller,+repack-controller
```

The helm chart sets that flag for you when `repack_enable: true`.

## 3. Install the engine

```bash
kubectl apply -f installer/repack/repack-engine.yaml
```

The engine mounts `volcano-scheduler-configmap` for `--scheduler-conf`, so it sees
the cluster exactly as the scheduler does. Its own Action and Plugin pipeline is
defined separately in `repack-engine.conf` and loaded with `--repack-conf`.
`actions` follows the scheduler syntax (`actions: "repack"`; multiple Actions are
comma-separated). Command-line `--repack-actions` and `--repack-plugins` values
take precedence over the Repack configuration file. The file is mounted from
`volcano-repack-engine-configmap`; it is plain component configuration, not a CR.
The `workloaddisruption` and `gangdisruption` plugin arguments configure cluster-wide disruption-score
weights. Omitted weights use the built-in defaults shown in the manifest; `0`
disables a score term. Values must be non-negative YAML integers. Each strategy
converts its raw disruption cost to an integer score in `[0,100]` (higher is
better), and the weighted sum ranks candidates from highest to lowest. These
weights rank freeable candidates only; receiver nodes retain the fixed
Stability, Disruption, then Packing lexicographic order.
The configuration is parsed strictly: unknown or misspelled top-level fields,
plugin fields, and plugin arguments stop the engine instead of silently using
defaults.

The plugin list is order-independent. `workloadscope`, `pdbconstraint`,
`repackbudget`, `nodeconsolidation`, `networktopologyaware`,
`workloaddisruption`, `gangdisruption`, and `binpack` are optional; omitting one
only disables its policy. `pdbconstraint` excludes accelerator Pods protected by
a fresh, deterministic zero-disruption PDB during planning; temporary allowance
exhaustion is still handled by the Eviction API and the existing retry loop.
`networktopologyaware` is included by default and stays dormant unless a
RepackRun sets `networkTopology`; dropping it from the list silently disables
the HyperNode-tier block constraints of such runs. The `repack` Action requires
at least one plugin that provides the `domain` capability
(`nodeconsolidation` today). Empty
accelerator nodes and fully occupied accelerator nodes are always excluded from
both sides of node-level relocation before scoring; this correctness boundary
does not depend on `binpack`.

## Notes

- Namespace is `volcano-system` throughout; adjust if your install differs.
- Images: `make vc-repack-engine-image` (Dockerfile under
  `installer/dockerfile/repack-engine`; binary via `make vc-repack-engine`). The
  controller needs no image of its own — it ships inside volcano-controller-manager.
- Helm chart integration (templating these under `installer/helm/...`): TODO.
