# Code Walkthrough

This document orients you in the codebase so you can find the code behind any behavior you see in a running driver. Open the repo alongside this guide.

## Package Map

| Package | Files | Role |
|---------|-------|------|
| `cmd/driver` | 1 | Driver entry point — config, clients, plugin registration |
| `cmd/webhook` | 1 | Webhook entry point — TLS server, `/mutate` endpoint |
| `pkg/synthesizer` | 5 + 4 test | Watch → Pair → Publish pipeline |
| `pkg/plugin` | 3 + 0 test | DRA plugin interface (Prepare/Unprepare), gRPC client, orphan reconciler |
| `pkg/shadow` | 2 + 1 test | Shadow claim CRUD, opaque device params resolver |
| `pkg/store` | 2 + 2 test | `DeviceStore` (in-memory), `StateStore` (BoltDB) |
| `pkg/config` | 3 + 1 test | Config types, YAML loading, validation |
| `pkg/webhook` | 4 + 3 test | HTTP handler, pod mutator, claim builder, template reconciler |

## Two Entry Points

### cmd/driver/main.go

The driver initializes in this order:

1. Parse flags (`--config`, `--node-name`, `--plugin-dir`, `--state-dir`)
2. `config.LoadFromFile()` → `config.Validate()`
3. Build Kubernetes client (QPS=100, Burst=200)
4. `store.NewStateStore()` — BoltDB at `stateDir/state.db`
5. `store.NewDeviceStore()` — in-memory device mappings
6. `shadow.NewClaimManager()` — API client for creating shadow claims
7. Optionally `shadow.NewDeviceParamsResolver()` if `cfg.DeviceParams` is set
8. `plugin.NewCompositePlugin()` — wires all dependencies together
9. `kubeletplugin.Start()` — registers gRPC socket with kubelet
10. `synthesizer.New()` → `Start()` in a goroutine — begins watching and publishing
11. `plugin.StartReconciler()` in a goroutine — orphan cleanup every 5 minutes
12. Block on `<-ctx.Done()`

### cmd/webhook/main.go

1. Parse flags (`--port`, `--tls-cert`, `--tls-key`, `--resource-mapping`)
2. Build Kubernetes client
3. `webhook.NewMutator()` with resource name → DeviceClass mappings
4. Register `/mutate` and `/healthz` HTTP handlers
5. Start TLS server (TLS 1.2 min, 10s timeouts)
6. `webhook.StartTemplateReconciler()` in a goroutine — cleans up stale templates
7. Block on signal, graceful shutdown

## Synthesizer Pipeline — Reading Order

The synthesizer is the driver's main loop. Read these four files in order:

### 1. watcher.go — "What changed?"

`Watcher` creates a SharedInformerFactory watching all ResourceSlices. Only slices from configured source drivers on this node trigger the callback.

Key detail: a **500ms debounce timer** prevents cascading recomputation when multiple ResourceSlices update within a short window. After 500ms of quiet, `onChange()` fires once.

The `onChange` callback is `synthesizer.recompute()` — which runs the full pipeline.

### 2. pairer.go — "Which devices go together?"

`ComputePairs()` iterates all configured compositions and routes each to one of three strategies:

- **`pairWithMatchAttribute()`** — groups devices across sources by a shared attribute value (e.g., `pcieRoot`). Within each group, generates C(n,k) combinations. This is the default for bare-metal clusters where PCIe topology is visible.

- **`pairWithExplicit()`** — matches node labels to find the right `ExplicitNodePool`, then evaluates admin-defined CEL selectors against available devices. Tracks consumed devices to prevent double-allocation. Sets rail index and NUMA node from the pair config.

- **`pairWithoutConstraints()`** — round-robin pairing when no topology constraints exist.

`buildCompositeDevice()` takes selected devices and produces a `CompositeDevice`:
- Forwards attributes from each source device, prefixed with `sourceName/attrName`
- Generates a deterministic name by joining sanitized device names with `--` (truncated to 63 chars)
- Detects NUMA node from device attributes
- Adds `composite/compositionName` as a metadata attribute

### 3. cel.go — "Does this device match?"

`CELFilter` compiles and caches CEL programs. The `Match()` function evaluates an expression against a device's attributes, returning a boolean.

Attributes are flattened into a nested map: `{"attributes": {"dra.net": {"rdma": true, "ipv4": "10.0.0.5"}}}`. The compiled program is cached by expression string — each unique CEL expression compiles once.

### 4. publisher.go — "Push to the API server"

`Publish()` groups composite devices by composition name, then chunks each group into ResourceSlices of at most **128 devices** (the Kubernetes limit). Empty pools still produce a slice so the driver stays registered.

The actual API call is `kubeletplugin.Helper.PublishResources()`, injected as a callback at startup.

## Plugin — The Prepare Path

When kubelet calls `NodePrepareResources`, the composite plugin's `prepareClaim()` runs a **two-phase fan-out**:

**Phase 1: Create shadow claims in parallel**

For each member in the composite device's mapping, a goroutine calls `claimMgr.Create()`. Each goroutine writes to its own `memberWork` struct — no shared mutation, no locks during the fan-out. After `wg.Wait()`, results are collected sequentially.

If any creation fails, all already-created shadows are cleaned up before returning the error.

**Phase 2: gRPC prepare on underlying drivers in parallel**

Another fan-out: each goroutine calls `grpcClient.Prepare()` on the underlying driver's socket at `/var/lib/kubelet/plugins/<driverName>/dra.sock`. CDI device IDs from responses are collected per composite device.

After both phases, shadow records are stored in-memory and persisted to BoltDB.

`unprepareClaim()` reverses the process: gRPC unprepare calls, shadow claim deletion, BoltDB cleanup. Falls back to label-based bulk delete if in-memory records are missing (crash recovery path).

## Webhook — The Mutate Path

`Mutate()` in `pkg/webhook/mutator.go`:

1. Skip pods with `composite.dra/mutated` annotation (idempotency guard)
2. Scan all containers for resource names matching configured mappings (e.g., `composite.dra/gpu-nic-pair`)
3. For each match: generate a deterministic template name, call `BuildClaimSpec()` to create N device requests, create a `ResourceClaimTemplate` via the API
4. Build JSON Patch operations that:
   - Add the mutated annotation
   - Remove synthetic resources from container requests/limits
   - Add `PodResourceClaim` entries referencing templates
   - Add per-container `ResourceClaim` references (`pair-0`, `pair-1`, ...)

`BuildClaimSpec()` in `claim_builder.go` generates N `DeviceRequest` entries, each requesting exactly 1 device from the target DeviceClass with `ExactCount` allocation mode.

The `TemplateReconciler` runs on a timer and deletes orphaned `ResourceClaimTemplates` that no pod references, respecting a grace period.

## Config and Store

**`pkg/config/types.go`** defines the full config schema. Key types:
- `CompositeConfig` — top-level (driver, sources, compositions, deviceParams)
- `SourceConfig` — an underlying driver (name, driver, DeviceClassName, forwarded attributes, socket path)
- `CompositionConfig` — a pairing rule (members, constraints, filters, pairing mode, node pools)
- `ExplicitPairConfig` — a single admin-defined pair (CEL selectors per source, rail index, NUMA node)

**`pkg/store/device_store.go`** — thread-safe (`sync.RWMutex`) map from `"pool/device"` to `*DeviceMapping`. `ReplaceAll()` atomically swaps the entire map on each synthesizer recompute.

**`pkg/store/state.go`** — BoltDB wrapper. Each shadow claim set is stored as JSON keyed by the composite claim's UID. Survives driver restarts for crash recovery.

## Testing

10 test files, 103 test functions total:

| File | Tests | What it covers |
|------|-------|----------------|
| `config/validation_test.go` | 29 | Config validation: missing fields, duplicates, pairing mode rules |
| `synthesizer/pairer_test.go` | 17 | Pairing: match-attribute, explicit, multi-root, combinations, attribute forwarding |
| `shadow/params_test.go` | 14 | Opaque params: matching, overrides, Go template functions |
| `synthesizer/cel_test.go` | 7 | CEL: type matching, caching, error handling |
| `webhook/mutator_test.go` | 7 | Mutation: skip/match/multi-resource, idempotency |
| `webhook/reconciler_test.go` | 7 | Template cleanup: orphans, grace period, multi-namespace |
| `store/device_store_test.go` | 6 | Thread-safe CRUD, atomic replace |
| `store/state_test.go` | 6 | BoltDB round-trip, persistence across restarts |
| `synthesizer/publisher_test.go` | 6 | 128-device splitting, pool naming, empty input |
| `webhook/claim_builder_test.go` | 4 | Claim spec generation, JSON patch escaping |

Start with **`pairer_test.go`** — it is the most instructive test file for understanding how devices get composed. The `TestPairWithMatchAttribute` test shows the full flow from source devices to composite devices.

## Exercise 3: Run Tests and Read a Test

**Time:** ~10 minutes

**Step 1:** Run all tests.

```bash
go test ./... -v
```

All should pass. If any fail, check Go version (`go version` — needs 1.26+).

**Step 2:** Open `pkg/synthesizer/pairer_test.go` and read `TestPairWithMatchAttribute`.

Trace how the test:
- Creates source devices with attributes including a `pcieRoot` value
- Configures a composition with a `matchAttribute` constraint on `pcieRoot`
- Calls `ComputePairs()` and verifies the pairer grouped devices sharing the same root

**Step 3:** Open `pkg/shadow/params_test.go` and read `TestResolveBasicPrefix`.

Trace how the test:
- Creates a params YAML with attribute matchers and Go template values
- Calls `ResolveForDevice()` with device attributes
- Verifies the resolver produces the correct opaque config bytes

---

**Next:** [06-contributing.md](06-contributing.md) — build, test, what to work on

**Previous:** [04-shadow-claims.md](04-shadow-claims.md) — the core mechanism
