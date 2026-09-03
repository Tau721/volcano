# Repack (runtime defragmentation) — deployment

The Repack documentation is organized around three sources of truth:

- [Proposal](../../docs/design/repack-runtime-defragmentation.md): motivation, goals, architecture, and community-level design.
- [Technical design](../../docs/design/repack-design.md): APIs, component interactions, algorithms, execution, recovery, performance, and testing.
- [User guide](../../docs/user-guide/how_to_use_repack.md): prerequisites, constraints, feature walkthroughs, examples, status interpretation, and FAQ.

Repack ships in two pieces:

- **RepackRun controller + nomination reconciler** — manages the `RepackRun`
  lifecycle (admission, the Execute K=1 gate + cooldown, phase/conditions, TTL GC)
  and steers replacement pods. Runs **by default inside volcano-controller-manager**;
  can also run standalone.
- **volcano-repack-engine** — a standalone Deployment that reuses the scheduler
  cache + the same `scheduler-conf`, plans the defrag, and (Execute) evicts.

## 1. Install the CRD

The `RepackRun` CRD is generated from the kubebuilder markers on the API types:

```bash
make manifests   # → config/crd/.../repack.volcano.sh_repackruns.yaml
kubectl apply -f config/crd/<...>/repack.volcano.sh_repackruns.yaml
```

## 2. Choose a controller deployment mode

### Default — built-in (recommended)

The controller is compiled into volcano-controller-manager and enabled by the
default `--controllers=*`. Just grant the extra RBAC:

```bash
kubectl apply -f installer/repack/repack-controller-rbac.yaml
```

Do **not** deploy `repack-controller-standalone.yaml` in this mode.

### Standalone (optional)

Run the controller as its own Deployment and **disable the built-in copy** so the
two don't both reconcile the same objects:

```bash
# tell volcano-controller-manager to skip the built-in repack controller:
#   --controllers=*,-repack-controller
kubectl apply -f installer/repack/repack-controller-standalone.yaml
```

Do **not** apply `repack-controller-rbac.yaml` in this mode (the standalone file
carries its own ServiceAccount + role).

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

The plugin list is order-independent. `workloadscope`, `repackbudget`,
`networktopologyaware`, `workloaddisruption`, `gangdisruption`, and `binpack`
are optional; omitting one only disables its policy. The `repack` Action
requires at least one plugin that provides the `domain` capability
(`nodeconsolidation` today). `networktopologyaware` is included by default and
stays dormant unless a RepackRun sets `networkTopology`; dropping it from the
list silently disables the HyperNode-tier block constraints of such runs. Empty
accelerator nodes and fully occupied accelerator nodes are always excluded from
both sides of node-level relocation before scoring; this correctness boundary
does not depend on `binpack`.

## Notes

- Namespace is `volcano-system` throughout; adjust if your install differs.
- Images: `make vc-repack-engine-image` and `make vc-repack-controller-image`
  (Dockerfiles under `installer/dockerfile/repack-engine` /
  `installer/dockerfile/repack-controller`; binaries via `make vc-repack-engine`
  / `make vc-repack-controller`). The standalone controller builds from its own
  module under `staging/src/volcano.sh/repack-controller`.
- Helm chart integration (templating these under `installer/helm/...`): TODO.
