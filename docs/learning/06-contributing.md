# Contributing

## Build and Test

```bash
make build          # Build both driver and webhook binaries
make test           # Run all tests (go test ./... -v)
make lint           # Run go vet ./...
make image          # Build container images (podman)
```

For a single package:
```bash
go test ./pkg/synthesizer/... -v -run TestPairWithMatchAttribute
```

CI runs with the race detector: `go test ./... -v -race`

## CI

Three GitHub Actions workflows in `.github/workflows/`:

- **`ci.yaml`** — runs on every PR: build, test, lint
- **`build-pr.yaml`** — builds container images for PR verification
- **`build-push.yaml`** — builds and pushes images on merge to main

## Good First Issues

These are well-scoped and don't require deep architectural knowledge:

**[Observability (#18)](https://github.com/openshift-psap/composite-dra-driver/issues/18)** — Add Prometheus metrics and Kubernetes events. The driver currently has no metrics. Standard `prometheus/client_golang` instrumentation for pairing counts, shadow claim latency, and gRPC call duration.

**[Blast radius isolation (#35)](https://github.com/openshift-psap/composite-dra-driver/issues/35)** — Per-composition ConfigMap validation with partial startup. Today a single invalid composition in the config prevents the entire driver from starting. Fix: validate each composition independently, start with the valid ones, log errors for the broken ones.

**[Attribute deduplication (#4)](https://github.com/openshift-psap/composite-dra-driver/issues/4)** — Dedup rules in `buildCompositeDevice()` in `pairer.go`. When two sources forward the same attribute (e.g., `pcieRoot`), the composite device has a redundant copy. Add config for dedup rules.

## Intermediate Issues

These require understanding the shadow claims pattern and synthesizer pipeline:

**[Cross-composition device exclusion (#28)](https://github.com/openshift-psap/composite-dra-driver/issues/28)** — When a GPU appears in both `gpu-nic-pair` and `gpu-only` composition pools, the scheduler can double-allocate the same physical GPU from different pools. Two approaches under discussion: pairer-side static partitioning vs. continuous synthesizer recomputation. See [RFC.md § D3](../RFC.md#d3-how-should-cross-composition-device-exclusion-be-solved) for the design options.

**[VF support + external IPAM (#34)](https://github.com/openshift-psap/composite-dra-driver/issues/34)** — Virtual Functions lack IP attributes at pairing time. Requires integrating an external IPAM controller to assign addresses before the composite device can be published. PF mode works today.

**[Consumable capacity (#21)](https://github.com/openshift-psap/composite-dra-driver/issues/21)** — CPU/memory DRA drivers use grouped mode with capacity fields (`AllowMultipleAllocations`, `Capacity`). The composite driver only handles discrete devices today. Supporting capacity-based resources requires a new abstraction. Investigation doc at `docs/investigations/cpu-memory-compat.md`.

## Adding a New Underlying Driver

Adding a new underlying driver is a **config change, not a code change**. Add a source entry with the driver name, DeviceClass, and attribute forwarding rules:

```yaml
sources:
- name: fpga
  driver: fpga.example.com
  deviceClassName: fpga.example.com
  forwardAttributes:
  - domain: fpga.example.com
    attributes: [model, frequency]
```

Then reference it in a composition. The composite driver has been validated with `gpu.nvidia.com` and `dra.net` but should work with any DRA driver that implements the standard kubelet plugin gRPC interface.

## Upstream KEPs

Two Kubernetes enhancements shape this project's roadmap:

**[KEP-5732 — Topology-Aware Scheduling](https://github.com/kubernetes/enhancements/issues/5732)** — Adds device packing policies to the scheduler. Alpha in K8s 1.36, DRA integration deferred to 1.37+ beta. Would reduce fragmentation for the composite driver's published devices.

**[KEP-5729](https://github.com/kubernetes/enhancements/issues/5729)** — Confirmed per-replica unique claims out of scope. Affects multi-replica workloads requesting composite devices. The composite driver's fast synthesizer cycle (500ms) mitigates this in practice.

## Before Proposing Architectural Changes

Read [RFC.md](../RFC.md). All architectural decisions (shadow claims, pairing algorithm, config schema) and their rationale are documented there with alternatives considered and risks. The [FAQ.md](../FAQ.md) covers the 15 most common questions.

---

**Previous:** [05-code-walkthrough.md](05-code-walkthrough.md) — source code orientation

**Start over:** [00-start-here.md](00-start-here.md)
