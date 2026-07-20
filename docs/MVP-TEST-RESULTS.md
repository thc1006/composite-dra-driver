# Composite DRA Driver — MVP Test Results

**Poseidon Cluster** · OCP 4.21 / K8s 1.34 · 4× 8×H100 GPU nodes · see [`git log`](https://github.com/openshift-psap/composite-dra-driver/commits/main/docs/MVP-TEST-RESULTS.md) for test date

> github.com/openshift-psap/composite-dra-driver
>
> This is a point-in-time test report. For current project status, see [STATUS.md](STATUS.md).

---

## What We Tested

1. Composite driver DaemonSet deploys on all nodes (including control-plane)
2. Synthesizer watches nvidia + dranet ResourceSlices, pairs by PCIe root
3. CEL filter removes non-RDMA NICs
4. Composite ResourceSlices published with correct attributes
5. Scheduler allocates composite device natively (no webhook, no node pinning)
6. Shadow claims created for nvidia + dranet
7. gRPC delegation to both underlying drivers succeeds
8. Pod starts with GPU + NIC

---

## Cluster: Poseidon — Test Environment

```
$ oc get nodes -o wide

NAME                             STATUS   ROLES                  AGE
psap-gpu-xhnvx-master-0          Ready    control-plane,master   483d
psap-gpu-xhnvx-storage-q6lkl     Ready    worker                 479d
psap-gpu-xhnvx-worker-1-n7mkr    Ready    worker                 483d
psap-gpu-xhnvx-worker-3-6bsjp    Ready    8xh100,worker          483d
psap-gpu-xhnvx-worker-3-db7jp    Ready    8xh100,worker          251d
psap-gpu-xhnvx-worker-3-dls46    Ready    8xh100,worker          468d
psap-gpu-xhnvx-worker-3-nwkdv    Ready    8xh100,worker          21d

OCP 4.21  ·  K8s 1.34.5  ·  CRI-O  ·  4 GPU nodes × 8 H100  ·  8 rails
```

## Underlying DRA Drivers

```
$ oc get resourceslices -o custom-columns='DRIVER:.spec.driver' | sort | uniq -c

      7 dra.net
      4 gpu.nvidia.com
      4 compute-domain.nvidia.com
```

**gpu.nvidia.com:** 8 GPUs per node, DeviceClass: gpu.nvidia.com

**dra.net:** 13 NICs per GPU node (8 RDMA + 5 non-RDMA), DeviceClass: dranet

---

## Step 1 — Deploy Composite Driver

### DaemonSet Running on All Nodes

```
$ oc get pods -n composite-dra-system -o wide

NAME                         READY   STATUS    NODE
composite-dra-driver-58szp   1/1     Running   psap-gpu-xhnvx-worker-3-dls46
composite-dra-driver-5nxn5   1/1     Running   psap-gpu-xhnvx-storage-q6lkl
composite-dra-driver-89qmc   1/1     Running   psap-gpu-xhnvx-worker-3-nwkdv
composite-dra-driver-d47wl   1/1     Running   psap-gpu-xhnvx-worker-3-db7jp
composite-dra-driver-gn8zr   1/1     Running   psap-gpu-xhnvx-worker-1-n7mkr
composite-dra-driver-k2plz   1/1     Running   psap-gpu-xhnvx-master-0        ← control-plane
composite-dra-driver-rl5wb   1/1     Running   psap-gpu-xhnvx-worker-3-6bsjp
```

7/7 pods running — all nodes including master. SCC with hostPath + privileged for kubelet plugin sockets.

---

## Step 2 — Device Synthesis

### Driver Logs — Synthesis

```
I0605 13:59:44.115382  synthesizer: starting for node psap-gpu-xhnvx-worker-3-6bsjp
I0605 13:59:44.115483  synthesizer: source gpu has 8 devices
I0605 13:59:44.115488  synthesizer: source nic has 13 devices
I0605 13:59:44.117887  pairer: CEL filter "...rdma == true": 8/13 devices passed
I0605 13:59:44.117967  synthesizer: computed 8 composite devices from 21 source devices
I0605 13:59:44.117983  publisher: publishing 8 devices in 1 slice(s)
```

- ✓ 8 GPUs discovered
- ✓ 13 NICs discovered, CEL filter removed 5 non-RDMA → 8 RDMA NICs remain
- ✓ 8 composite GPU-NIC pairs computed (1:1 by PCIe root)
- ✓ Published as 1 ResourceSlice

### Composite ResourceSlices Published

```
$ oc get resourceslices -o custom-columns='DRIVER:.spec.driver' | sort | uniq -c

      7 composite.dra.example.io    ← NEW
      7 dra.net
      4 gpu.nvidia.com
      4 compute-domain.nvidia.com
```

7 composite slices — one per node (4 GPU workers + 3 non-GPU nodes with 0 devices each).

---

## Step 3 — Composite Device Attributes

### Example: gpu-0--pci-0000-e9-00-0

```yaml
# One composite device on worker-3-6bsjp
name: gpu-0--pci-0000-e9-00-0
attributes:
  resource.kubernetes.io/pcieRoot:    "pci0000:e6"      # shared constraint
  composite/numaNode:                1                   # NUMA zone
  gpu/pciBusID:                      "0000:ea:00.0"      # GPU PCI address
  gpu/pcieRoot:                      "pci0000:e6"        # GPU PCIe root
  nic/pciAddress:                    "0000:e9:00.0"      # NIC PCI address
  nic/pcieRoot:                      "pci0000:e6"        # NIC PCIe root (matches GPU ✓)
  nic/ipv4:                          "10.7.0.5/16"       # NIC IP (rail 7)
  nic/rdma:                          true                # RDMA capable
  nic/encapsulation:                 "ether"             # RoCE
  nic/ifName:                        "enp233s0"          # host interface
  nic/mac:                           "02:00:02:bc:50:fd" # MAC
  nic/numaNode:                      1                   # NIC NUMA
```

12 attributes per device (under 32 limit). GPU and NIC share `pci0000:e6` PCIe root — pairing correct.

---

## Step 4 — Test Pod Submission

### ResourceClaimTemplate

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: composite-1pair
  namespace: composite-dra-test
spec:
  spec:
    devices:
      requests:
      - name: pair-0
        exactly:
          deviceClassName: composite-gpu-nic
          allocationMode: ExactCount
          count: 1
```

Single request for 1 composite device. No node pinning. No device selection. Scheduler decides everything.

### Test Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: composite-test-1pair
  namespace: composite-dra-test
spec:
  restartPolicy: Never
  containers:
  - name: test
    image: registry.access.redhat.com/ubi9/ubi-minimal:latest
    command: ["sh", "-c", "echo 'Composite DRA test'; sleep 300"]
    resources:
      claims:
      - name: gpu-nic
        request: pair-0
  resourceClaims:
  - name: gpu-nic
    resourceClaimTemplateName: composite-1pair
```

---

## Step 5 — Scheduler Allocated Natively

```
$ oc get pod composite-test-1pair -n composite-dra-test -o wide

NAME                   READY   STATUS    NODE
composite-test-1pair   1/1     Running   psap-gpu-xhnvx-worker-3-nwkdv
```

```
$ oc get resourceclaims -n composite-dra-test

NAME                                 STATE
composite-test-1pair-gpu-nic-ptd6f   allocated,reserved
```

- ✓ Scheduler picked node `worker-3-nwkdv` — no webhook involved, no nodeAffinity pinning
- ✓ ResourceClaim allocated and reserved for the pod

---

## Step 6 — Shadow Claims Created

### Three Claims in API Server

```
$ oc get resourceclaims -n composite-dra-test

NAME                                                              STATE
composite-test-1pair-gpu-nic-ptd6f                                allocated,reserved
shadow-composite-test-1pair-gpu-nic-ptd6f-gpu-gpu-0               allocated,reserved
shadow-composite-test-1pair-gpu-nic-ptd6f-nic-pci-0000-e9-00-0    allocated,reserved
```

**GPU Shadow Claim:**
- Driver: `gpu.nvidia.com`
- Device: `gpu-0`
- DeviceClass: `gpu.nvidia.com`
- Pre-filled allocation + ReservedFor

**NIC Shadow Claim:**
- Driver: `dra.net`
- Device: `pci-0000-e9-00-0`
- DeviceClass: `dranet`
- Pre-filled allocation + ReservedFor

---

## Step 7 — gRPC Delegation (Driver Logs)

### Full Prepare Flow

```
I0605 14:13:22.341729  shadow: created composite-dra-test/
    shadow-...-gpu-gpu-0 (uid=01994325...)
    for gpu.nvidia.com device psap-gpu-xhnvx-worker-3-nwkdv/gpu-0

I0605 14:13:22.341842  grpc: connected to gpu.nvidia.com
    at /var/lib/kubelet/plugins/gpu.nvidia.com/dra.sock

I0605 14:13:22.341852  grpc: calling NodePrepareResources on gpu.nvidia.com
    for claim shadow-...-gpu-gpu-0  ✓

I0605 14:13:22.612781  shadow: created composite-dra-test/
    shadow-...-nic-pci-0000-e9-00-0 (uid=68818ab0...)
    for dra.net device psap-gpu-xhnvx-worker-3-nwkdv/pci-0000-e9-00-0

I0605 14:13:22.612868  grpc: connected to dra.net
    at /var/lib/kubelet/plugins/dra.net/dra.sock

I0605 14:13:22.612874  grpc: calling NodePrepareResources on dra.net
    for claim shadow-...-nic-pci-0000-e9-00-0  ✓

I0605 14:13:22.633197  plugin: prepared claim composite-dra-test/
    composite-test-1pair-gpu-nic-ptd6f
    — 1 composite devices, 2 shadow claims  ✓
```

### Timing

| Phase | Duration |
|-------|----------|
| Create GPU shadow claim + gRPC to nvidia | ~271ms |
| Create NIC shadow claim + gRPC to dranet | ~20ms |
| **Total Prepare** | **~291ms** |

nvidia takes longer (CDI spec generation). dranet is fast (stores config for NRI phase).

---

## Step 8 — Pod Running

```
Events:
  Normal  Scheduled       21s  default-scheduler  Assigned to worker-3-nwkdv
  Normal  AddedInterface  20s  multus             Add eth0 [10.130.2.231/23]
  Normal  Pulling         19s  kubelet            Pulling ubi9/ubi-minimal
  Normal  Pulled          17s  kubelet            Pulled in 1.766s
  Normal  Created         17s  kubelet            Created container: test
  Normal  Started         17s  kubelet            Started container test
```

**Pod is Running ✓** — Scheduler allocated → Kubelet prepared → nvidia CDI applied → dranet NRI configured → Container started

---

## Scaling — Multi-Pair Prepare Timing

### Sequential Prepare Cost (measured)

| Pairs | Shadow Claims | Total Prepare | Per-pair avg |
|-------|---------------|---------------|--------------|
| 1 | 2 | 291ms | 291ms |
| 4 | 8 | 1,550ms | 388ms |
| 8 | 16 | 4,633ms | 579ms |

Per shadow claim: 2 API server calls (Create + UpdateStatus) + 1 gRPC call.
nvidia gRPC: ~270ms avg (CDI gen) · dranet gRPC: ~50ms avg (config store).
Parallelization tracked in [#5](https://github.com/openshift-psap/composite-dra-driver/issues/5) — estimated ~310ms for any pair count.

---

## Verify: GPU Visibility — nvidia-smi

### 2 GPUs Visible Per Pod (2-pair claim)

```
$ oc exec node-a -- nvidia-smi -L
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-41b4aa88-8d29-d1df-f723-ea3d34e19741)
GPU 1: NVIDIA H100 80GB HBM3 (UUID: GPU-8c2567b5-5786-b22f-607e-94291e5c37ce)

$ oc exec node-b -- nvidia-smi -L
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-8bae7aae-0cea-c558-f4b7-ec2d5b47479c)
GPU 1: NVIDIA H100 80GB HBM3 (UUID: GPU-7a2193fd-082d-6f3b-97eb-603e775f9b69)
```

✓ nvidia CDI injection working — GPUs visible via shadow claims.

### GPU Topology (nvidia-smi topo -m)

```
       GPU0  GPU1  NIC0  NIC1  ... CPU Affinity  NUMA
GPU0    X    NV18  NODE  PIX       80-159        1
GPU1   NV18   X    PIX   NODE      80-159        1
NIC0   NODE  PIX    X    NODE
NIC1   PIX   NODE  NODE   X
```

GPU0↔NIC1 and GPU1↔NIC0: PIX (single PCIe bridge) — optimal GPUDirect RDMA placement.
GPU0↔GPU1: NV18 (18× NVLink). Both NUMA 1.

---

## Verify: NIC + Routing Configuration

### NICs in Pod Network Namespace

**node-a (nwkdv):**
```
net0  UP  10.7.0.7/16  mtu 9000
net1  UP  10.6.0.7/16  mtu 9000
```

**node-b (db7jp):**
```
net0  UP  10.7.0.4/16  mtu 9000
net1  UP  10.6.0.4/16  mtu 9000
```

dranet NRI hook moved NICs to pod netns, set MTU 9000. ✓

### Per-Rail Policy Routing

```
# Policy rules
32765: from 10.7.0.0/16 lookup 107
32765: from 10.6.0.0/16 lookup 106

# Table 107 (rail 7)
10.7.0.0/16 dev net0 scope link
default via 10.7.0.1 dev net0

# Table 106 (rail 6)
10.6.0.0/16 dev net1 scope link
default via 10.6.0.1 dev net1

# Main table — cross-rail routes
10.0.0.0/16 via 10.7.0.1 dev net0    ← rail 0 reachable via rail 7 gw
10.1.0.0/16 via 10.7.0.1 dev net0    ← rail 1
10.2.0.0/16 via 10.7.0.1 dev net0    ...
10.5.0.0/16 via 10.7.0.1 dev net0
10.6.0.0/16 via 10.7.0.1 dev net0    ← cross-rail to rail 6
```

Per-rail isolation + cross-rail reachability via L3 gateway. Matches original webhook config. ✓

---

## Verify: Network Connectivity Tests

### Ping Results — Two Nodes

| Test | Source | Dest | Result | TTL |
|------|--------|------|--------|-----|
| Same rail 7 | node-a net0 (10.7.0.7) | node-b net0 (10.7.0.4) | 0% loss, 0.5ms | 64 (direct) |
| Same rail 6 | node-a net1 (10.6.0.7) | node-b net1 (10.6.0.4) | 0% loss, 0.6ms | 64 (direct) |
| Cross rail 7→6 | node-a net0 (10.7.0.7) | node-b net1 (10.6.0.4) | 0% loss, 0.7ms | 63 (via gateway) |

Same-rail: direct L2. Cross-rail: routed via L3 gateway (ttl=63). Both working. ✓

---

## Verify: RDMA Devices

```
$ oc exec node-a -- rdma link show
link mlx5_0/1 state ACTIVE physical_state LINK_UP netdev net0
link mlx5_1/1 state ACTIVE physical_state LINK_UP netdev net1

$ oc exec node-b -- rdma link show
link mlx5_0/1 state ACTIVE physical_state LINK_UP netdev net0
link mlx5_1/1 state ACTIVE physical_state LINK_UP netdev net1
```

mlx5_0 → net0, mlx5_1 → net1. Both ACTIVE/LINK_UP. RDMA ready for GPUDirect. ✓

---

## Verify: RDMA Write Bandwidth

### ib_write_bw — Same Rail + Cross Rail

```
=== Same Rail 7: node-a mlx5_0 → node-b mlx5_0 ===
 #bytes     #iterations    BW peak[Gb/sec]    BW average[Gb/sec]
 65536      5000             51.83              47.54

=== Cross Rail 7→6: node-a mlx5_0 → node-b mlx5_1 ===
 #bytes     #iterations    BW peak[Gb/sec]    BW average[Gb/sec]
 65536      5000             73.12              69.36
```

| Test | QP | Path | BW avg | BW peak |
|------|----|------|--------|---------|
| Same rail | 1 | mlx5_0 → mlx5_0 (10.7.x) | 47.54 Gb/s | 51.83 Gb/s |
| Cross rail | 1 | mlx5_0 → mlx5_1 (10.7→10.6) | 69.36 Gb/s | 73.12 Gb/s |
| Same rail | 8 | mlx5_0 → mlx5_0 (10.7.x) | **318.35 Gb/s** | 372.97 Gb/s |
| Cross rail | 8 | mlx5_0 → mlx5_1 (10.7→10.6) | **317.91 Gb/s** | 372.97 Gb/s |

QP=8: ~318 Gb/s avg, 373 Gb/s peak — near 400G line rate. Same-rail and cross-rail identical. ✓

---

## Full Verification Summary

| Component | Status | Evidence |
|-----------|--------|----------|
| DaemonSet on all nodes | ✓ | 7/7 pods Running (incl. control-plane) |
| ResourceSlice watch | ✓ | 8 GPUs + 13 NICs discovered |
| CEL filter (rdma==true) | ✓ | 8/13 NICs passed filter |
| PCIe root pairing | ✓ | 8 pairs, matching pcieRoot values |
| Composite ResourceSlice publish | ✓ | 7 slices via kubeletplugin.Helper |
| Scheduler-native allocation | ✓ | No webhook, no node pinning |
| Shadow claim creation | ✓ | GPU + NIC claims in API server |
| gRPC to gpu.nvidia.com | ✓ | nvidia-smi shows GPUs, CDI working |
| gRPC to dra.net | ✓ | NICs in pod netns, MTU 9000 |
| Per-rail routing | ✓ | Policy tables + rules configured |
| Cross-rail routing | ✓ | Ping via L3 gateway, ttl=63 |
| RDMA devices | ✓ | mlx5 bound to net0/net1, ACTIVE |
| Multi-pair (4 pairs) | ✓ | 8 shadow claims, 1.5s prepare |
| Multi-pair (8 pairs) | ✓ | 16 shadow claims, ~3s prepare (parallel) |
| RDMA write BW (QP=1) | ✓ | 47-69 Gb/s avg |
| RDMA write BW (QP=8) | ✓ | 318 Gb/s avg, 373 peak — near 400G line rate |

---

## Architecture: Validated

```
User: ResourceClaimTemplate (1 composite pair)
         │
         ▼
   ┌───────────┐   allocates composite device
   │ Scheduler │   from composite ResourceSlice        ← no webhook
   └─────┬─────┘   picks node natively                 ← no node pinning
         │
         ▼
   ┌───────────────────────────────┐
   │  Composite Driver (DaemonSet) │
   │                               │
   │  PrepareResourceClaims:       │
   │   1. Create shadow GPU claim  │──→ gRPC to gpu.nvidia.com ✓
   │   2. Create shadow NIC claim  │──→ gRPC to dra.net        ✓
   │   3. Return combined CDI IDs  │
   └───────────────────────────────┘
         │
         ▼
   Container starts with GPU + NIC ✓
```

Shadow claims pattern works end-to-end on real hardware. Generic — no nvidia/dranet code in composite driver.

---

## Status at Time of Test

Items marked ✓ were validated during this MVP test. Items below were open at test time — see [STATUS.md](STATUS.md) for current state.

- ✓ Multi-pair test (4 pairs, 8 pairs)
- ✓ GPU visible in container (nvidia-smi, topo)
- ✓ NIC in pod netns (ip link, routing, RDMA)
- ✓ Same-rail + cross-rail ping
- ✓ Parallel shadow claims (PR #12) — 4.6s → 3s for 8 pairs
- Attribute dedup + rail index ([#4](https://github.com/openshift-psap/composite-dra-driver/issues/4))
- Cleanup test — delete pod, verify shadow claims GC'd
- K8s 1.36+ DRAExtendedResource testing

---

## Auxiliary: Thin Webhook — When & Why

### Why It Exists

The `DRAExtendedResource` feature gate (K8s 1.36+ beta) lets users request composite devices via standard extended resources — **no webhook needed**.

But extended resources create a **single DeviceRequest with Count=N**. No cross-device constraints possible. **NUMA affinity needs MatchAttribute constraints across multiple requests** — only a webhook can generate those.

| K8s Version | Simple "give me N pairs" | NUMA-affine pairs |
|-------------|--------------------------|-------------------|
| 1.34 | Webhook or manual claims | Webhook |
| 1.35+ (alpha gate) | Extended resource ✓ | Webhook |
| 1.36+ (beta, default) | Extended resource ✓ | Webhook |

### What It Does

Synthetic resource → ResourceClaimTemplate with NUMA MatchAttribute constraints. No allocation, no node pinning.

```yaml
# User writes:
resources:
  requests:
    composite.dra/gpu-nic-pair: "2"
  limits:
    composite.dra/gpu-nic-pair: "2"   # must match requests

# Webhook strips the synthetic resource and generates:
devices:
  requests:
  - {name: pair-0, exactly: {deviceClassName: composite-gpu-nic, count: 1}}
  - {name: pair-1, exactly: {deviceClassName: composite-gpu-nic, count: 1}}
```

### Verified on Poseidon

```
$ oc apply -f - <<< 'kind: Pod ... resources.requests: composite.dra/gpu-nic-pair: "2"'

$ oc get pod webhook-test -o wide
NAME           READY   STATUS    NODE
webhook-test   1/1     Running   psap-gpu-xhnvx-worker-3-6bsjp

$ oc get resourceclaims -n composite-dra-test
NAME                                              STATE
webhook-test-composite-pairs-4hpl6                 allocated,reserved
shadow-...-gpu-gpu-0                               allocated,reserved
shadow-...-gpu-gpu-1                               allocated,reserved
shadow-...-nic-pci-0000-df-00-0                    allocated,reserved
shadow-...-nic-pci-0000-e9-00-0                    allocated,reserved

$ oc exec webhook-test -- ip -br addr
net0  UP  10.7.0.5/16
net1  UP  10.6.0.5/16

$ oc exec webhook-test -- rdma link show | grep netdev
mlx5_0/1 ACTIVE LINK_UP netdev net0
mlx5_1/1 ACTIVE LINK_UP netdev net1
```

Webhook intercepts resource request → claim template → scheduler allocates → shadow claims → GPUs + NICs + RDMA in pod. ✓

### Deployment

```
# Enable webhook via Helm
helm upgrade composite charts/composite-dra-driver \
  -n composite-dra-system \
  -f values-poseidon.yaml \
  --set webhook.enabled=true \
  --set webhook.tls.certManager.issuerRef.name=composite-dra-selfsigned

# Label namespaces for interception
oc label ns <namespace> composite.dra/webhook-enabled=true
```

Webhook is optional. cert-manager handles TLS. 2 replicas for HA.

---

## MVP: Pass ✓

Poseidon cluster · see git history for test date
