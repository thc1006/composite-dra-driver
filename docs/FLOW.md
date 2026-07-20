# Composite DRA Driver — Operation Flow

Full operation flow for `gpu-nic-pair: 4`.

> github.com/openshift-psap/composite-dra-driver

---

## Stages Overview

| Stage | Name | Description |
|-------|------|-------------|
| 1 | **Setup** | Cluster admin deploys driver + DeviceClass |
| 2 | **Synthesis** | DaemonSet watches underlying drivers, publishes composite devices |
| 3 | **Submit** | User writes `gpu-nic-pair: 4` in pod spec |
| 4 | **Schedule** | Scheduler auto-creates claim, allocates 4 composite devices |
| 5 | **Prepare** | Composite driver creates shadow claims, delegates via gRPC |
| 6 | **Runtime** | CDI specs + NRI hooks configure GPUs + NICs in pod |
| 7 | **Cleanup** | Pod deletion cascades through shadow claims |

---

## Stage 1 — Cluster Setup

One-time admin configuration.

### Deploy Composite Driver DaemonSet

- Runs on **all nodes** including control-plane (tolerations)
- `priorityClassName: system-node-critical`
- Registers kubelet plugin at `/var/lib/kubelet/plugins/composite.dra.example.io/dra.sock`
- Mounts: kubelet plugins dir, CDI specs dir, state dir

### Create DeviceClass

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: composite-gpu-nic
spec:
  selectors:
  - cel:
      expression: 'device.driver == "composite.dra.example.io"'
  # K8s 1.36+ (DRAExtendedResource beta) — enables extended resource UX
  extendedResourceName: "composite.dra.example.io/gpu-nic-pair"
```

### ConfigMap — Driver Configuration

```yaml
sources:
  - name: gpu
    driver: "gpu.nvidia.com"
    forwardAttributes:
      - domain: "resource.kubernetes.io"
        attributes: ["pciBusID", "pcieRoot"]
  - name: nic
    driver: "dra.net"
    forwardAttributes:
      - domain: "dra.net"
        attributes: ["pciAddress", "numaNode", "rdma", "ipv4"]

compositions:
  - name: "gpu-nic-pair"
    pairingMode: auto          # default; or "explicit" for CEL-based per-MachineConfigPool pairing
    members: [{source: gpu, count: 1}, {source: nic, count: 1}]
    constraints:
      - type: matchAttribute
        attribute: "resource.kubernetes.io/pcieRoot"

# Generic device params — references an external ConfigMap maintained by admin/operator.
# The composite driver never interprets param content; it resolves templates and passes
# opaque params to shadow claims. See charts/composite-dra-driver/examples/ for templates.
deviceParams:
  configMapName: composite-device-params
  configMapNamespace: composite-dra-system
```

> **Note**: Auto pairing with multiple unique sources requires at least one constraint (`matchAttribute` or CEL filter). Explicit pairing mode (`pairingMode: explicit`) allows admin-defined CEL selectors per MachineConfigPool — required on cloud VMs where PCIe topology isn't exposed.

---

## Stage 2 — Device Synthesis

Continuous — runs on every node.

### Watch Underlying ResourceSlices

**gpu.nvidia.com:**
```
gpu-0: pcieRoot=root-0, numa=0
gpu-1: pcieRoot=root-1, numa=0
gpu-2: pcieRoot=root-2, numa=1
gpu-3: pcieRoot=root-3, numa=1
```

**dra.net:**
```
nic-0: pcieRoot=root-0, ip=10.0.0.5, rdma=✓
nic-1: pcieRoot=root-1, ip=10.0.1.7, rdma=✓
nic-2: pcieRoot=root-2, ip=10.0.2.3, rdma=✓
nic-3: pcieRoot=root-3, ip=10.0.3.9, rdma=✓
```

↓ informer watch + 500ms debounce

### Pair by PCIe Root Topology

```
Pairer groups devices by matchAttribute: "resource.kubernetes.io/pcieRoot"

  root-0 → gpu-0 + nic-0  →  composite device "gpu-0--nic-0"
  root-1 → gpu-1 + nic-1  →  composite device "gpu-1--nic-1"
  root-2 → gpu-2 + nic-2  →  composite device "gpu-2--nic-2"
  root-3 → gpu-3 + nic-3  →  composite device "gpu-3--nic-3"
```

### Publish Composite ResourceSlice

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
spec:
  driver: composite.dra.example.io
  nodeName: worker-1
  pool: {name: composite.dra.example.io-worker-1-gpu-nic-pair}
  devices:
  - name: gpu-0--nic-0
    attributes:
      resource.kubernetes.io/pcieRoot: "root-0"
      composite/numaNode: 0
      gpu/pciBusID: "0000:0c:00.0"
      nic/ipv4: "10.0.0.5"
      nic/rdma: true
  - name: gpu-1--nic-1
    attributes:
      resource.kubernetes.io/pcieRoot: "root-1"
      composite/numaNode: 0
      nic/ipv4: "10.0.1.7"
      # ...
  # ... gpu-2--nic-2, gpu-3--nic-3
```

**DeviceStore** records mapping: `gpu-0--nic-0 → {nvidia/gpu-pool/gpu-0, dranet/nic-pool/nic-0}`

---

## Stage 3 — Pod Submission

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llm-worker
spec:
  replicas: 8
  template:
    spec:
      containers:
      - name: worker
        image: llm-inference:latest
        resources:
          requests:
            composite.dra.example.io/gpu-nic-pair: "4"  # ← this is all the user writes
```

No webhook. No ResourceClaimTemplate. No annotations. `DRAExtendedResource` feature gate handles the translation.

---

## Stage 4 — Scheduler Allocation

Fully native — no webhook bypass.

### Extended Resource → ResourceClaim

`preFilterExtendedResources()` detects `gpu-nic-pair: 4`, looks up DeviceClass with matching `extendedResourceName`, builds in-memory ResourceClaim:

```yaml
spec:
  devices:
    requests:
    - name: container-0-request-0
      exactly:
        deviceClassName: composite-gpu-nic
        allocationMode: ExactCount
        count: 4
```

### Node Filter + Allocate

```
For each candidate node:
  ├─ filterExtendedResources() → does node have ≥4 composite devices?
  ├─ DRA allocator matches devices via CEL selectors
  ├─ Picks 4 devices from composite ResourceSlice
  └─ Scores node (MostAllocated / LeastAllocated / spread)

Selected: worker-1 with devices gpu-0--nic-0, gpu-1--nic-1, gpu-2--nic-2, gpu-3--nic-3
```

### PreBind — Persist Claim

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: llm-worker-abc-extended-resources-xyz
  ownerReferences: [{kind: Pod, name: llm-worker-abc}]
status:
  allocation:
    devices:
      results:
      - request: container-0-request-0
        driver: composite.dra.example.io
        pool: composite.dra.example.io-worker-1-gpu-nic-pair
        device: gpu-0--nic-0
      - {driver: composite.dra.example.io, device: gpu-1--nic-1}
      - {driver: composite.dra.example.io, device: gpu-2--nic-2}
      - {driver: composite.dra.example.io, device: gpu-3--nic-3}
  reservedFor:
  - uid: <pod-uid>
```

Pod bound to worker-1.

---

## Stage 5 — Prepare (Shadow Claims)

Kubelet → composite driver → underlying drivers via gRPC.

### Kubelet Calls Composite Driver

```
kubelet
  → NodePrepareResources(claim: llm-worker-abc-extended-resources-xyz)
    → kubeletplugin.Helper:
        Fetches claim from API server  ✓
        Verifies allocated             ✓
        Verifies UID                   ✓
    → CompositePlugin.PrepareResourceClaims()
        Iterates 4 allocation results
```

### Per Device: GPU Shadow Claim

× 4 devices — showing gpu-0--nic-0:

```
1. DeviceStore lookup: gpu-0--nic-0
   → members: [{nvidia, gpu-pool, gpu-0}, {dranet, nic-pool, nic-0}]

2. Create shadow claim "shadow-...-gpu-gpu-0" in API server:
   status.allocation.results: [{driver: gpu.nvidia.com, pool: gpu-pool, device: gpu-0}]
   status.reservedFor: [{uid: <pod-uid>}]
   ownerReference → composite claim

3. gRPC → /var/lib/kubelet/plugins/gpu.nvidia.com/dra.sock
   NodePrepareResources({claim: shadow-gpu-gpu-0})

4. nvidia driver:
   ├─ Helper fetches shadow claim from API server  ✓
   ├─ Finds gpu-0 in allocatable devices           ✓
   ├─ Generates CDI spec → /var/run/cdi/
   └─ Returns CDI: ["nvidia.com/gpu=0"]
```

### Per Device: NIC Shadow Claim

```
5. DeviceParamsResolver: matches device attributes against ConfigMap entries
   → generates opaque params for shadow claim via Go template substitution

6. Create shadow claim "shadow-...-nic-nic-0" in API server:
   status.allocation.results: [{driver: dra.net, pool: nic-pool, device: nic-0}]
   status.allocation.config: [{opaque: {driver: dra.net, params: <device params>}}]
   status.reservedFor: [{uid: <pod-uid>}]

7. gRPC → /var/lib/kubelet/plugins/dra.net/dra.sock
   NodePrepareResources({claim: shadow-nic-nic-0})

8. dranet driver:
   ├─ Reads ReservedFor → pod UID                  ✓
   ├─ Reads allocation → device nic-0              ✓
   ├─ Reads opaque config → MTU, routing rules
   ├─ Discovers hardware: PCI addr, RDMA, MAC
   ├─ Stores in PodConfigStore[pod-uid][nic-0]
   └─ Persists to BoltDB checkpoint
```

### Return to Kubelet

```
9. Persist shadow mapping to BoltDB (crash recovery)

10. Return PrepareResult to kubelet:
    Devices:
      gpu-0--nic-0 → CDI: ["nvidia.com/gpu=0"]
      gpu-1--nic-1 → CDI: ["nvidia.com/gpu=1"]
      gpu-2--nic-2 → CDI: ["nvidia.com/gpu=2"]
      gpu-3--nic-3 → CDI: ["nvidia.com/gpu=3"]
```

8 shadow claims created total (4 GPU + 4 NIC). 4 CDI specs generated by nvidia driver. 4 NIC configs stored by dranet in PodConfigStore.

---

## Stage 6 — Container Runtime

CDI specs + NRI hooks bring devices into the pod.

### CDI Device Injection

```
Kubelet passes CDI device IDs to container runtime (CRI-O / containerd)

Runtime reads CDI specs from /var/run/cdi/:
  nvidia.com/gpu=0 → /dev/nvidia0, env NVIDIA_VISIBLE_DEVICES, mounts
  nvidia.com/gpu=1 → /dev/nvidia1
  nvidia.com/gpu=2 → /dev/nvidia2
  nvidia.com/gpu=3 → /dev/nvidia3

→ 4 GPUs visible inside container
```

### NRI Hook — NIC Setup

```
Runtime creates pod sandbox → RunPodSandbox event

dranet NRI plugin fires:
  GetPodConfig(pod-uid) → 4 device configs found

  For each NIC (nic-0, nic-1, nic-2, nic-3):
    ├─ Find network interface by PCI address
    ├─ Move interface to pod network namespace
    ├─ Rename to configured name (net0, net1, net2, net3)
    ├─ Set MTU = 9000
    ├─ Configure per-rail routing table + rules
    ├─ Attach RDMA device to pod namespace
    └─ Apply ethtool / eBPF settings

  Update shadow claim status → Ready condition
```

### Pod Running

Container starts with:

**4 × GPU:**
- /dev/nvidia0 — nvidia3
- CUDA / ROCm ready
- CDI-injected

**4 × RDMA NIC:**
- net0 (rail 0, table 100)
- net1 (rail 1, table 101)
- net2 (rail 2, table 102)
- net3 (rail 3, table 103)
- Per-rail policy routing active

---

## Stage 7 — Cleanup

Pod deletion cascades through all layers.

```
Pod deleted
  │
  ├─ Kubelet calls NodeUnprepareResources on composite driver
  │   ├─ For each shadow claim (8 total):
  │   │   ├─ gRPC NodeUnprepareResources → nvidia (cleans CDI specs)
  │   │   ├─ gRPC NodeUnprepareResources → dranet (cleans PodConfigStore)
  │   │   └─ Delete shadow claim from API server
  │   └─ Delete BoltDB state entry
  │
  ├─ dranet NRI StopPodSandbox → netns cleanup
  │
  ├─ Scheduler-created ResourceClaim
  │   └─ GC'd via Pod OwnerReference
  │
  └─ Shadow claims (backup)
      └─ GC'd via composite claim OwnerReference
```

---

## Resilience — Failure Recovery

### Why Idempotent

Every layer in the chain can be called multiple times for the same claim and produce the same result:

- **Kubelet** retries `PrepareResourceClaims` on any error or after driver restart — by design.
- **Composite driver** — shadow claims use deterministic names (`shadow-{claim}-{source}-{device}`). Second create for the same name hits `AlreadyExists` → driver detects and reuses. BoltDB restores mapping on restart so Unprepare knows what to clean.
- **nvidia driver** — checks `checkpoint.PreparedClaims[claimUID]`. If already prepared, returns cached CDI device IDs. No duplicate CDI specs.
- **dranet driver** — `PodConfigStore.SetDeviceConfig(podUID, device)` overwrites same key. NRI hook reads latest config. No duplicate interface moves.
- **Webhook** — checks `composite.dra/mutated` annotation. If set, returns no patches. Re-admission of same pod is a no-op.

### Failure Scenarios

| Failure | Recovery | Why it works |
|---------|----------|--------------|
| Driver pod crash mid-prepare | DaemonSet restarts → BoltDB restore → kubelet retries | Shadow claims already in API server survive crash. Re-prepare is idempotent. |
| Node goes down | ResourceSlices GC'd → scheduler won't allocate → pods reschedule | ResourceSlices owned by Node object. No stale devices to allocate. |
| Stale composite device allocated | Prepare fails → kubelet reports error → pod rescheduled | Self-healing: scheduler picks different node on retry. |
| Orphaned shadow claims | Reconciler (5-min loop) + OwnerReference GC | Shadow claims have OwnerRef → composite claim. Cascade delete on claim removal. |
| Underlying driver unavailable | gRPC error → kubelet retries Prepare later | Kubelet has built-in retry with backoff. Driver socket reconnects. |
| Webhook pod crash | 2 replicas behind Service → surviving replica handles traffic | Webhook is stateless. Annotation idempotency prevents re-mutation. |

---

## Architecture Summary

```
                User: gpu-nic-pair: 4
                         │
              ┌──────────▼──────────┐
              │     Scheduler       │  DRAExtendedResource
              │  auto-creates claim │  feature gate
              │  allocates from     │
              │  composite slices   │  ← no webhook
              └──────────┬──────────┘    no node pinning
                         │
              ┌──────────▼──────────┐
              │  Composite Driver   │
              │                     │
              │  Synthesizer:       │  watch nvidia + dranet slices
              │    pair by pcieRoot │  → publish composite ResourceSlice
              │                     │
              │  Plugin:            │  PrepareResourceClaims →
              │    shadow claims    │    → gRPC to nvidia (CDI)
              │    + gRPC delegate  │    → gRPC to dranet (NIC config)
              └─────────┬──┬────────┘
                        │  │
           ┌────────────┘  └────────────┐
           ▼                            ▼
  ┌─────────────────┐         ┌─────────────────┐
  │  gpu.nvidia.com │         │    dra.net       │
  │  CDI specs      │         │  PodConfigStore  │
  │  /var/run/cdi/  │         │  + NRI hooks     │
  └─────────────────┘         └─────────────────┘
```

---

## Links

- **Repo:** github.com/openshift-psap/composite-dra-driver
- **Issue #1:** NUMA affinity with DRAExtendedResource API
- **Discussion #3:** Architecture — Shadow Claims + Extended Resources

Go 1.26 · K8s v0.35.3 modules · targets K8s 1.34+ clusters
