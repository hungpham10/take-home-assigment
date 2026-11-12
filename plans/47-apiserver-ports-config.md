# Plan 47 — K8s API server egress ports configurable

**Status:** draft (2026-11-12)  
**Dependencies:** 31 (platform networking config)  
**Goal:** make kube-apiserver egress ports in env `default-deny` NetworkPolicy configurable via environment variable, removing hardcoded `443/6443` assumptions so non-standard endpoints (e.g. k3s/colima node port `60898`) work without code changes.

## Problem

`pkgs/envreconciler/render.go` `defaultDenySpec` hardcodes apiserver egress ports to TCP 443 and 6443 for `K8S_NODE_CIDRS`. In some environments (k3s/colima) the apiserver endpoint is exposed on a different node port (observed: `192.168.5.1:60898`), so initdb/jobs that must reach the Kubernetes API are blocked by NetworkPolicy even when `K8S_NODE_CIDRS` is correct.

Plan 31 made CIDRs/namespaces/ports configurable for mock-service and operator namespace, but apiserver ports remained hardcoded.

## Scope

- Add `K8S_APISERVER_PORTS` (comma-separated TCP ports, default `443,6443,60898`) to `pkgs/config.Config` and `FromEnv` validation.
- Extend `envreconciler.PlatformNet` / `Env.Net` with `K8SAPIServerPorts []int32` and wire through `cmds/loadtest-controller/orchestration.go`.
- Use configured ports in `defaultDenySpec` apiserver egress rule (replace hardcoded 443/6443/60898).
- Update docs: `docs/network-policy.md` (Configuration table + text), `docs/k8s-orchestration.md` if it lists platform networking env vars.
- TDD: config parsing/validation/default tests; envreconciler tests asserting rendered apiserver ports.

## Non-goals

- Auto-detecting apiserver endpoint port from cluster at runtime.
- Making DNS/ingress-nginx/cnpg-system namespace selectors env-configurable in this plan (separate concern).

## Implementation steps

1. **Docs/plan** (this file) + update `docs/network-policy.md`.
2. **Config**: add field + `envPorts` parsing + default; tests for default, override, invalid.
3. **PlatformNet/render**: add field/default/wiring; render ports from config; tests for default and custom ports.
4. **Wire**: `platformNet()` in orchestration includes ports; rebuild/verify.

## Acceptance

- `K8S_APISERVER_PORTS=443,6443` or custom list works; invalid/out-of-range fails `FromEnv`.
- `default-deny` egress to node CIDRs uses configured ports.
- Existing behavior preserved with defaults (includes 443,6443 and 60898 for k3s compatibility).
