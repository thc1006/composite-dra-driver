# Composite DRA Driver — High Availability Design

## Requirements

1. Must run on control-plane/master nodes
2. Graceful failover when a node goes down
3. Shadow claim orphan cleanup
4. No split-brain or duplicate device allocation

## Architecture: Per-Node DaemonSet

The composite driver runs as a **DaemonSet** — one pod per node. This is the standard DRA driver deployment model (same as nvidia, dranet). Each instance only manages devices on its local node.

### Why DaemonSet, Not Deployment

- DRA kubelet plugins register via unix sockets at `/var/lib/kubelet/plugins/<driver>/dra.sock`
- Kubelet calls PrepareResourceClaims on the LOCAL node's driver instance
- ResourceSlices are per-node (Spec.NodeName)
- A Deployment would need affinity rules to achieve the same, adding complexity

### Control-Plane Scheduling

Already configured in `deploy/daemonset.yaml`:

```yaml
tolerations:
  - key: node-role.kubernetes.io/control-plane
    operator: Exists
    effect: NoSchedule
  - key: node-role.kubernetes.io/master
    operator: Exists
    effect: NoSchedule
priorityClassName: system-node-critical
```

The driver runs on ALL nodes including control-plane. `system-node-critical` priority prevents eviction during resource pressure.

## Failure Scenarios & Handling

### 1. Driver Pod Crash/Restart (Same Node)

**What happens:**
- kubeletplugin.Helper deregisters, ResourceSlices for this node are cleaned up by the controller
- BoltDB state persists at `/var/lib/composite-dra/state.db` (hostPath volume)
- On restart: `restoreFromState()` reloads shadow claim → composite claim mappings
- Synthesizer recomputes and republishes ResourceSlices
- kubeletplugin.Helper re-registers with kubelet

**Already handled:** BoltDB persistence + restoreFromState() in plugin.go.

### 2. Worker Node Goes Down

**What happens:**
- Node marked NotReady after `node-monitor-grace-period` (default 40s)
- Pods on that node enter Terminating state
- ResourceSlices for that node are eventually garbage collected (owned by Node object)
- Composite claims allocated to devices on that node will fail
- Shadow claims have OwnerReferences to composite claims → GC cascade

**Scheduler behavior:**
- Scheduler won't allocate composite devices from a missing node's ResourceSlices (deleted)
- Pods waiting for composite devices on the failed node will be rescheduled to other nodes
- The scheduler handles this natively — no composite driver involvement needed

**Shadow claim cleanup:**
- OwnerReferences on shadow claims point to composite claims
- When composite claims are deallocated/deleted, shadow claims cascade-delete
- For orphaned shadows (OwnerReference target deleted but shadow remains): add reconciler

### 3. Control-Plane Node Goes Down

**What happens:**
- The composite driver pod on that node dies
- Other nodes' composite driver pods continue operating independently
- The synthesizer on the failed node stops publishing ResourceSlices → devices on that node become unavailable
- API server HA (multi-master) ensures the control plane remains functional
- When the node recovers, the DaemonSet controller reschedules the pod

**No special handling needed** — each node is independent.

### 4. Shadow Claim Orphans

Shadow claims can become orphaned if:
- Composite driver crashes between creating a shadow claim and persisting to BoltDB
- Node goes down and OwnerReference GC hasn't run yet
- Race between Unprepare and API server GC

**Solution: Orphan Reconciler**

A periodic reconciler (runs every 5 minutes in the driver pod) that:
1. Lists all shadow claims with label `app.kubernetes.io/managed-by=composite.dra.llm-d.io`
2. For each shadow claim, checks if the composite claim (from `composite-claim-uid` label) still exists
3. If composite claim is gone → delete shadow claim
4. If composite claim exists but is not allocated → delete shadow claim

### 5. Underlying Driver Unavailable

If nvidia or dranet gRPC socket is unreachable during PrepareResourceClaims:
- The composite driver returns an error for that claim
- Kubelet retries PrepareResourceClaims later
- Standard DRA retry behavior — no special handling

### 6. ResourceSlice Stale During Recompute

If an underlying device disappears between allocation and Prepare:
- PrepareResourceClaims will fail (underlying driver rejects the shadow claim)
- Kubelet reports the pod as unschedulable
- Pod is rescheduled to another node
- Self-healing — no intervention needed

## Reconciler Implementation

```go
// pkg/plugin/reconciler.go
func (p *CompositePlugin) StartReconciler(ctx context.Context, interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            p.reconcileOrphans(ctx)
        }
    }
}

func (p *CompositePlugin) reconcileOrphans(ctx context.Context) {
    // List shadow claims managed by this driver
    // Check if parent composite claim still exists and is allocated
    // Delete orphans
}
```

## What We Get For Free (From K8s)

| Concern | Handled By |
|---------|-----------|
| DaemonSet pod restart | DaemonSet controller |
| Node failure detection | node-lifecycle controller (40s grace) |
| ResourceSlice GC on node loss | ResourceSlice controller (Node OwnerReference) |
| Shadow claim cascade delete | K8s garbage collector (OwnerReference) |
| Pod rescheduling after node failure | Scheduler (node goes NotReady → pods evicted → rescheduled) |
| Prepare retry on transient failure | Kubelet DRA manager |
| Plugin re-registration | kubeletplugin.Helper (re-registers on startup) |

## What We Must Implement

| Concern | Implementation |
|---------|---------------|
| State persistence across restarts | BoltDB at hostPath ✅ done |
| Shadow claim orphan cleanup | Reconciler loop (5-min interval) ✅ done (`pkg/plugin/reconciler.go`) |
| Graceful shutdown (unprepare in-flight) | Context cancellation in main.go ✅ done |
| Master node scheduling | DaemonSet tolerations ✅ done |
| System-critical priority | priorityClassName: system-node-critical ✅ done |

## Rolling Update Strategy

The DaemonSet uses `RollingUpdate` with `maxUnavailable: 1`. During upgrades:
- kubeletplugin.Helper supports rolling updates: new instance registers while old is still running
- The Helper serializes gRPC calls to prevent concurrent Prepare for the same claim
- State is on hostPath, so the new pod reads the same BoltDB

## Summary

The HA story is mostly handled by K8s primitives (DaemonSet, OwnerReference GC, kubelet retry, scheduler rescheduling). The composite driver needs:
1. ✅ BoltDB persistence for crash recovery
2. ✅ Control-plane tolerations
3. ✅ system-node-critical priority
4. ✅ Orphan shadow claim reconciler (5-min loop, `pkg/plugin/reconciler.go`)
5. ✅ Graceful shutdown via context cancellation
