# Agent Guidance

This file provides guidance to coding agents when working with code in this
repository. This repository is part of the praxis project and follows the same conventions
as the proxy's AGENTS.md in `https://github.com/praxis-proxy/praxis`; this
file restates the project-wide rules that apply here and adds the
controller-specific ones.

Tools may assist with implementation, but **do not add any tool as a commit
collaborator, co-author, or signatory**. Commit sign-off belongs to the human
contributor responsible for the change.

## Repository

Gateway API controller for praxis (`https://github.com/praxis-proxy/praxis`),
a Rust proxy framework. Go module: `github.com/praxis-proxy/controller`.

## Mandatory: k8s-controller-runtime skill

All work in this repository — API/CRD types, reconcilers, codegen, RBAC,
webhooks, metrics, tests, review — MUST follow the `k8s-controller-runtime`
skill. It MUST be installed from
`https://github.com/rikatz/k8s-controller-skill` and called before writing or
reviewing any controller code; follow its reference files per its "Which
reference when" table. Target stack: controller-runtime v0.25.2,
controller-tools v0.23.0, k8s.io/* v0.37, Go 1.26+.

## Commits and Pull Requests

Same rules as the proxy repository, except that the proxy's 500-line PR cap and
PR-description requirement are withdrawn for this repository (authorized by the
architect):

- Conventional commit subjects: `type(scope): summary`, with type one of
  `build`, `chore`, `ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `test`,
  and the summary at most 72 characters.
- Every commit is signed off (`git commit -s`) and cryptographically signed
  (GPG or SSH), by the human responsible for it. No AI tool as author,
  co-author, or signatory.

## Project Context

- Proxy being driven: `https://github.com/praxis-proxy/praxis` (Rust,
  config-driven; clone it when you need its internals). It hot-reloads filter
  pipelines and routing rules from its config file; listener addresses/protocols
  and TLS on/off are NOT hot-reloadable — Gateway API objects that change those
  need a proxy restart or rejection. In that repository: architecture in
  `docs/architecture/overview.md`, config reference in
  `docs/operating/configuration.md`, example configs in `examples/configs/`.
- Gateway API upstream: `https://github.com/kubernetes-sigs/gateway-api`
  (clone it for API-shape and conformance questions; main ≈ v1.6.x).
