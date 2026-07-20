# Composite DRA Driver — FAQ

## Architecture

### Why not just use the existing webhook?

The webhook pins pods to specific nodes and makes allocation decisions outside the scheduler. This creates problems: the scheduler doesn't know what the webhook decided, can't optimize across workloads, and can't rebalance. The composite driver presents devices to the scheduler as first-class ResourceSlices — the scheduler allocates natively, same as any other DRA device.

### Why shadow claims instead of reimplementing nvidia/dranet logic?

Council review (codex + gemini) confirmed: reimplementing driver internals is fragile — breaks on upstream changes. Shadow claims delegate to the real drivers via their existing gRPC sockets. The composite driver is a pure orchestrator — zero nvidia/dranet code. Validated that neither driver checks if claims are in `pod.spec.resourceClaims`.

### Why not use gRPC passthrough without real claims?

The kubeletplugin.Helper fetches the ResourceClaim from the API server by namespace/name/UID before passing it to the driver. Synthetic claims that don't exist in the API server would fail this lookup. Shadow claims are real claims — they pass all validation.

### Does this need to be reimplemented for each new DRA driver?

No. Adding a new underlying driver is a config change — add a source entry with driver name, DeviceClass, and attribute forwarding rules. No code changes. The pairing, shadow claim creation, and gRPC delegation are all generic.

### What could break across driver versions?

Only if a driver starts validating that claims appear in `pod.spec.resourceClaims`, or K8s changes the DRA gRPC protocol. Both would be breaking changes affecting the entire DRA ecosystem, not just us.

## Scheduling

### Does the scheduler know about NUMA?

No. The scheduler has zero NUMA awareness for DRA devices. NUMA enforcement in K8s happens at the kubelet level (Topology Manager), and DRA is not integrated with it. The composite driver publishes `composite/numaNode` as a device attribute — users can add `MatchAttribute` constraints explicitly in ResourceClaimTemplates, but the scheduler won't enforce NUMA automatically.

### Why did you remove NUMA constraints from the webhook?

The `MatchAttribute` constraint is a hard requirement, not best-effort. If a user requests 4 pairs on the same NUMA and no single NUMA zone has 4 free pairs (but 4 exist across zones), the pod stays Pending forever. The old webhook avoided this by scanning ResourceSlices and falling back to cross-NUMA — we don't scan ResourceSlices. NUMA is now opt-in only. See Discussion #11.

### What happens when a node is NotReady?

The node gets a `node.kubernetes.io/unreachable:NoExecute` taint. The scheduler's `TaintToleration` plugin filters it out before DRA allocation — composite ResourceSlices on that node won't be used for new pods. Stale ResourceSlices persist (owned by Node object) but are harmless. ~40s window between actual failure and taint application.

### How does the webhook differ from the old one?

The old webhook scanned ResourceSlices, picked a node, picked specific devices, and pinned the pod via nodeAffinity. The new webhook only generates ResourceClaimTemplates with DeviceRequests — no ResourceSlice scanning, no node selection, no device selection, no pinning. All scheduling decisions stay with the scheduler.

## Networking

### How are rails handled?

Each composite device inherits its NIC's IP, which determines the rail. The `DeviceParamsResolver` matches device attributes against entries in an external ConfigMap and generates opaque parameters for the shadow claim via Go template substitution. The ConfigMap is maintained externally (by admin, script, or operator) — the composite driver never interprets the params content. dranet's NRI hook applies this config when setting up the NIC in the pod netns. See `charts/composite-dra-driver/examples/` for ConfigMap templates.

### Why are cross-rail routes in the main routing table?

Per-rail policy rules (`from 10.X.0.0/16 lookup table 10X`) divert same-rail traffic to the per-rail table. Cross-rail routes must be in the main table so they're reachable from any source IP going through the per-rail default gateway. Matches the original webhook's template pattern.

### Is the routing config coupled to dranet?

No — it was decoupled in PR #32. The `DeviceParamsResolver` reads an external ConfigMap, matches device attributes against entries using CEL selectors, and generates opaque parameters via Go template substitution. The composite driver never interprets the param content. See `charts/composite-dra-driver/examples/` for ConfigMap templates.

## Performance

### How long does Prepare take?

For 8 GPU-NIC pairs (16 shadow claims): ~3 seconds. Shadow claim creation is 15ms (parallel). The remaining ~3s is nvidia's CDI spec generation, which is serialized internally in the nvidia driver. This is the floor.

### Why was parallel creation slow initially?

client-go defaults to QPS=5, Burst=10. 16 concurrent API calls were throttled to 5/s = 3.2s. Fixed by bumping to QPS=100, Burst=200. Shadow creation went from 3s to 15ms.

### Can we parallelize nvidia CDI gen?

Not from our side — nvidia's `PrepareResourceClaims` serializes internally. Each GPU CDI spec takes ~250ms. Would need nvidia driver changes. Combined shadow claims (#7) could reduce the number of gRPC calls.

## Deployment

### Why DaemonSet, not Deployment?

DRA kubelet plugins register via unix sockets at `/var/lib/kubelet/plugins/<driver>/dra.sock` — must be on the same node as kubelet. ResourceSlices are per-node. PrepareResourceClaims is called by local kubelet. A Deployment can't satisfy these constraints.

### Can the webhook have HA?

Yes — runs as a 2-replica Deployment behind a Service. Stateless (intercepts synthetic resource request, creates template, patches pod). The `composite.dra/mutated` annotation prevents re-processing on retries. Active-standby with leader election is discussed in Discussion #10.

### Why does it need privileged SCC on OpenShift?

The driver needs hostPath volumes for kubelet plugin sockets (`/var/lib/kubelet/plugins/`) and BoltDB state (`/var/lib/composite-dra/`). SELinux labels on hostPath directories require privileged access for writes.

## Compatibility

### What K8s versions are supported?

1.34+ (compiled against v0.35.3 client modules). The `resource.k8s.io/v1` API is stable across 1.34+.

### When can we drop the webhook entirely?

K8s 1.36+ (where `DRAExtendedResource` is beta and on by default). Users write `resources.requests: {"composite.dra.example.io/gpu-nic-pair": "4"}` — no webhook, no templates. Only keep the webhook if NUMA constraints are needed.

### Why do I need to set both requests and limits for the synthetic resource?

Kubernetes requires extended resources to have `limits == requests`. If you only set `requests`, controllers like StatefulSet and LeaderWorkerSet will fail to create pods because the API server rejects the pod template. The webhook strips both at Pod CREATE time — they never reach the scheduler.

### Does it work with LeaderWorkerSet (LWS)?

Yes, as long as both `requests` and `limits` are set for the synthetic resource in the pod template. LWS creates a StatefulSet, which validates the pod template at creation time. The webhook only fires on actual Pod CREATE, so the template must pass API server validation first.

### Does it work with the nvidia GPU Operator?

Yes. The composite driver reads nvidia's ResourceSlices and creates shadow claims that nvidia's kubelet plugin processes normally. No changes to the GPU Operator needed. CDI specs are generated by nvidia, not by us.
