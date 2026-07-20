# Composite DRA Driver — Status Report

**Date:** 2026-05-31 (point-in-time snapshot — all phases now complete)
**Session:** composite-dra-driver

## Goal

Replace the dra-rail-admission-webhook's scheduler bypass (node pinning, out-of-band allocation) with a proper DRA driver that presents composite devices (e.g., GPU-NIC pairs) to the K8s scheduler natively. Generic, config-driven — not hardcoded to GPU+NIC.

## Architecture: Shadow Claims Pattern

The composite driver (`composite.dra.llm-d.io`) publishes ResourceSlices where each device represents a pre-validated grouping of underlying devices. On `PrepareResourceClaims`, it creates real "shadow" ResourceClaims in the API server for each underlying driver and calls their gRPC sockets directly.

```
Scheduler allocates composite device
  → Kubelet calls composite driver Prepare
    → Create shadow claim for gpu.nvidia.com (pre-filled allocation)
    → gRPC to nvidia socket → nvidia generates CDI specs, returns CDI IDs
    → Create shadow claim for dra.net (pre-filled allocation + opaque params from external ConfigMap)
    → gRPC to dranet socket → dranet stores NIC config in PodConfigStore
    → Return combined CDI IDs to kubelet
      → Container runtime applies CDI specs (GPU visible)
      → NRI RunPodSandbox hook fires → dranet moves NIC to pod netns
```

## Validation Results

### Shadow Claims: FEASIBLE ✅

Traced both drivers' Prepare codepaths and the kubeletplugin.Helper library. No blockers.

### dranet (dra.net)

| Check | Code Location | Result |
|-------|--------------|--------|
| Pod spec reference validation | — | **Not performed anywhere** |
| ReservedFor check | `dra_hooks.go:149-159` | Checks ReservedFor has pod UID — shadow claim satisfies this ✅ |
| Driver name filter | `dra_hooks.go:185` | Filters `result.Driver == "dra.net"` — shadow claim matches ✅ |
| PodConfigStore key | `pod_device_config.go:189` | Keyed by `(podUID, deviceName)` — shadow claim stored correctly ✅ |
| NRI hook discovery | `nri_hooks.go:136` | `GetPodConfig(podUID)` — finds shadow claim config ✅ |
| NIC netns move | `nri_hooks.go:180` | `attachNetdevToNS()` — executes normally ✅ |
| RDMA attach | `nri_hooks.go:188` | `attachRdmaToNS()` — executes normally ✅ |
| Claim status update | `nri_hooks.go:211-225` | `ApplyStatus()` — updates shadow claim ✅ |
| BoltDB persistence | `pod_device_config_bolt.go:78-98` | Persisted under podUID bucket ✅ |

### nvidia GPU driver (gpu.nvidia.com)

| Check | Code Location | Result |
|-------|--------------|--------|
| Pod spec reference validation | — | **Not performed anywhere** |
| kubeletplugin.Helper validation | `draplugin.go:1100-1116` | Checks: exists, allocated, UID stable. No pod spec check ✅ |
| Device existence/health | `device_state.go:637-645` | Validates device is allocatable — shadow claim satisfies ✅ |
| Overlapping device check | `device_state.go:1118-1154` | Only checks against other prepared claims, not pod spec ✅ |
| CDI spec generation | `cdi.go:194-304` | Generated on-the-fly per claim UID. Written to `/var/run/cdi/` ✅ |
| Checkpoint storage | `device_state.go:232` | Keyed by claim UID ✅ |
| ReservedFor validation | — | **Neither driver nor helper validates it** |

### kubeletplugin.Helper (k8s.io/dynamic-resource-allocation)

The helper that wraps gRPC → driver calls performs exactly three checks (`draplugin.go:1100-1116`):
1. Claim exists in API server
2. `claim.Status.Allocation != nil`
3. `claim.UID == request.UID` (not replaced)

**No pod spec reference check. No ReservedFor validation.** Shadow claims that exist and are allocated pass all checks.

### Cleanup Concern

Kubelet only calls `UnprepareResourceClaims` for claims in `pod.spec.resourceClaims`. Shadow claims are not there, so the composite driver must handle cleanup itself:
- On its own Unprepare: call underlying drivers' `NodeUnprepareResources` with shadow claims, then delete them
- OwnerReferences on shadow claims → composite claim ensures GC on crash

## Council Review (Codex + Gemini)

Reviewed full architecture proposal. Key findings:
- **No blockers** — approach is architecturally sound
- **Do NOT reimplement underlying driver logic** — delegate via shadow claims + gRPC (confirmed by validation)
- **Attribute limits safe** — ~25 attributes per composite device, well under 32 limit
- **Race windows self-healing** — stale composite device → Prepare failure → reschedule
- **NUMA bin-packing loss acceptable** — add custom scheduler plugin later
- **Scheduler handles multi-request MatchAttribute constraints natively**

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Approach | Full composite DRA driver | User choice — not thin webhook |
| Scope | Generic composability | Config-driven, any source drivers |
| NUMA packing | Deferred | Nice-to-have, future scheduler plugin |
| Per-rail NIC config | Required | Embedded in shadow claim opaque params |
| Prepare delegation | Shadow claims + gRPC passthrough | Validated against both drivers |
| State persistence | BoltDB | Crash recovery for shadow claim tracking |

## Phase 1: Complete ✅

All code compiles, 14 unit tests passing.

### Files Created

```
composite-dra-driver/
  cmd/driver/main.go                    — DaemonSet entry point
  pkg/config/types.go                   — Config structs (generic, config-driven)
  pkg/config/loader.go                  — YAML/ConfigMap loading
  pkg/config/validation.go              — Config validation
  pkg/config/validation_test.go         — 10 validation tests
  pkg/synthesizer/synthesizer.go        — Orchestrator: watcher→pairer→publisher→store
  pkg/synthesizer/watcher.go            — ResourceSlice informer + debounced recomputation
  pkg/synthesizer/pairer.go             — Generic matchAttribute-based pairing algorithm
  pkg/synthesizer/pairer_test.go        — 7 pairing tests (basic, mismatch, multi-root, etc.)
  pkg/synthesizer/publisher.go          — Composite ResourceSlice building (128-device splitting)
  pkg/synthesizer/publisher_test.go     — 4 publisher tests (empty, single, split, pool name)
  pkg/store/device_store.go             — Thread-safe composite→underlying device mapping
  pkg/store/device_store_test.go        — 5 store tests
  go.mod / go.sum                       — K8s 1.35 APIs (k8s.io/api@v0.35.3)
```

### What Works

- Config parsing + validation for arbitrary driver compositions
- matchAttribute-based device pairing (groups devices by shared attribute, generates valid combinations)
- ResourceSlice spec building with forwarded attributes (prefixed by source name)
- Automatic slice splitting at 128-device limit
- DeviceStore maintains composite→underlying device mappings
- Informer-based ResourceSlice watching with 500ms debounce

### What's Stubbed (Phase 2)

All Phase 2 items are now implemented. See Phase 2 status below.

## Phase 2: Complete ✅

All components implemented and validated on hardware (poseidon cluster, 8×H100 + RDMA NICs).

| Component | Status |
|-----------|--------|
| `pkg/plugin/plugin.go` — DRAPlugin (Prepare/Unprepare) | ✅ Implemented |
| `pkg/plugin/grpc_client.go` — gRPC to underlying drivers | ✅ Implemented |
| `pkg/shadow/claims.go` — Shadow claim CRUD | ✅ Implemented |
| `pkg/shadow/params.go` — Device params resolver | ✅ Implemented |
| `pkg/store/state.go` — BoltDB persistence | ✅ Implemented |
| `pkg/synthesizer/cel.go` — CEL filter evaluation | ✅ Implemented |
| `kubeletplugin.Start()` wiring | ✅ Implemented |

## Phase 3: Observability — Complete ✅

| Component | Status |
|-----------|--------|
| `pkg/metrics/metrics.go` — 16 Prometheus metrics | ✅ Implemented |
| `/metrics` endpoint (port 8080) on driver + webhook | ✅ Implemented |
| K8s Events on ResourceClaims (Prepare/Unprepare lifecycle) | ✅ Implemented |
| Structured logging (klog.InfoS/ErrorS) across all packages | ✅ Implemented |
| Helm ServiceMonitor + driver Service | ✅ Implemented |
| Validated on OpenShift (poseidon) with user workload monitoring | ✅ Verified |

## Key Reference Files

| File | Why |
|------|-----|
| `refs_repos/kubernetes/.../kubeletplugin/draplugin.go` | DRAPlugin interface + Helper (shadow claim validation traced here) |
| `refs_repos/dranet/pkg/driver/dra_hooks.go` | dranet Prepare flow (shadow claim compatibility traced here) |
| `refs_repos/dranet/pkg/driver/nri_hooks.go` | NRI RunPodSandbox (config discovery by podUID traced here) |
| `refs_repos/dranet/pkg/driver/pod_device_config.go` | PodConfigStore keying (podUID, deviceName) |
| `nvidia-dra-driver-gpu/.../device_state.go` | nvidia Prepare + checkpoint (claim UID keying traced here) |
| `nvidia-dra-driver-gpu/.../cdi.go` | CDI spec generation (on-the-fly, per claim UID) |
| `refs_repos/dra-rail-admission-webhook/internal/webhook/allocator.go` | Existing pairing logic (reference for synthesizer) |
