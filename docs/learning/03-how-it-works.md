# How It Works

This document explains the composite driver's architecture at a level where you can follow what happens when a user requests GPU-NIC pairs. Code details are in [05-code-walkthrough.md](05-code-walkthrough.md).

## Two Binaries

| Binary | Deployment | Role |
|--------|------------|------|
| **Driver** (`cmd/driver`) | DaemonSet on every node | Watches underlying drivers, pairs devices, publishes composite ResourceSlices, handles Prepare/Unprepare via shadow claims |
| **Webhook** (`cmd/webhook`) | Deployment (typically 1 replica) | Mutating admission webhook — intercepts pod resource requests, generates ResourceClaimTemplates |

The driver is always required. The webhook is optional on Kubernetes 1.36+ where `DRAExtendedResource` feature gate handles the same translation natively.

## The Synthesizer Pipeline

The driver's main loop is a three-stage pipeline that runs on every node:

![Overview Diagram](../overview-diagram.png)

**1. Watch** — An informer watches all ResourceSlices from configured source drivers on this node. Changes trigger a 500ms debounced recompute.

**2. Pair** — The pairer groups devices from different sources into composite devices based on topology constraints:
- **Auto mode** (default): devices sharing the same attribute value (e.g., `pcieRoot`) are paired. If 4 GPUs and 4 NICs share PCIe root `0x0a`, that produces 4 GPU-NIC pairs.
- **Explicit mode**: admin defines exact pairs via CEL selectors per node pool. Required when hardware doesn't expose topology attributes (cloud VMs).

**3. Publish** — Composite devices are published as ResourceSlices, chunked at 128 devices per slice (the Kubernetes limit). The scheduler sees them as normal devices.

For the full end-to-end allocation flow with mermaid diagrams, see [ARCHITECTURE.md § 1.3](../ARCHITECTURE.md#13-end-to-end-allocation-flow).

## What the User Sees

Requesting composite devices is a one-liner:

```yaml
resources:
  requests:
    composite.dra.io/gpu-nic-pair: "4"
  limits:
    composite.dra.io/gpu-nic-pair: "4"
```

The webhook (or `DRAExtendedResource` gate on K8s 1.36+) translates this into a `ResourceClaimTemplate` with 4 device requests. The scheduler allocates from the composite driver's ResourceSlices. The user never creates claims manually.

For all three request methods (webhook resource request, manual ResourceClaimTemplate, DRAExtendedResource), see [CHEATSHEET.md](../CHEATSHEET.md).

## Shadow Claims — Preview

When kubelet tells the composite driver to prepare an allocated device, the driver creates real ResourceClaim objects for each underlying driver with pre-filled allocation results. It then calls their gRPC sockets directly. Neither nvidia nor dranet knows it's talking to a composite driver.

This is the core mechanism. The next document covers it in depth: [04-shadow-claims.md](04-shadow-claims.md).

## Configuration

A minimal config with one composition:

```yaml
driver:
  name: "composite.dra.io"

sources:
- name: nvidia
  driver: gpu.nvidia.com
  deviceClassName: gpu.nvidia.com
  forwardAttributes:
  - domain: nvidia.com
    attributes: [model, memory]

- name: dra-net
  driver: dra.net
  deviceClassName: dra-rdma-nic
  forwardAttributes:
  - domain: dra.net
    attributes: [speed, mtu, rdma_type, ipv4]
  - domain: resource.kubernetes.io
    attributes: [pcieRoot]

compositions:
- name: gpu-nic-pair
  members:
  - source: nvidia
    count: 1
  - source: dra-net
    count: 1
  constraints:
  - type: matchAttribute
    attribute: "resource.kubernetes.io/pcieRoot"
```

Key points:
- **Sources** define the underlying drivers and which attributes to forward into composite devices
- **Compositions** define pairing rules — which sources, how many of each, and what topology constraint to enforce
- Adding a new underlying driver is a config change, not a code change

For the full schema with all options (explicit pairing, CEL filters, opaque device params), see [ARCHITECTURE.md § 5.3](../ARCHITECTURE.md#53-configuration-schema).

## Exercise 1: Deploy and Inspect

**Time:** ~10 minutes
**Requires:** Cluster with GPU and NIC DRA drivers installed (e.g., `gpu.nvidia.com` and `dra.net`)

**Step 1:** Install the composite driver.

```bash
helm install composite charts/composite-dra-driver \
  -n composite-dra-system --create-namespace \
  -f charts/composite-dra-driver/values.yaml
```

**Step 2:** Verify composite ResourceSlices appear alongside the source drivers' slices.

```bash
kubectl get resourceslices -o custom-columns='DRIVER:.spec.driver' | sort | uniq -c
```

You should see three drivers: `gpu.nvidia.com`, `dra.net`, and your composite driver name (e.g., `composite.dra.io`).

**Step 3:** Examine a composite ResourceSlice.

```bash
kubectl get resourceslices -l 'app.kubernetes.io/managed-by=composite-dra-driver' -o yaml | head -80
```

Look for:
- **Pool name** — format: `driverName-nodeName-compositionName`
- **Device name** — joined underlying device names (e.g., `gpu0--nic2`)
- **Forwarded attributes** — prefixed by source name (e.g., `nvidia/model`, `dra-net/ipv4`)
- **`composite/compositionName`** — metadata attribute identifying which composition produced this device

**Step 4:** Compare a composite device's attributes to the source devices it was paired from.

```bash
# Pick a composite device name from Step 3 (e.g., gpu0--nic2)
# Find the source GPU and NIC in their respective ResourceSlices
kubectl get resourceslices -o json | jq '.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[]'
```

<details>
<summary>Expected output (if you don't have cluster access)</summary>

```
# Step 2 output
   4 composite.dra.io
   8 dra.net
   8 gpu.nvidia.com

# Step 3: composite ResourceSlice has pool "composite.dra.io-node01-gpu-nic-pair",
# devices named like "gpu0--nic2" with attributes:
#   nvidia/model: "H100"
#   nvidia/memory: 81920
#   dra-net/speed: "100Gbps"
#   dra-net/ipv4: "10.0.0.5"
#   composite/compositionName: "gpu-nic-pair"
```

</details>

---

**Next:** [04-shadow-claims.md](04-shadow-claims.md) — the core mechanism

**Previous:** [02-the-composition-gap.md](02-the-composition-gap.md) — why this project exists
