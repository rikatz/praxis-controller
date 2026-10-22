# Praxis Gateway API controller implementation plan

Research baseline: 2026-10-19. Status: the first mock-backed HTTP Core control-plane slice is implemented. Repository-local validation results and remaining gaps are recorded below; no real Praxis dataplane or upstream conformance run exists.

The objective is Core conformance for HTTPRoute, GRPCRoute, TLSRoute, TCPRoute and UDPRoute, with independently updated Praxis, AI and policy configuration. Configuration delivery remains undecided. A sidecar/file adapter and a future network control plane must fit the same controller contracts without changing Gateway API interpretation.

## Read in this order

| Document | Purpose |
| --- | --- |
| [Implementation phases](implementation-plan.md) | Scope, dependencies, deliverables, parallel work and completion gates |
| [Architecture and interfaces](architecture.md) | Compilation, independent resources, configurators, lifecycle and acknowledgements |
| [Testing and security](testing-and-security.md) | Test layers, incremental-update assertions, threat model and release gates |
| [File delivery adapter](file-delivery.md) | Opt-in Option A adapter: flags, path layout, delivered-vs-active semantics and limitations |
| [Gateway API research](../research/gateway-api-conformance.md) | Pinned upstream API versions, profiles, test inventory and runner details |
| [Praxis configuration research](../research/praxis-configuration.md) | Source evidence, runtime gaps and prerequisites in Praxis, AI and policy |

## Decisions this plan establishes

- Pin initial research and conformance to Gateway API **v1.6.3**, rather than moving `main`. All five requested Route kinds are Standard-channel `v1` in that release, with corresponding Gateway conformance profiles. Exact tests and qualifications are recorded in the research note.
- Treat Core conformance as both specification behavior and the applicable upstream tests. A green subset is progress; it is not completion or certification. Extended support is outside the initial goal.
- Make the first implementation milestone the complete `GATEWAY-HTTP` Core controller path into a mock configurator. It covers Gateway/listener and HTTPRoute attachment/status, backend resolution, and typed config changes, but serves no traffic. GRPC, TLS, TCP and UDP follow as later slices.
- Separate Kubernetes observation, semantic compilation, independently versioned desired resources, Praxis configuration lowering, delivery, and observed application. Keep Kubernetes objects and transport formats out of the intermediate configuration contract.
- Give clusters stable references to independently versioned endpoint sets. Watch Services and EndpointSlices, not every Pod. A Pod replacement updates only affected endpoint sets, not clusters, routes, listeners, AI or policy roots, or unrelated runtime filters.
- Make the mock stateful per synthetic Gateway target and record sanitized, structured add/update/delete operations for listeners, routes, clusters and endpoints. Gateway reconciliation writes only the Praxis root; AI and policy roots retain independent ownership and revisions.
- Let tests receive an explicit simulated-active receipt from the mock so they can verify status transitions. Label mock runs as simulated; they are not request-serving tests or Gateway API conformance evidence. Production `Programmed` status requires an actual active observation.
- Support listener changes through a provisioner contract. Praxis startup-only listener settings require managed replacement or a future runtime capability; permanently rejecting required listener updates cannot satisfy Core conformance.
- Deliver first through an opt-in file adapter (Option A), selected by `--delivery-adapter=file`. It writes one Praxis YAML document per Gateway atomically and reports delivered, never active. A future control-plane/data-plane protocol (Option B) stays behind the same seam; neither path lifts the runtime-independent-endpoint requirement until Praxis changes or a real observation path exists.

## Current repository and toolchain

The initial repository contained only `AGENTS.md` and `go.mod`. The current code adds the HTTPRoute controller/compiler, typed resource contracts, a stateful mock configurator/provisioner, generated RBAC and envtest coverage. It uses Go `1.27.2`, controller-runtime `v0.25.2`, Kubernetes libraries `v0.37.0` and Gateway API `v1.6.3`.

The required [k8s-controller-runtime skill](https://github.com/rikatz/k8s-controller-skill/blob/main/k8s-controller-runtime/SKILL.md) is installed. Its installed `SKILL.md` was byte-verified against the required upstream source (SHA-256 `d76e05ab511db723d2c174c226711a0685f175dbc26906d1f0aa81ba6ff00302`). The research used its versions, manager, reconcile, status, cache, API design, generation, webhooks, testing, RBAC, metrics and review references. Re-read the relevant references when implementing each phase; their examples do not override Gateway API schemas or conditions.

Use controller-runtime `v0.25.2`, controller-tools `v0.23.0`, aligned `k8s.io/* v0.37.0`, and Go 1.26 or newer, as required by this repository. Both official module files require Go 1.26 and the Kubernetes 0.37 line; the existing Go 1.27.2 declaration satisfies that minimum. Keep the upstream conformance runner in its separate pinned module, since its dependency versions differ. [controller-runtime module](https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.2/go.mod), [controller-tools module](https://github.com/kubernetes-sigs/controller-tools/blob/v0.23.0/go.mod), [Go downloads](https://go.dev/dl/?mode=json).

## What remains open

The delivery transport is selected for Option A (an opt-in file adapter); deployment ownership mode, the runtime incremental resource interface, operational update budgets and eventual AI/policy Kubernetes APIs still need decisions during the phases below. They are explicit work items with completion gates, not assumed capabilities. Nothing in these documents promises existing Praxis can pass all five profiles unchanged. See the source gap inventory before scheduling protocol work.

The file adapter delivers configuration but does not prove runtime application; a control-plane/data-plane protocol (Option B) and real target provisioning remain undecided. GRPCRoute, TLSRoute, TCPRoute and UDPRoute are not implemented. The mock-backed HTTP work does not serve requests and does not establish Gateway API conformance. The generated controller role grants cluster-wide Secret `get/list/watch`; those verbs permit reading Secret data even though the informer is metadata-only and direct reads are reduced to validation metadata before compilation.
