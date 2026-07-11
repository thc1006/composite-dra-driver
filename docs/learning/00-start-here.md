# Start Here

A guided learning path for contributors to the composite DRA driver. Takes about 60-90 minutes including hands-on exercises.

## Prerequisites

- Kubernetes concepts: pods, claims, controllers, CRDs
- Go 1.26+ for building and testing
- `kubectl` access to a cluster (optional — exercises include expected output for readers without hardware)
- Familiarity with reading Go code

## Reading Order

| # | Document | Time | What you'll learn |
|---|----------|------|-------------------|
| 01 | [DRA Primer](01-dra-primer.md) | 5 min | DRA from zero — ResourceSlices, claims, CEL selectors, allocation lifecycle |
| 02 | [The Composition Gap](02-the-composition-gap.md) | 4 min | Why GPU-NIC pairing matters, what DRA can't do, why this project exists |
| 03 | [How It Works](03-how-it-works.md) | 6 min + Exercise | Architecture overview, synthesizer pipeline, configuration |
| 04 | [Shadow Claims](04-shadow-claims.md) | 5 min + Exercise | The core mechanism — how the driver delegates to underlying drivers |
| 05 | [Code Walkthrough](05-code-walkthrough.md) | 8 min + Exercise | Package map, data flow, key functions, testing |
| 06 | [Contributing](06-contributing.md) | 4 min | Build, test, good first issues, upstream KEPs |

Exercises add ~30 minutes. Exercise 3 (code reading) works without a cluster.

## Deeper Reading

These existing docs go deeper than the learning path. Read them when you're ready:

- **[RFC.md](../RFC.md)** — Full design proposal. Motivation, alternatives considered, risks, decisions. Read before proposing architectural changes.
- **[ARCHITECTURE.md](../ARCHITECTURE.md)** — Comprehensive reference. Expressiveness matrix, gap analysis, shadow claim lifecycle validation, config schema, performance data.
- **[FAQ.md](../FAQ.md)** — 15 questions covering architecture decisions, scheduling, networking, performance, deployment.
- **[HA-DESIGN.md](../HA-DESIGN.md)** — Failure scenarios, crash recovery, rolling updates.
- **[CHEATSHEET.md](../CHEATSHEET.md)** — Install, request methods, verify, troubleshoot.

## Glossary

**CDI** — Container Device Interface. A standard for injecting device nodes, environment variables, and mounts into containers. DRA drivers return CDI device IDs during Prepare; the container runtime applies them.

**CEL selector** — A Common Expression Language expression that filters devices by attribute values (e.g., `device.attributes["gpu.nvidia.com"].memory >= 80`). Evaluated by the scheduler against every candidate device.

**Composite device** — A virtual device published by the composite driver representing a pre-validated grouping of underlying devices (e.g., a GPU-NIC pair on the same PCIe root).

**Composition** — A configured pairing rule defining which source drivers to combine, how many devices from each, and what topology constraint to enforce. Defined in the driver's config YAML.

**DeviceClass** — A cluster-scoped Kubernetes object naming a category of devices and identifying which driver manages them.

**DRA kubelet plugin** — A gRPC interface drivers implement on each node. Two methods: `NodePrepareResources` (set up devices) and `NodeUnprepareResources` (tear down).

**matchAttribute** — A DRA constraint type requiring multiple devices in a claim to share the same value for a named attribute (e.g., `resource.kubernetes.io/pcieRoot`).

**Opaque device params** — Driver-specific configuration (JSON/YAML) attached to a ResourceClaim's allocation. The composite driver uses Go templates to render per-device config at Prepare time.

**Pairing** — The process of grouping devices from different source drivers into composite devices based on topology constraints.

**ResourceClaim** — A namespace-scoped Kubernetes object representing a workload's request for devices. Contains device requests, CEL selectors, and constraints.

**ResourceClaimTemplate** — Creates a new ResourceClaim per pod. Used when each pod needs its own devices.

**ResourceSlice** — A cluster-scoped Kubernetes object where a DRA driver publishes its devices and their typed attributes. The scheduler reads these to find allocation candidates.

**ResourceSlice splitting** — Kubernetes limits ResourceSlices to 128 devices. The composite driver's publisher automatically chunks larger pools into multiple slices.

**Shadow claim** — A real ResourceClaim created by the composite driver with pre-filled allocation status, used to delegate device preparation to an underlying driver via its gRPC socket.

**Underlying driver** — A real hardware DRA driver (e.g., `gpu.nvidia.com`, `dra.net`) that the composite driver orchestrates through shadow claims.

---

**Next:** [01-dra-primer.md](01-dra-primer.md) — DRA fundamentals
