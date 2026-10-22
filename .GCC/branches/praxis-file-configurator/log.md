# OTA Log — praxis-file-configurator

Task: plan Option A (per-target Praxis YAML file configurator) without modifying
controller code.

## 2026-10-22 — Initial analysis

### Observation

- The delivery seam is `internal/delivery.Configurator`; the mock is the only
  implementation and the default in `cmd/controller/main.go`.
- `ApplyState` has `active/pending/rejected/deleted`; `delivery.Diff` re-sends
  any resource that is not `ApplyActive` with a matching `ActiveRevision`.
- `allResourcesActive` and the controller's `Programmed` write depend only on
  `ApplyActive`; a file write cannot honestly produce it.
- `internal/config` resources are typed but there is no Praxis YAML lowering.
- Praxis config (`1ea7a79a`) requires listener `address`, supports exact/prefix
  `path`, `host`, `headers`, one `cluster` per route, endpoint weights inside a
  `load_balancer` cluster, and TLS paths. It rejects empty clusters and has no
  cross-cluster route weights.
- The pinned research revision `d1d8e1f3` is absent from the local clone
  (1968 commits, HEAD `1ea7a79a`), so only a spot-check is possible.
- `BackendReconciler` treats non-active as pending and requeues every 5s.

### Thought

- Option A must not fake runtime activation. The smallest contract change is a
  `delivered` state that satisfies `Diff` but not `allResourcesActive`.
- The file adapter needs durable per-target state (including resource values) to
  re-render the whole file for incremental changes and after restart.
- Writing `state.json` before `config.yaml` makes a crash between the two
  self-healing: `Observe` compares digests, reports `pending`, and `Diff`
  re-sends until the config matches.
- Table-level rejection is the only safe first answer for unrepresentable
  rules; partial files can silently drop traffic or produce invalid config.
- UID-scoped target paths preserve existing target-identity semantics and
  prevent cross-incarnation cleanup; the static operator must handle the path
  change on Gateway recreation.

### Action

- Wrote `.GCC/branches/praxis-file-configurator/plan.md`.
- No code, manifests, RBAC or Praxis source modified.

### Next

- Maintainer decision on: private endpoint option, UID-scoped static path,
  and mtime-based fencing vs `flock`.
- Then execute step 1 (delivery contract) and step 2 (pure renderer).

## 2026-10-22 — Execution

### Observation

- Maintainer confirmed: private endpoints opt-in (default off), UID-scoped
  target paths, mtime lease fencing.
- `config.Listener` has no `Name`; the resource key supplies it.
- `config.Value` is implemented on value types, so decoding stored resources
  must return values, not pointers, or the renderer's type switch misses them.
- RBAC generation produced byte-identical output.

### Thought

- Keep the contract change additive and minimal: `ApplyDelivered`,
  `DeliveredRevision`, `Committed`, and one `Diff` condition.
- Serialize adapter operations with one mutex; disk throughput is not a
  bottleneck for this slice.
- Bound owner refreshes to half the stale window so periodic reconciliation
  does not write every 5s.
- Delete the document when nothing remains instead of writing an empty config.

### Action

- Implemented `internal/praxis` renderer and `internal/delivery/file` adapter
  and provisioner; wired `--delivery-adapter=file` with mock as default.
- Added renderer, contract, adapter, provisioner and Programmed-proof tests.
- Updated docs and the research drift note.
- `go test ./...` 73 passed, `go vet ./...` clean, race tests pass, RBAC
  unchanged.

### Next

- Implement scoped destination trust before enabling private endpoints.
- Prove Praxis parsing/reload and TLS rotation on a real target (Option A
  delivery is not runtime application).
- Capabilities gating and an active-observation signal remain Option B work.

## 2026-10-22 — QA/security rework

### Observation

- QA verified with the local Praxis binary that a router with an empty `routes`
  list is rejected (`router: 'routes' is empty`). An accepted listener with no
  routes is a normal state, so the emitted document was unloadable.
- QA also reproduced a wedge: a parseable state entry whose value no longer
  matches its revision made `Observe` report delivered and every later `Apply`
  fail.
- Security flagged fail-open parsing of corrupt owner/state and the missing
  path/symlink validation in `Provisioner.Ensure`.
- Praxis accepts `static_response` with `status: 404` for a listener chain;
  `filter_chains: []` is rejected for HTTP listeners.

### Thought

- A no-route listener must still produce a loadable document and must not
  retain stale routes; a static 404 is the correct fail-closed shape.
- State inconsistency should self-heal from a full Gateway snapshot, while a
  partial endpoint-only apply must stay rejected. Treating bad state as absent
  satisfies both; owner corruption stays fail-closed.
- Sensitive address rejection belongs at render time; RFC1918 Pod IPs are
  allowed by Praxis.

### Action

- Renderer: static 404 for empty route tables, zero listeners rejected,
  sensitive-address rejection (loopback/link-local/this-host/unspecified/
  metadata) unless opted in, empty paths rejected.
- State: unreadable or inconsistent state is treated as absent; owner parse
  errors remain errors.
- Paths: `secureJoin` rejects symlinked/non-directory components; used by the
  configurator and provisioner.
- Tests: golden fixture (validated with `praxis -t`), fuzz target, empty
  routes, sensitive endpoints, missing references, wildcard hosts, restart,
  symlink rejection, inconsistent-state repair, and a real
  `Reconciler.applyDesired` run with the file adapter.
- Gates: 94 tests pass, vet clean, race clean, gofmt clean.

### Next

- Re-run QA on the reworked slice.

## 2026-10-22 — Verification closed

### Observation

- QA re-ran the gates and independently reproduced the empty-route, state
  repair, symlink and sensitive-address paths with the real Praxis binary:
  PASS / RELEASE, 95/100, no blocker or major issue.
- Remaining minor items were coverage of `internal/delivery`, doc drift on
  `Observe.Connected`, a delete-receipt wording mismatch, and missing cheap
  regression tests.

### Action

- Added direct tests for the delivery revision helpers and receipt validation,
  which raised `internal/delivery` coverage from 52.9% to 79.4%.
- Added adapter regressions: deleting a referenced cluster is rejected without
  touching the file, retired-epoch deletes are accepted during takeover,
  duplicate apply leaves no temp files, and a truncated config is repaired.
- Corrected `Observe.Connected` and delete-receipt wording in the plan and
  docs, and labelled the `praxis -t` golden validation as a manual gate.
- Final gates: 100 tests pass in 8 packages; vet, race and gofmt clean; RBAC
  SHA unchanged.

### Next

- Followups: scoped destination trust for the global private-endpoint flag, a
  tombstone/atomic fence for shared roots, and Option B active observation.
