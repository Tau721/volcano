# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

### Build
- `make all` — build all binaries (vc-scheduler, vc-agent-scheduler, vc-controller-manager, vc-webhook-manager, vc-agent, vc-repack-engine, vcctl, plus CLI subcommands)
- `make vc-scheduler` / `make vc-controller-manager` / etc. — build a single binary
- `make vc-repack-controller` — build the standalone repack controller from `staging/src/volcano.sh/repack-controller/`
- `make images` — build Docker images for all core components
- `make repack-e2e-images` — build Docker images needed for repack e2e tests
- `make repack-e2e-images-from-bin` — fast local image build from pre-compiled binaries (skips docker buildx golang stages)
- `make release` — build images + generate YAML manifests

### Test
- `make unit-test` — run all unit tests; on Linux uses `-race -p 8` across `pkg/` and `cmd/`; on macOS uses `go list` + `GOOS=darwin`
- Run a single test: `go test ./pkg/scheduler/plugins/binpack/ -run TestBinpack -v`
- `make e2e` — build images + run full e2e suite on kind
- `make e2e-test-schedulingbase` / `make e2e-test-repack` / `make e2e-test-hypernode` / etc. — run specific e2e suites
- `make e2e-test-repack-local` — run repack e2e using locally-built binaries (faster iteration)

### Lint and Verify
- `make lint` — run golangci-lint (configured in `.golangci.yml`)
- `make verify` — run `hack/verify-gofmt.sh` + `hack/verify-gencode.sh`
- `make lint-licenses` — verify third-party license compliance

### Code Generation
- `make generate-code` — run client-gen, informer-gen, lister-gen, deepcopy-gen
- `make manifests` — generate CRD YAML files via controller-gen (requires `controller-gen` binary, auto-downloaded if missing)

### Local Development
- `./hack/local-up-volcano.sh` — spin up a local Kubernetes cluster with Volcano
- `./hack/local-up-cluster.sh` — spin up a local kind cluster (no Volcano)

## Architecture

### Module Layout
- Module path: `volcano.sh/volcano` (Go 1.25), pinned to Kubernetes v0.35.3 via `replace` directives
- Vendored dependencies live in `vendor/` (includes both upstream deps and local staging modules)
- **Staging pattern** (like k8s.io upstream): local modules under `staging/src/volcano.sh/` are linked via `replace` directives in `go.mod`:
  - `staging/src/volcano.sh/apis/` — ALL CRD API types (batch, scheduling, bus, nodeinfo, topology, flow, repack, config, shard, training) plus generated clients, informers, listers, and apply configurations
  - `staging/src/volcano.sh/repack-controller/` — standalone repack controller with its own `go.mod`

### Binaries (`cmd/`)
| Directory | Binary | Purpose |
|---|---|---|
| `cmd/scheduler/` | vc-scheduler | Core batch scheduler |
| `cmd/agent-scheduler/` | vc-agent-scheduler | Agent scheduler variant |
| `cmd/controller-manager/` | vc-controller-manager | Controllers (Jobs, Queues, PodGroups, CronJobs, Hypernodes, Shards, etc.) |
| `cmd/webhook-manager/` | vc-webhook-manager | Admission webhooks (validating + mutating) |
| `cmd/volcano-repack-engine/` | vc-repack-engine | Hypernode pod defragmentation engine |
| `cmd/agent/` | vc-agent + network-qos | Per-node agent and CNI network QoS plugin |
| `cmd/cli/` | vcctl + subcommands | CLI for job/queue operations (vcancel, vresume, vsuspend, vjobs, vqueues, vsub) |

Also: `vc-repack-controller` built from `staging/src/volcano.sh/repack-controller/cmd/repack-controller/`.

### Scheduler Framework (`pkg/scheduler/`)
The scheduler uses a **session-based action+plugin framework** modeled after kube-scheduler's scheduling framework:

- **Actions** (in `pkg/scheduler/actions/`) orchestrate each scheduling round: `allocate`, `backfill`, `enqueue`, `preempt`, `reclaim`, `shuffle`, `gangpreempt`, `gangreclaim`
- **Plugins** (in `pkg/scheduler/plugins/`) implement individual scheduling policies: `binpack`, `capacity`, `cdp`, `conformance`, `deviceshare`, `drf`, `extender`, `gang`, `network-topology-aware`, `nodegroup`, `nodeorder`, `numaaware`, `overcommit`, `pdb`, `predicates`, `priority`, `proportion`, `rescheduling`, `resource-strategy-fit`, `resourcequota`, `sla`, `task-topology`, `tdm`, `usage`
- **Framework** (`pkg/scheduler/framework/`): `Session` holds per-cycle state (nodes, jobs, queues); `Statement` tracks per-task scheduling decisions; `Arguments` injects plugin configuration; `interface.go` defines Action and Plugin interfaces
- Plugin registration uses a factory pattern via `pkg/scheduler/plugins/factory.go` and `defaults.go`
- **Cache** (`pkg/scheduler/cache/`) maintains the scheduling view of the cluster (nodes, pods, podgroups, queues)

### Repack Engine (`pkg/repackengine/`)
A separate framework for GPU/NPU cluster defragmentation, mirroring the scheduler framework pattern:

- **Framework** (`pkg/repackengine/framework/`) — candidate selection, plugin/receiver interfaces, per-run session
- **Planner** (`pkg/repackengine/planner/drain/`) — incremental lazy drain planner
- **Actions** (`pkg/repackengine/actions/repack/`) — main repack action orchestrator
- **Plugins** (`pkg/repackengine/plugins/repackbudget/`) — budget enforcement plugins
- **Adapter** (`pkg/repackengine/adapter/`) — bridges to the scheduler cache, gang-aware interfaces, node freeability evaluation
- The standalone **repack-controller** in staging handles nomination, placement recovery, gang-aware draining, and PodGroup lease management

### Controllers (`pkg/controllers/`)
Controllers for CRD reconciliation: `job`, `queue`, `podgroup`, `cronjob`, `jobflow`, `jobtemplate`, `hypernode`, `repack`, `sharding`, `garbagecollector`

### Webhooks (`pkg/webhooks/`)
Admission webhooks organized by resource: `jobs` (validate + mutate), `podgroups`, `queues`, `pods`, `cronjobs`, `hypernodes`, `jobflows`

## Key Conventions
- **Commit format**: `<subsystem>: <what changed>` — Kubernetes-style, subject ≤70 chars, body explaining why
- **PRs require approval from two maintainers**, CI runs tests automatically
- **Code generation** scripts in `hack/`: `update-gencode.sh` (client/informer/lister/deepcopy), `generate-groups.sh`, `generate-internal-groups.sh`
- **Vendoring**: always keep `vendor/` in sync when changing dependencies; use `make mod-download-go` to refresh
- **Feature gates**: defined in `pkg/features/`, accessed via `utilfeature` from k8s.io/apiserver
- **Version info** injected at build time via ldflags into `pkg/version/` (GitSHA, Build date, Version)