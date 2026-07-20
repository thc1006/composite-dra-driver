# Device Params Examples

ConfigMap templates for opaque driver parameters injected into shadow claims during `PrepareResourceClaims`. The composite driver never interprets their content — it passes them through to underlying drivers.

| File | Use Case |
|------|----------|
| `device-params-static-gateway.yaml` | Uniform gateways across all nodes. Simplest starting point. |
| `device-params-per-node-gateway.yaml` | Per-node gateway overrides (common when nodes have different ToR switches). |
| `device-params-multi-driver.yaml` | Params for multiple underlying drivers (e.g. GPU + NIC) in a single ConfigMap. |
| `poseidon-device-params.yaml` | Production config for Poseidon cluster (8-rail, static gateways). |
| `b200-pf-device-params.yaml` | Production config for B200 PF mode (8-rail, per-node gateway overrides, `sameRailRouteMode: gateway`). |

Set `deviceParams.configMapPath` in your config or `--set deviceParams.configMapName=<name>` in Helm to use.
