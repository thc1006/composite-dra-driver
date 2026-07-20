# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
make build              # Build both driver and webhook binaries
make build-driver       # Build composite-dra-driver only
make build-webhook      # Build composite-dra-webhook only
make test               # Run all tests (go test ./... -v)
make lint               # Run go vet ./...
make mod-tidy           # go mod tidy
make image              # Build both container images (podman)
make deploy             # kubectl apply all manifests in deploy/

# Single package test
go test ./pkg/synthesizer/... -v -run TestPairDevices

# CI runs with race detector
go test ./... -v -race
```

Go version: 1.26. Module: `github.com/openshift-psap/composite-dra-driver`.

## Architecture

This is a Kubernetes **Dynamic Resource Allocation (DRA)** driver that composes devices from multiple underlying DRA drivers (e.g. GPU + NIC) into single allocatable units. Two binaries:

**Driver** (`cmd/driver/main.go`) — DaemonSet on every node. Registers as a kubelet DRA plugin via gRPC socket. Pipeline:

1. **Synthesizer** watches underlying drivers' ResourceSlices via informers
2. **Pairer** groups devices across sources using `matchAttribute` constraints (e.g. same PCIe root), CEL filters, or explicit CEL-based pairs per MachineConfigPool
3. **Publisher** builds composite ResourceSlices (splits at 128-device K8s limit) and publishes via kubelet helper
4. **DeviceStore** maps composite device names → underlying device members (thread-safe, in-memory)
5. On allocation, **CompositePlugin.Prepare** creates **shadow ResourceClaims** for each underlying driver with pre-filled allocation, then calls each driver's gRPC `NodePrepareResources`
6. **StateStore** (BoltDB) persists shadow claim records for crash recovery
7. **Reconciler** (5-min loop) garbage-collects orphaned shadow claims

**Webhook** (`cmd/webhook/main.go`) — Mutating admission webhook. Supports multiple resource-to-DeviceClass mappings via repeatable `--resource-mapping` flag (old `--device-class`/`--resource-name` flags deprecated). Intercepts synthetic resource requests from pod containers, strips them from requests/limits, generates one ResourceClaimTemplate per composition type with N device pair requests, and patches the pod spec with claim refs. Handles `generateName` pods by stripping trailing `-` for template naming. Runs a **template reconciler** (`pkg/webhook/reconciler.go`) that periodically GCs orphaned ResourceClaimTemplates whose owning pod no longer exists (configurable via `--reconcile-interval` and `--reconcile-grace-period`).

## Key Packages

| Package | Role |
|---------|------|
| `pkg/plugin` | DRA plugin (Prepare/Unprepare), gRPC client to underlying drivers, orphan reconciler, K8s Events |
| `pkg/synthesizer` | Watcher → Pairer → Publisher pipeline, CEL filter evaluation with compiled program caching |
| `pkg/shadow` | Shadow claim CRUD (`ClaimManager`), external device params resolver |
| `pkg/store` | `DeviceStore` (in-memory device mappings), `StateStore` (BoltDB persistence) |
| `pkg/config` | Config types, YAML loading, validation |
| `pkg/webhook` | HTTP handler, pod mutator, claim builder, template reconciler |
| `pkg/metrics` | Prometheus metric definitions (composition-level gauges, histograms, counters) |

## Shadow Claims Pattern

Core design: the composite driver doesn't implement device logic. Instead:
- Prepare creates real K8s ResourceClaims ("shadows") with pre-filled allocation pointing to underlying drivers
- Calls each underlying driver via gRPC at `/var/lib/kubelet/plugins/<driver>/dra.sock`
- Combines CDI device IDs from all underlying drivers
- Shadow claims have OwnerReference → composite claim for GC cascade
- Unprepare reverses: gRPC unprepare calls, then deletes shadow claims

## Configuration

Config YAML (`/etc/composite-dra/config.yaml` in-cluster) defines:
- `driver.name` — composite driver name (e.g. `composite.dra.llm-d.io`)
- `sources[]` — underlying drivers with forwarded attributes
- `sources[].socketPath` — optional custom gRPC socket path for an underlying driver
- `compositions[]` — pairing rules: which sources, member counts, matchAttribute constraints, CEL filters
- `compositions[].pairingMode` — `auto` (default, attribute-based) or `explicit` (CEL selectors per MachineConfigPool)
- `compositions[].transportMode` — `auto` (default), `ethernet`, or `infiniband`
- `compositions[].deviceClassName` / `extendedResourceName` — per-composition overrides for DeviceClass and extended resource name
- `compositions[].nodePoolLabelKey` + `nodePools[]` — explicit mode: group CEL-based device pairs by node pool label value
- `deviceParams` — references an external ConfigMap providing opaque driver params for shadow claims (replaces the old NIC-specific `railConfig`)

**Validation**: auto pairing with multiple unique sources requires at least one constraint (`matchAttribute` or CEL filter).

## Pairing Modes

**Auto** (`pairingMode: auto`) — devices grouped by shared attribute values (e.g. `pcieRoot`). Works on bare-metal/IBM where PCIe topology is visible.

**Explicit** (`pairingMode: explicit`) — admin specifies exact device pairs using CEL selectors per MachineConfigPool label. Required on AKS/cloud VMs where `pcieRoot` isn't exposed. Each pair maps source names to CEL expressions matching device attributes (e.g. `pciBusID`, `pciAddress`). Consumed-device tracking prevents double-allocation. Rail and NUMA are set explicitly per pair.

## Deployment

- DaemonSet runs as `privileged` on all nodes (including control-plane), priority `system-node-critical`
- Mounts kubelet plugin dirs and BoltDB state dir as hostPath volumes
- Config delivered via ConfigMap
- DeviceClass manifest tells scheduler about the composite driver
- Helm chart in `charts/composite-dra-driver/` with values files (`values.yaml`, `values-poseidon.yaml`, `values-b200-pf.yaml`)
- `webhook.mode`: `auto` (default, skips webhook on K8s >=1.36 with DRAExtendedResource), `enabled`, `disabled`
- TLS modes: `cert-manager` (default), `helm-generated`, `manual`
- `metrics.*` values for Prometheus endpoint and ServiceMonitor CRs (driver + webhook)
- Example device params configs in `charts/composite-dra-driver/examples/`
- OpenShift: SCC manifest via `openshift.scc.enabled`

## Testing

Table-driven unit tests. Helpers in pairer_test.go (`strAttr`, `intAttr`, `boolAttr`) for building device attributes. No integration tests — those require a K8s cluster with DRA feature gate. Test files: config validation, pairer algorithm, device store thread-safety, publisher splitting, device params resolver, webhook claim builder, webhook reconciler.

### Cluster Testing

```bash
export KUBECONFIG=<path-to-cluster-kubeconfig>

# Deploy via Helm
helm install composite charts/composite-dra-driver \
  -n composite-dra-system \
  -f charts/composite-dra-driver/values-poseidon.yaml \
  --set webhook.enabled=true \
  --set webhook.tls.certManager.issuerRef.name=composite-dra-selfsigned

# Label namespace for webhook
oc label ns <ns> composite.dra/webhook-enabled=true

# Test pod
oc apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test
  namespace: <ns>
spec:
  containers:
  - name: test
    image: quay.io/dagray/rdma-tools:tiny
    command: ["sleep", "300"]
    resources:
      requests:
        composite.dra/gpu-nic-pair: "2"
      limits:
        composite.dra/gpu-nic-pair: "2"
EOF

# Verify
oc get resourceclaims -n <ns>                     # shadow claims visible
oc exec test -- ip -br addr                        # net0, net1 with IPs
oc exec test -- rdma link show                     # mlx5 devices
oc logs -l app.kubernetes.io/component=driver \
  -n composite-dra-system | grep "plugin:"         # prepare timing
```

## Observability

Both binaries serve `/metrics` on port 8080 (configurable via `--metrics-port`).

16 Prometheus metrics under `composite_dra` namespace:
- **Gauges**: `synthesis_devices_total`, `claims_active`, `shadow_claims_active` (by composition)
- **Histograms**: `synthesis_duration_seconds`, `prepare_duration_seconds`, `prepare_shadow_create_duration_seconds`, `prepare_grpc_duration_seconds`, `webhook_duration_seconds`
- **Counters**: `reconciler_claims_cleaned_total`, `grpc_errors_total`, `device_params_errors_total`, `webhook_mutations_total`, `webhook_skipped_total`, `webhook_errors_total`, `webhook_templates_created_total`, `webhook_reconciler_templates_cleaned_total`

K8s Events emitted on ResourceClaims: `PrepareStarted` (Normal), `PrepareCompleted` (Normal), `PrepareFailed` (Warning), `UnprepareCompleted` (Normal).

Structured logging via `klog.InfoS`/`ErrorS` with key-value pairs throughout. See `docs/OBSERVABILITY.md` for full metric catalog and PromQL examples.

## CI

GitHub Actions workflows in `.github/workflows/`:

| Workflow | Trigger | What |
|----------|---------|------|
| `ci.yaml` | PR to main | `go vet`, `go test -race`, `go build`, 16 Helm template assertion tests |
| `build-push.yaml` | Push to main | Images to `ghcr.io/openshift-psap/composite-dra-{driver,webhook}` (`sha-*` + `latest`) |
| `build-pr.yaml` | PR (maintainers) | `pr-<N>` tagged images |
| `nightly.yaml` | Cron 04:12 UTC | `nightly-main-MM-DD-YY` + `nightly-main-latest` tags |

CI Helm tests cover: webhook auto/enabled/disabled modes, K8s version-conditional behavior, matchConditions CEL, namespace exclusion, TLS modes, backwards-compat guards.

## Docs

Reference docs in `docs/`:

| File | Content |
|------|---------|
| `ARCHITECTURE.md` | Detailed architecture reference |
| `OBSERVABILITY.md` | Full metrics catalog, PromQL examples, scraping setup |
| `CHEATSHEET.md` | Quick command reference |
| `FAQ.md` | Frequently asked questions |
| `HA-DESIGN.md` | High-availability design considerations |
| `STATUS.md` | Project status (Phase 1-3 complete) |

## References

- [kubernetes/kubernetes](https://github.com/kubernetes/kubernetes) — kubeletplugin at `staging/src/k8s.io/dynamic-resource-allocation/`, DRA APIs at `staging/src/k8s.io/api/resource/v1/`
- [kubernetes-sigs/dranet](https://github.com/kubernetes-sigs/dranet) — dranet DRA driver (NRI hooks, PodConfigStore)
- [openshift-psap/dra-rail-admission-webhook](https://github.com/openshift-psap/dra-rail-admission-webhook) — old webhook (reference for VF/IPAM implementation)
- [kubernetes-sigs/dra-driver-nvidia-gpu](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) — nvidia DRA driver (CDI, checkpoint)
