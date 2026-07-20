# Composite DRA Driver

A generic, config-driven Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) driver that composes devices from multiple underlying DRA drivers into single allocatable units.

For example, pairing GPUs from `gpu.nvidia.com` with RDMA NICs from `dra.net` by PCIe root topology — presenting each GPU-NIC pair as one composite device the scheduler allocates natively.

## Why

The previous approach ([dra-rail-admission-webhook](https://github.com/openshift-psap/dra-rail-admission-webhook)) scanned ResourceSlices, picked nodes, selected specific devices, and pinned pods via nodeAffinity — bypassing the Kubernetes scheduler entirely. This driver moves all scheduling decisions back to the scheduler while preserving topology-aware device pairing.

## How It Works

```
Underlying Drivers                    Composite Driver                      User
┌──────────────┐                     ┌─────────────────────┐
│gpu.nvidia.com│─ ResourceSlices ──▶│ Synthesizer         │
│  (8 GPUs)    │                    │  watch → pair →     │
└──────────────┘                    │  publish composite  │──▶ ResourceSlices
┌──────────────┐                    │  ResourceSlices     │    (8 GPU-NIC pairs)
│   dra.net    │─ ResourceSlices ──▶│                     │
│  (8 NICs)    │                    ├─────────────────────┤         │
└──────────────┘                    │ Plugin (DRAPlugin)  │         ▼
                                    │  PrepareResources:  │    ┌──────────┐
                  ┌─── gRPC ◀───────│  1. shadow claims   │    │Scheduler │
                  │                 │  2. gRPC delegate   │◀───│allocates │
                  ▼                 │  3. return CDI IDs  │    │natively  │
           Underlying drivers       └─────────────────────┘    └──────────┘
           prepare their own
           devices as usual
```

**Shadow Claims Pattern**: On `PrepareResourceClaims`, the composite driver creates real ResourceClaims ("shadow claims") for each underlying driver with pre-filled allocation results, then calls their gRPC sockets to prepare hardware. Neither nvidia nor dranet checks if claims are in `pod.spec.resourceClaims` — validated on real hardware.

## What Gets Published

The composite driver watches ResourceSlices from underlying drivers and publishes new composite ResourceSlices. Here's what the transformation looks like on a node with 8× H100 GPUs and 8× ConnectX RDMA NICs:

**Source: GPU driver (`gpu.nvidia.com`)**
```yaml
# One of 8 GPU devices in the source ResourceSlice
- name: gpu-7
  attributes:
    addressingMode:             { string: HMM }
    architecture:               { string: Hopper }
    brand:                      { string: Nvidia }
    cudaComputeCapability:      { version: 9.0.0 }
    driverVersion:              { version: 580.126.20 }
    productName:                { string: "NVIDIA H100 80GB HBM3" }
    resource.kubernetes.io/pciBusID:   { string: "0000:a4:00.0" }
    resource.kubernetes.io/pcieRoot:   { string: pci0000:a0 }       # ← pairing key
    uuid:                       { string: GPU-7c664fc3-... }
  capacity:
    memory:                     { value: 81559Mi }
```

**Source: NIC driver (`dra.net`)**
```yaml
# One of 8 RDMA NIC devices in the source ResourceSlice
- name: pci-0000-a3-00-0
  attributes:
    dra.net/ifName:             { string: enp163s0 }
    dra.net/ipv4:               { string: "10.0.0.5/16" }
    dra.net/mac:                { string: "02:00:02:bc:50:f4" }
    dra.net/mtu:                { int: 9000 }
    dra.net/numaNode:           { int: 0 }
    dra.net/pciAddress:         { string: "0000:a3:00.0" }
    dra.net/pciDevice:          { string: ConnectX Family mlx5Gen Virtual Function }
    dra.net/pciVendor:          { string: Mellanox Technologies }
    dra.net/rdma:               { bool: true }                      # ← CEL filter: rdma == true
    dra.net/sriov:              { bool: false }
    dra.net/state:              { string: up }
    resource.kubernetes.io/pcieRoot:   { string: pci0000:a0 }       # ← same root → paired with gpu-7
```

**Output: Composite driver (`composite.dra.example.io`)**
```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
spec:
  driver: composite.dra.example.io
  nodeName: worker-3
  pool:
    name: composite.dra.example.io-worker-3-gpu-nic-pair
    resourceSliceCount: 1
  devices:
  - name: gpu-7--pci-0000-a3-00-0                         # concatenated source device names
    attributes:
      # Forwarded from GPU (prefixed with source name "gpu/")
      gpu/pciBusID:             { string: "0000:a4:00.0" }
      gpu/pcieRoot:             { string: pci0000:a0 }
      # Forwarded from NIC (prefixed with source name "nic/")
      nic/pciAddress:           { string: "0000:a3:00.0" }
      nic/numaNode:             { int: 0 }
      nic/rdma:                 { bool: true }
      nic/ipv4:                 { string: "10.0.0.5/16" }
      # Constraint attribute promoted to top level
      resource.kubernetes.io/pcieRoot:  { string: pci0000:a0 }
      # Auto-injected composite metadata
      composite/compositionName:        { string: gpu-nic-pair }
      composite/numaNode:               { int: 0 }
  - name: gpu-6--pci-0000-ad-00-0
    # ... (7 more pairs, one per matching pcieRoot)
```

Only attributes listed in `forwardAttributes` config appear in the composite ResourceSlice. The full source attribute set (GPU uuid, NIC vendor, etc.) is preserved internally for shadow claim construction.

## Quick Start

### Install

```bash
# Driver only (webhook auto-detected based on K8s version)
helm install composite charts/composite-dra-driver \
  -n composite-dra-system \
  -f charts/composite-dra-driver/values-poseidon.yaml
```

### Request GPU-NIC Pairs

**K8s 1.36+ (DRAExtendedResource beta, no webhook needed):**
```yaml
containers:
- resources:
    requests:
      composite.dra.example.io/gpu-nic-pair: "4"
```

**K8s < 1.36 (webhook intercepts synthetic resource):**
```yaml
containers:
- resources:
    requests:
      composite.dra/gpu-nic-pair: "4"
    limits:
      composite.dra/gpu-nic-pair: "4"  # must match requests
```

**Manual ResourceClaimTemplate (any K8s version, no webhook):**
```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: my-pairs
spec:
  spec:
    devices:
      requests:
      - name: pair-0
        exactly:
          deviceClassName: composite-gpu-nic
          allocationMode: ExactCount
          count: 1
      - name: pair-1
        exactly:
          deviceClassName: composite-gpu-nic
          allocationMode: ExactCount
          count: 1
```

### Verify

```bash
# Composite devices published
kubectl get resourceslices -o custom-columns='DRIVER:.spec.driver' | sort | uniq -c

# Inside pod
kubectl exec <pod> -- nvidia-smi -L          # GPUs
kubectl exec <pod> -- ip -br addr            # NICs (net0, net1, ...)
kubectl exec <pod> -- rdma link show         # RDMA devices
```

## Features

- **Generic composition** — config-driven, not hardcoded to GPU+NIC. Add any DRA driver via config.
- **Scheduler-native** — no node pinning, no allocation bypass. Scheduler decides everything.
- **Shadow claims** — delegates hardware prep to underlying drivers via gRPC. Zero driver-specific code.
- **CEL filters** — filter source devices (e.g., `rdma == true`) before pairing.
- **Per-rail NIC config** — routing tables, gateways, cross-rail routes embedded in shadow claims.
- **Auto-detect webhook** — Helm chart auto-deploys webhook on K8s < 1.36, skips on 1.36+ (DRAExtendedResource).
- **Parallel prepare** — shadow claims created concurrently. 8 pairs in ~3s (bottleneck: nvidia CDI gen).
- **Crash recovery** — BoltDB persistence + orphan reconciler.
- **HA** — driver: DaemonSet (one per node). Webhook: multi-replica Deployment.

## AI-Assisted Development

Built with [Claude Code](https://claude.ai/code) (Claude Opus 4.6).

## Configuration

```yaml
driver:
  name: "composite.dra.example.io"

sources:
  - name: gpu
    driver: "gpu.nvidia.com"
    deviceClassName: "gpu.nvidia.com"
    forwardAttributes:
      - domain: "resource.kubernetes.io"
        attributes: ["pciBusID", "pcieRoot"]
  - name: nic
    driver: "dra.net"
    deviceClassName: "dranet"
    forwardAttributes:
      - domain: "dra.net"
        attributes: ["pciAddress", "numaNode", "rdma", "ipv4"]

compositions:
  - name: "gpu-nic-pair"
    members:
      - source: gpu
        count: 1
      - source: nic
        count: 1
    constraints:
      - type: matchAttribute
        attribute: "resource.kubernetes.io/pcieRoot"
    filters:
      nic:
        cel: 'device.attributes["dra.net"].rdma == true'

deviceParams:
  configMapPath: "/etc/composite-dra/device-params/params.yaml"
```

Opaque driver params (routes, gateways, MTU, etc.) are provided via an external ConfigMap — the composite driver never interprets their content. See [examples/](charts/composite-dra-driver/examples/) for ConfigMap templates and [values.yaml](charts/composite-dra-driver/values.yaml) for all options.

## Observability

Both binaries serve 16 Prometheus metrics on `/metrics` (port 8080, configurable) — gauges for device/claim counts, histograms for prepare/synthesis latency, counters for errors and webhook activity. Kubernetes Events are emitted on ResourceClaims for the prepare/unprepare lifecycle. All logging uses structured `klog.InfoS`/`ErrorS`.

Enable scraping: `--set metrics.serviceMonitor.enabled=true`. See [Observability reference](docs/OBSERVABILITY.md) for the full metric catalog, PromQL examples, and scraping setup.

## Requirements

- Kubernetes 1.34+ with DRA enabled
- Go 1.26 (build)
- Underlying DRA drivers deployed (e.g., nvidia GPU driver, dranet)
- OpenShift: SCC for hostPath volumes (auto-created by Helm chart)

## Known Limitations

- **No NUMA affinity enforcement** — composite devices carry `numaNode` as an attribute but the webhook does not generate MatchAttribute constraints. NUMA packing requires manual ResourceClaimTemplates with explicit constraints. ([#1](https://github.com/openshift-psap/composite-dra-driver/issues/1), [Discussion #11](https://github.com/openshift-psap/composite-dra-driver/discussions/11))

- **Device sharing conflict across compositions** — when multiple compositions share a source (e.g. GPU appears in both `gpu` and `gpu-nic-pair`), the scheduler can allocate the same physical device to both compositions on the same node. Each composition publishes an independent pool — the scheduler has no cross-pool mutual exclusion. Safe when pods land on different nodes or only one composition is actively used at a time. Fix requires pairer-side device partitioning. ([#28](https://github.com/openshift-psap/composite-dra-driver/issues/28))

- **VF support requires external IPAM** — PF mode works with the external device params ConfigMap. VF mode needs an external controller to allocate IPs and populate the ConfigMap (VFs lack `dra.net/ipv4`). ([#34](https://github.com/openshift-psap/composite-dra-driver/issues/34))

- **Webhook required on K8s < 1.36** — the `DRAExtendedResource` feature gate is beta (on by default) in K8s 1.36, eliminating the need for the webhook. On K8s 1.35, the gate exists as alpha and can be manually enabled — see [Method 3 in the cheatsheet](docs/CHEATSHEET.md#method-3-extended-resource-k8s-135-with-draextendedresource-gate) and the [FAQ entry on dropping the webhook](docs/FAQ.md#when-can-we-drop-the-webhook-entirely). On K8s 1.34, the webhook is the only option for the resource request UX.

- **Extended resources require limits == requests** — Kubernetes requires this for all extended resources. Affects StatefulSet/LWS pod templates. Not a bug — K8s API constraint.

## Documentation

- [Cheatsheet](docs/CHEATSHEET.md) — install, request methods, verify, troubleshoot
- [Observability](docs/OBSERVABILITY.md) — metrics reference, scraping setup, events, structured logging
- [FAQ](docs/FAQ.md) — architecture decisions, scheduling, networking, performance
- [HA Design](docs/HA-DESIGN.md) — failure scenarios, DaemonSet vs Deployment HA
- [Status](docs/STATUS.md) — implementation status and validation evidence
## License

Apache 2.0
