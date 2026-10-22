# Plan: Praxis file configurator (Option A)

Status: IMPLEMENTED and verified — QA re-verification PASS (95/100), security review SECURE for the default deployment
Owner: @architect, executed by @engineer
Target repository: `github.com/praxis-proxy/controller`
Scope: HTTP/HTTPS Gateway API Core slice, delivery only. No request-serving
claim. No new CRDs, no Praxis source changes, no second transport.

## 1. Summary

Implement **Option A**: a delivery adapter/configurator that lowers the existing
typed Praxis resources (`config.Listener`, `config.HTTPRouteTable`,
`config.Cluster`, `config.EndpointSet`, `config.SecretHandle`) into one Praxis
YAML configuration file per target and writes it atomically to a configurable
root. The mock remains the default adapter. The file adapter is opt-in in the
same binary.

The adapter proves **delivery**, not runtime application. A successful atomic
rename means bytes reached the target path; it never means Praxis parsed,
accepted, reloaded, or served them. `Gateway.status.conditions[Programmed]`
therefore stays `False/Pending` for the file adapter. The `delivered` state
added to the delivery contract is the minimum change needed to stop the
controller re-sending unchanged resources every `deliveryRecheck` while keeping
`allResourcesActive` and `Programmed` gated on runtime evidence.

Option B (a control-plane to data-plane protocol with real ACK/NACK and active
revision observation) remains a documented followup behind the same
`delivery.Configurator` seam. It is not implemented here.

## 2. Decisions taken

| Decision | Value |
| --- | --- |
| Delivery shape | One YAML file per target (`<root>/<namespace>/<gateway>/<uid>/`), at least one root per target. Additional AI/policy roots are not written. |
| Default adapter | `mock` unchanged. `file` is selected by flag. |
| Runtime application | Not claimed. File writes produce `delivered`, never `active`. |
| Programmed | Remains `False/Pending` under the file adapter until a future adapter reports `active`. |
| Provisioner mode | Pre-existing/static target. No managed Deployment/Service ownership in this slice. |
| Private endpoints | Opt-in only (`--praxis-allow-private-endpoints`, default off). |
| Fencing | mtime-based owner lease with `--praxis-owner-stale-after` (default 1m), acceptable behind leader election. |
| TLS material | Paths only, never bytes. Configurable mount root. Rotation/restart is a documented limitation. |
| Capabilities | Advertised honestly (`IndependentEndpointSets=false`) but not used to gate status yet; followup below. |
| Option B | Documented followup, no code. |

## 3. Obstacles and assumptions

1. **No lowering exists.** `internal/config` resources have no Praxis YAML
   mapping. A pure renderer is required (section 4.3).
2. **Delivery vs active.** `ApplyState` has only `active/pending/rejected/deleted`,
   and `delivery.Diff` re-sends anything that is not `ApplyActive`. A file write
   would be re-applied forever. A `delivered` state is required (section 4.2).
3. **Whole-file reload.** Praxis re-reads one main YAML and rebuilds all
   listener pipelines. Independent endpoint updates are not satisfied by this
   adapter. `Capabilities.IndependentEndpointSets` must be `false`. This slice
   optimizes controller writes, not runtime pipeline identity.
4. **Not representable in the first slice.** Route-level weights across
   different clusters, redirects, per-rule request-header modifiers,
   `no-backend`/`backend-error` actions, zero-endpoint clusters, and multiple
   listeners sharing `address:port`. These are rejected at table/config
   granularity, never silently omitted (section 4.3.4).
5. **Private backend addresses.** Praxis validates cluster endpoints; normal
   Pod IPs are private and require `insecure_options.allow_private_endpoints:
   true`. The renderer does not emit that insecure option by default. Whether
   to opt in is a maintainer decision (section 8).
6. **TLS rotation.** The listener `tls` block is restart-only in Praxis.
   Certificate file content may be hot-reloaded only under the `config-reload`
   build feature and with a single certificate. This slice declares rotation
   unsupported/unproven and requires restart.
7. **Schema drift.** The local Praxis clone is at `1ea7a79a` (2026-10-09); the
   research pinned `d1d8e1f3`, which is not present in the local history, so a
   full diff cannot be produced. Facts were verified against the local HEAD and
   are listed in section 9.1. The pinned revision must be fetched and re-checked
   before release.
8. **Static target path.** Including the Gateway UID in the path prevents stale
   writers and cross-incarnation deletes, but an externally started Praxis
   instance must be pointed at a path that changes when a Gateway is recreated.
   This is acceptable for an explicitly static pre-existing target and is an
   open operational decision (section 8).
9. **Target identity.** `Target` is namespace/name/UID. `Provisioner.Ensure`
   must echo exactly that target; the controller rejects a mismatch.

## 4. Technical blueprint

### 4.1 Seam and package boundaries

Reuse the existing seam. Do not add a plugin framework.

| Package | Change |
| --- | --- |
| `internal/delivery` | Add `ApplyDelivered`, `DeliveredRevision`, committed-state helpers, and make `Diff` consider delivered revisions. |
| `internal/praxis` (new, reserved by architecture) | Pure, I/O-free lowering from `[]config.Resource` to deterministic Praxis YAML. |
| `internal/delivery/file` (new) | File adapter: state store, atomic publication, owner-epoch fencing, observation, static provisioner. |
| `internal/delivery/mock` | Unchanged. |
| `internal/controller` | No production logic change expected; add tests proving delivered is not active and that controller reconciliation does not rewrite unchanged files. |
| `cmd/controller` | Flags and adapter selection; mock default. |

The reconciler never calls the renderer. `internal/praxis` does no filesystem or
network I/O and is golden-tested independently.

### 4.2 Delivery contract change (`internal/delivery/configurator.go`)

```go
const ApplyDelivered ApplyState = "delivered"

type ResourceResult struct {
    Key               config.Key `json:"key"`
    DesiredRevision   string     `json:"desiredRevision,omitempty"`
    DeliveredRevision string     `json:"deliveredRevision,omitempty"`
    ActiveRevision    string     `json:"activeRevision,omitempty"`
    State             ApplyState `json:"state"`
    Reason            string     `json:"reason,omitempty"`
}
```

Add:

```go
// Committed reports whether the adapter has durably taken this revision.
// Active is a stronger claim than Delivered; only Active satisfies Programmed.
func Committed(result ResourceResult, revision string) bool
func (r Receipt) DeliveredRevision(key config.Key) (string, bool)
func (o Observation) DeliveredRevision(key config.Key) (string, bool)
```

`Diff` changes exactly one condition:

```go
if !exists || !Committed(active, resource.Revision) {
    changes.Upserts = append(changes.Upserts, resource)
}
```

Rules:

- `ApplyActive` + `ActiveRevision == revision` -> committed.
- `ApplyDelivered` + `DeliveredRevision == revision` -> committed.
- `ApplyPending`, `ApplyRejected`, `ApplyDeleted` -> not committed, re-sent.
- `ActiveRevision` helpers keep returning only `ApplyActive`.
- `allResourcesActive` and `simulatedReceiptReady` remain unchanged, so a
  delivered file can never produce `Programmed=True`.
- The mock never emits `ApplyDelivered`; its behavior and tests are unchanged.

No change to `Configurator`, `Provisioner`, `ChangeSet` or `Target`.
`BackendReconciler` continues to requeue every 5s while the target is not
active, but `Diff` returns an empty change set after delivery, so there are no
writes. Replacing that polling with a future active-observation signal is an
Option B followup.

### 4.3 Praxis lowering mapping (`internal/praxis/render.go`)

Entry point:

```go
type Options struct {
    ListenerBindAddress string // default "0.0.0.0"
    TLSRoot             string // default "/etc/praxis/tls"
    AllowPrivateEndpoints bool // default false; emits insecure_options only when true
}

func Render(resources []config.Resource, opts Options) ([]byte, error)
```

Only `DomainPraxis` resources are accepted; any AI/policy/opaque root is an
error. Output is built from ordered structs and sorted slices (not Go maps) and
marshaled with `go.yaml.in/yaml/v3` (already in the module graph; promote to a
direct dependency). No file I/O.

#### 4.3.1 Mapping table

| Internal resource / field | Praxis YAML | Rules / gaps |
| --- | --- | --- |
| `Listener{Port,Protocol="HTTP"}` + `Resource.Key.Name` | `listeners[].name`, `address: "<bind>:<port>"`, `filter_chains: ["route-<name>"]` | The listener name comes from the resource key; `config.Listener` has no `Name` field. `address` is mandatory in Praxis; the bind address is adapter config, not Gateway API state. |
| `Listener{Protocol="HTTPS"}` | same + `tls.certificates[0].cert_path`, `key_path` | Exactly one SecretRef is produced by the compiler. Paths derived from the ref, never Secret bytes. |
| `SecretHandle` | no direct emission; only the paths above | UID and resourceVersion are never emitted, logged or persisted in the YAML. |
| `HTTPRouteTable` | `filter_chains[].filters[]: {filter: router, multi_level_subdomain_matching: true, routes: [...]}` | Rules emitted in `Priority` order (declaration order encodes ties). `multi_level_subdomain_matching: true` is required for Gateway API wildcard semantics. |
| `HTTPRule.Matches[i].PathType="Exact"` | `routes[].path` | `""`/`/` default from the compiler is valid. |
| `HTTPRule.Matches[i].PathType="PathPrefix"` | `routes[].path_prefix` | |
| `HTTPRule.Matches[i].Headers` | `routes[].headers: {name: value}` | Exact match only; compiler lowercases names. Praxis documents case-sensitive matching; verify against runtime later. |
| `HTTPRule.Hostnames` | `routes[].host` | Compiler emits zero or one host per rule. |
| `HTTPAction{Kind="forward"}` with one valid backend | `routes[].cluster: <cluster key name>` | Exactly one valid backend required. |
| `Cluster` + `EndpointSet` | `filters[]: {filter: load_balancer, clusters: [{name, endpoints: ["ip:port", ...]}]}` | Duplicated into every listener chain that references the cluster. Only endpoints with `Ready && Serving && !Terminating` are emitted. Cluster must have at least one endpoint (Praxis rejects empty clusters). |
| Cluster key/name | `clusters[].name` and `routes[].cluster` | Use the existing stable `service-<port>-tcp-<hash>` name. |

Not representable in this slice, rejected with a deterministic reason:

| Input | Why | Handling |
| --- | --- | --- |
| `ActionForward` with >1 valid backend, or any weight split across clusters | Praxis routes select one cluster; endpoint weights are intra-cluster only | Reject the whole `HTTPRouteTable` resource. |
| `ActionRedirect`, `ActionNoBackend`, `ActionBackendError`, `ActionUnsupported` | No faithful per-rule mapping in this slice | Reject the table. |
| Per-rule `RequestHeaders` | Filter `conditions` cannot precisely exclude a more specific rule's request; applying it to a broader match would leak header mutation | Reject the table. |
| Backend with zero ready/serving endpoints | Empty Praxis clusters are invalid | Reject the table; do not write a config Praxis cannot validate. |
| Endpoint that is loopback, link-local, this-host, unspecified or a cloud metadata address | Praxis rejects it at load time unless private endpoints are explicitly enabled | Reject the render by default; the opt-in flag permits it. |
| Two listeners with the same bind address and port | Praxis listeners bind `address:port`; merging chains/multi-cert is out of scope | Reject the whole render. |
| Multiple TLS certificate refs / TLS options | Compiler already rejects outside HTTP Core; renderer also rejects | Reject. |
| Unsupported `config.Kind` or non-Praxis domain | Out of scope | Reject. |
| Reference to a missing route table/cluster/endpoint set | Would produce dangling YAML | Reject. |

"Omitting" is never allowed for a desired resource in this slice: a partial
config could route traffic to the wrong place or silently drop a rule. Rejection
is table/config granular and no file is written.

#### 4.3.2 TLS path convention

```
<Options.TLSRoot>/<secretNamespace>/<secretName>/tls.crt
<Options.TLSRoot>/<secretNamespace>/<secretName>/tls.key
```

Namespace and name come from `config.SecretRef`; validate with
`validation.IsDNS1123Label` and reject anything else. The generated config
contains only these paths. Secret bytes are read only by the existing compiler
for validation and are already cleared; the file adapter never reads them.
Secret names and paths are not logged by the adapter.

#### 4.3.3 Rendering shape

```yaml
listeners:
  - name: web
    address: "0.0.0.0:8080"
    filter_chains: ["route-web"]
filter_chains:
  - name: route-web
    filters:
      - filter: router
        multi_level_subdomain_matching: true
        routes:
          - path_prefix: "/"
            cluster: service-8080-tcp-<hash>
      - filter: load_balancer
        clusters:
          - name: service-8080-tcp-<hash>
            endpoints:
              - "10.0.0.1:8080"
```

An HTTP listener with no representable routes emits a `static_response` filter
with status 404 and no `load_balancer` filter. Praxis rejects a router whose
`routes` list is empty, so this is the smallest valid fail-closed shape. The
load balancer filter is emitted only when at least one cluster is referenced.

#### 4.3.4 Determinism

- Listeners sorted by name; routes by `Priority`; clusters by name; endpoints by
  `address:port`; headers by name.
- No timestamps, UIDs, resourceVersions, map iteration, or random data.
- Render the same input 50 times and from permuted input order in tests; bytes
  must be identical.
- `sha256` of the exact bytes is the config digest stored in state.

### 4.4 File adapter mechanics (`internal/delivery/file`)

#### 4.4.1 Path layout and permissions

```
<root>/<namespace>/<gateway-name>/<gateway-uid>/
    owner.json        0600
    state.json        0600
    config.yaml       0600
    .*.tmp-*          temporary, same directory
```

Directories created `0700`. Components validated against DNS labels / safe UID
characters before path construction. Root comes from `--praxis-config-root`.
Including the UID matches existing target identity semantics and prevents a
recreated Gateway from inheriting or deleting the old target's files. The
target directory is removed only by `Provisioner.Release`.

`owner.json`:

```json
{"epoch":"...","retiredEpochs":["..."],"updatedAt":"RFC3339Nano"}
```

`state.json`:

```json
{
  "version": 1,
  "target": {"namespace":"...","name":"...","uid":"..."},
  "ownerEpoch": "...",
  "configDigest": "sha256:...",
  "resources": [
    {"key": {...}, "revision": "...", "source": {...}, "valueKind": "listener", "value": {...}}
  ]
}
```

The `value` is stored as `json.RawMessage` and decoded by `valueKind` into the
typed `config.Value`; `config.NewResource` re-derives and verifies the
revision on load. An unreadable or inconsistent `state.json` is treated as
absent, so a later full-snapshot apply repairs it; partial applies without a
listener are rejected, so this cannot clobber a good document. A corrupt
`owner.json` is an error: ownership is never granted from an unparseable file.

#### 4.4.2 Apply state machine

`Apply(ctx, changes)`:

1. Validate context, `ChangeSet`, stable ID, domain/kind allow-list.
2. Validate `changes.OwnerEpoch` equals the current owner file epoch (fencing).
3. Load state; apply upserts/deletes in memory.
   - Upsert keys must belong to the current epoch (`ChangeSet.Validate`).
   - Deletes may carry the current or any retired epoch.
   - Unknown/missing deletes are idempotent `ApplyDeleted`.
4. Render the complete prospective resource set through `praxis.Render`.
   - Render error: return a receipt with the changed upserts `ApplyRejected`
     and a safe reason, and any deletes `ApplyPending` (individual delete blame
     is not knowable; nothing was written and both states are retried).
     Do not modify `state.json` or `config.yaml`.
5. If the resulting resource map and config digest are unchanged and the file
   on disk matches, return `ApplyDelivered` for upserts and `ApplyDeleted` for
   deletes without writing (duplicate-apply idempotency).
6. Write `state.json` atomically (temp + fsync + rename + directory fsync),
   including the new config digest.
7. Write `config.yaml` atomically the same way.
8. Re-read `owner.json`; if the epoch changed during the write, return an error
   (the other writer will win; no ACK).
9. Return per-resource results: upserts `ApplyDelivered` with
   `DeliveredRevision=revision`; deletes `ApplyDeleted`. `Simulated=false`.

Crash semantics:

- Crash before step 6: nothing changed.
- Crash between 6 and 7: `Observe` sees the config digest mismatch, reports
  `ApplyPending` for every resource; `Diff` re-sends the same revisions; `Apply`
  re-renders and repairs. No stale success is claimed.
- Crash after 7 before the caller observes: the next `Observe` reports
  `Delivered`; `Diff` is empty. Delivery is acknowledged on restart.
- Duplicate/out-of-order Apply with older revisions: an older revision is still
  a legitimate desired revision if the state no longer contains a newer one;
  the in-memory state is authoritative for the target. Stale *epoch* writes are
  rejected. Stale *revision* replay from the same epoch is treated as an
  upsert and would regress content; the controller does not generate it because
  `Diff` is computed from the current observation. Documented residual risk.

`Observe(ctx, target)`:

1. Validate target. Missing target directory -> `Connected=false`, empty
   observation. Cleanup stays idempotent because `Claim` runs first and creates
   the directory.
2. Read owner/state/config. If state is missing or unusable ->
   `Connected=true`, no delivered resources; the next full apply rebuilds it.
3. If `sha256(config.yaml) == state.configDigest`, report every stored resource
   as `ApplyDelivered` with `DeliveredRevision=revision`; otherwise report every
   stored resource as `ApplyPending` with `DesiredRevision=revision`.
4. `Simulated=false`; `ProcessEpoch` empty (the adapter cannot observe the
   Praxis process). `Connected` means the delivery root is readable, not that
   the runtime is healthy.
5. Never report `ApplyActive` and never report a rejection as active.

`Claim(ctx, target, epoch)`:

- Validate target and epoch, create the target directory.
- Same epoch: refresh `updatedAt`, return.
- Different epoch:
  - If the current `updatedAt` is younger than `--praxis-owner-stale-after`
    (default `1m`), return a fencing error.
  - Otherwise append the old epoch to `retiredEpochs`, write the new epoch.
- This is advisory, filesystem-based fencing with a TOCTOU window. It is
  sufficient for a single leader with default leader election, but not a
  substitute for a kernel lock or a real protocol. Document and test.

`Delete` behavior is part of Apply: explicit keys only; never infer deletion
from the desired set. `Provisioner.Release` removes the target directory and is
idempotent on `IsNotExist`; it does not stop an external Praxis process.

#### 4.4.3 Capabilities

```go
Capabilities{
    ResourceKinds: []config.Kind{Listener, HTTPRoute, Cluster, EndpointSet, SecretHandle},
    IndependentEndpointSets: false, // whole-file reload
    AtomicRelatedResourceGroups: true, // one atomic file rename
    Simulated: false,
}
```

Not consulted by the controller in this slice. The renderer itself rejects
unsupported kinds, and `Programmed` cannot become true without `ApplyActive`.
Followup acceptance criteria: after `Ensure`/`Claim`, the controller calls
`Capabilities` once per target, refuses `Apply` (and keeps `Programmed=False`)
when a desired kind or required atomicity is not advertised, and has a test
with a capability list missing `EndpointSet`.

### 4.5 Provisioner (`internal/delivery/file/provisioner.go`)

```go
type Provisioner struct {
    Root      string
    Addresses []string
}
```

- `Ensure` validates the `TargetIntent` Gateway kind/namespace/name/UID, builds
  the same `config.Target`, creates the target directory, and returns:
  - `Target` exactly matching the request.
  - `Ready=true` meaning "static target directory is provisioned and may
    receive configuration", not "Praxis is serving".
  - `Addresses` copied from the configured flag (empty when unset).
  - `BootstrapRevision` = stable hash of the sorted listener intent (for
    change tracking only, not a runtime fact).
  - `Simulated=false`.
- `Release` removes the UID-scoped target directory. It never deletes any
  Kubernetes object and never signals the external process.
- Listener add/remove/address/protocol/TLS-mode changes require an external
  Praxis restart. `Ensure` does not and cannot verify this. No managed
  Deployment/Service/exposure is created in this slice.

### 4.6 Wiring (`cmd/controller/main.go`)

New flags (all ignored by the mock adapter unless noted):

| Flag | Default | Meaning |
| --- | --- | --- |
| `--delivery-adapter` | `mock` | `mock` or `file` |
| `--praxis-config-root` | `/var/run/praxis/config` | Required for `file`; target root directory. |
| `--praxis-listener-bind-address` | `0.0.0.0` | Address used for every rendered listener. |
| `--praxis-tls-root` | `/etc/praxis/tls` | Mount root for `tls.crt`/`tls.key` paths. |
| `--praxis-gateway-address` | empty, repeatable | Static Gateway status address(es); no runtime probing. |
| `--praxis-allow-private-endpoints` | `false` | When true, render `insecure_options.allow_private_endpoints: true`. Security decision; see risks. |
| `--praxis-owner-stale-after` | `1m` | Owner-epoch takeover threshold. |

Selection:

```go
switch adapter {
case "mock":
    configurator, provisioner = mock.NewConfigurator(), mock.NewProvisioner()
case "file":
    configurator, provisioner = file.NewConfigurator(file.Options{...}), file.NewProvisioner(...)
default:
    return error
}
```

Validate required file options before starting the manager. Startup log names
the selected adapter. No RBAC, CRD or manager-cache changes; no new API watch.

### 4.7 Exact file inventory

New:

- `internal/praxis/render.go`
- `internal/praxis/render_test.go`
- `internal/praxis/testdata/*.golden.yaml`
- `internal/delivery/file/configurator.go`
- `internal/delivery/file/state.go` (wire types, load/store, atomic write helpers)
- `internal/delivery/file/provisioner.go`
- `internal/delivery/file/configurator_test.go`
- `internal/delivery/file/provisioner_test.go`
- `internal/controller/file_adapter_test.go` (unit/envtest wiring proof)
- `docs/gateway-api/file-delivery.md`

Changed:

- `internal/delivery/configurator.go`
- `internal/delivery/configurator_test.go`
- `cmd/controller/main.go`
- `internal/controller/status_test.go`
- `go.mod` (direct `go.yaml.in/yaml/v3`)
- `README.md`
- `docs/gateway-api/architecture.md`
- `docs/gateway-api/implementation-plan.md`
- `docs/gateway-api/testing-and-security.md`
- `docs/research/praxis-configuration.md` (short revision-drift addendum)

No changes to `internal/delivery/mock`, `internal/gateway`, generated RBAC, the
Dockerfile, or Praxis source.

## 5. Test plan

### 5.1 Pure renderer (`internal/praxis`)

- Golden HTTP listener: single route, prefix and exact paths, host, headers,
  cluster and endpoints.
- Golden HTTPS listener: exactly two path strings; assert the Secret UID,
  resourceVersion, and key/cert bytes never appear in output.
- Determinism: repeated and permuted input -> byte-identical output.
- Empty routes -> valid minimal router, no load balancer.
- Wildcard host -> `multi_level_subdomain_matching: true`.
- Rejections, each with a stable, sanitized reason: multiple valid backends,
  weight split, redirect, header modifier, no-backend/backend-error, zero ready
  endpoints, same address:port twice, missing cluster/endpoint set/route table,
  non-Praxis domain, unsupported kind, invalid Secret namespace/name.
- `AllowPrivateEndpoints` false omits `insecure_options`; true emits exactly the
  one option.
- Fuzz `Render` with arbitrary resource slices: no panic, bounded output, either
  valid YAML or an error.

### 5.2 Adapter contract (`internal/delivery/file`, `t.TempDir`)

- Apply returns `ApplyDelivered` (not `ApplyActive`) with matching
  `DeliveredRevision`; no `ActiveRevision`.
- `Observe` after Apply reports the same delivered revisions; `delivery.Diff`
  with the same desired set returns an empty change set (no churn).
- Duplicate Apply: no file content change, no new temp files, no mtime change.
- Endpoint-only update: one config rewrite; unrelated resource revisions stay
  unchanged; the file remains valid.
- Explicit delete removes the resource; deleting a still-referenced cluster is
  rejected and leaves the previous file intact.
- Atomicity: simulate state-written/config-missing and truncated config; Observe
  reports `Pending`; next Apply repairs. A failed config write leaves no partial
  `config.yaml`.
- Owner fencing: same epoch idempotent; fresh different epoch rejected; stale
  epoch takes over and retires the old epoch; old-epoch Apply rejected;
  retired-epoch deletes accepted.
- Restart: a second Configurator over the same root observes identical
  revisions and computes an empty diff.
- Permissions: directories `0700`, files `0600`; symlinked/unexpected target
  components rejected.
- Secret hygiene: capture logs and assert no Secret data, UID/resourceVersion of
  handles, or full config bytes are logged.
- Provisioner: target identity mismatch rejected; `Ready` semantics; addresses
  passed through; `Release` is idempotent and UID-scoped.

### 5.3 Controller behavior

- Unit: a `ResourceResult{State: ApplyDelivered, DeliveredRevision: rev}` is
  committed in `Diff` but returns false from `allResourcesActive`; `setProgrammed`
  remains false.
- Unit: `Diff` re-sends `ApplyPending` and `ApplyRejected` revisions.
- envtest (or a focused unit with the real reconciler + fake client and the real
  file adapter): reconcile a Gateway/HTTPRoute/Service/EndpointSlice, assert
  `config.yaml` exists, `Programmed=False/Pending` with the current
  `observedGeneration`, and no `Apply` on a second reconcile after delivery.
  Assert `Gateway.status.addresses` is empty unless addresses were configured.
- Existing mock envtest suite and all compiler/config tests must stay green.

### 5.4 Gates

Every step runs:

```sh
go test ./...
go vet ./...
go tool controller-gen rbac:roleName=manager-role paths=./internal/controller output:rbac:dir=config/rbac
git diff --exit-code config/rbac   # no unintended RBAC drift
```

Plus `go test -race ./internal/delivery/... ./internal/praxis/...` for the new
concurrent adapter/state code. No upstream conformance claim is made.

## 6. Ordered implementation steps and validation gates

| Step | Work | Validation gate |
| --- | --- | --- |
| 1 | Add `ApplyDelivered`, `DeliveredRevision`, `Committed`, and the `Diff` change; update `configurator_test.go`. | All delivery tests pass; mock tests unchanged; a delivered revision is not re-sent. |
| 2 | Implement `internal/praxis` renderer and golden/fuzz tests. | Deterministic output; all rejection cases fail closed; no secret material in output. |
| 3 | Implement `internal/delivery/file` state store, atomic writes, Claim/fencing, Apply, Observe. | Adapter contract suite passes, including crash, duplicate, fencing, restart and permission cases. |
| 4 | Implement file `Provisioner`; add flags and adapter selection in `cmd/controller/main.go`. | `--delivery-adapter=file` starts; `mock` remains default; invalid/missing root fails at startup. |
| 5 | Add controller proof tests (`status_test.go`, `file_adapter_test.go`). | File adapter produces `Programmed=False`; no write churn across reconciles; mock status tests remain. |
| 6 | Update docs and the research drift addendum; regenerate RBAC. | `go test`, `go vet`, codegen diff clean; docs record Option A and Option B. |

One integrator owns `internal/delivery/configurator.go` and `internal/config`.
No shared-file parallel edits.

## 7. Impact matrix

| Component/API | Impact |
| --- | --- |
| GatewayClass/Gateway/HTTPRoute CRDs | No schema change. Status behavior unchanged except honest `Programmed=False` when the file adapter is selected. |
| `internal/delivery` contract | Additive enum/field/helpers; `Diff` semantics extended to delivered. |
| `internal/config` resources | No change. |
| `internal/gateway` compiler | No change. |
| `internal/delivery/mock` | No change; remains default. |
| `internal/controller` | No production logic change expected; tests only. |
| `cmd/controller` | New flags and adapter selection. |
| RBAC / CRDs / manifests | No new permissions or CRDs. Deployment needs a writable config volume and a TLS mount in file mode (operator-provided). |
| Praxis runtime | No source change. One static Praxis process per target must watch the generated file; listener/TLS changes require restart. |
| Observability | New sanitized adapter logs (key/revision/digest, never values); no new metrics in this slice. |

## 8. Risks and open decisions

1. **Private endpoint policy (must decide).** Normal Pod IPs require
   `insecure_options.allow_private_endpoints: true`; the default omits it, so
   generated configs will likely be rejected by Praxis for real backends.
   Recommendation: keep default off and require an explicit operator flag;
   before enabling, define scoped trust or network-policy enforcement.
2. **Static target identity/path (must decide).** UID-scoped path is safest for
   fencing but changes on Gateway recreation. Recommendation: accept for the
   static mode and document the operator procedure; managed mode is a followup.
3. **Fencing mechanism (should decide).** mtime-based owner lease has a TOCTOU
   window and clock/NFS assumptions. Recommendation: accept behind default
   leader election now; use `flock` or a real protocol before multi-writer
   operation.
4. **Representability gaps (must decide before widening).** Multi-backend
   weights, redirects, per-rule header modifiers and zero-endpoint clusters are
   rejected. Decide whether the next slice adds per-rule filter-chain branches,
   static error responses, or a runtime-native resource model.
5. **TLS rotation (accept limitation).** Paths are static; Secret content
   rotation requires a Praxis restart in this slice. Verify the `config-reload`
   single-certificate watcher before claiming otherwise.
6. **Capabilities gating (followup).** The adapter advertises
   `IndependentEndpointSets=false`; nothing consumes it. Acceptance criteria are
   in section 4.4.3.
7. **Whole-file reload (accepted for Option A).** Delivery is correct but
   runtime endpoint updates are not independent. Option B is the path to the
   architecture's independent-update requirement.
8. **Backend polling (accepted).** `BackendReconciler` requeues every 5s while
   not active, but performs no writes after delivery.
9. **Schema drift (must re-verify).** Fetch pinned `d1d8e1f3` and diff before
   release; the plan was verified only against local HEAD `1ea7a79a`.

## 9. Documentation updates

### 9.1 Research drift notes to record

Verified against local Praxis `1ea7a79a` (2026-10-09); pinned `d1d8e1f3` is not
in the local clone, so this is a spot-check, not a full diff:

- `insecure_options.allow_private_endpoints` governs HTTP/data-plane private
  literal addresses; `allow_private_upstreams` is the TCP startup-only option;
  cluster `trusted_private_endpoints` is a hostname allowlist. The renderer
  emits none of these by default.
- Listener `address` is required; there is no port-only form.
- Router supports `path`, `path_prefix`, `host`, `headers`, and
  `multi_level_subdomain_matching` (Gateway API needs it on).
- `load_balancer` supports endpoint weights (intra-cluster) but router routes
  select a single cluster; cross-cluster route weights are not representable.
- Empty clusters are rejected at configuration validation.
- Listener add/remove/rebind and any `tls` block change require restart; cert
  file contents may hot-reload only in the single-certificate `config-reload`
  configuration.
- Dynamic reload covers router routes and load-balancer endpoints/weights, but
  it rebuilds pipelines; this does not satisfy independent endpoint updates.

### 9.2 Files

- `docs/gateway-api/architecture.md`: record Option A, the `delivered` vs
  `active` distinction, path layout, static provisioner, and Option B followup.
- `docs/gateway-api/implementation-plan.md`: Phase 8 selected adapter = file
  (delivery only); update the decision table; Option B remains open.
- `docs/gateway-api/testing-and-security.md`: file adapter contract tests,
  atomicity/fencing/permission cases, and the unproven runtime/TLS-rotation
  gate.
- `docs/gateway-api/file-delivery.md` (new): flags, path layout, mount
  requirements, one-static-process assumption, restart requirements, private
  endpoint security decision, cleanup.
- `README.md`: `--delivery-adapter=file`, `Programmed=False` until runtime
  evidence, and TLS restart limitation.
- `docs/research/praxis-configuration.md`: add the revision-drift addendum.

## 10. Execution notes

Decisions confirmed by the maintainer: private endpoints opt-in (default off),
UID-scoped target paths, mtime lease fencing.

Deviations from the draft, all smaller than the plan:

- `Claim` refreshes `owner.json` at most once per half of `OwnerStaleAfter`
  rather than on every reconcile, so periodic reconciliation does not write
  every 5s.
- Deleting every desired resource removes `state.json` and `config.yaml`
  instead of rendering an empty document that Praxis might reject.
- When no state exists, an apply without a listener is rejected, so a partial
  EndpointSlice-only event cannot overwrite the document before the Gateway is
  compiled. An unreadable or inconsistent `state.json` is treated as absent so
  the next full-snapshot apply repairs it; a corrupt `owner.json` remains a
  fail-closed error.
- Security/QA follow-ups: the owner epoch is re-checked between `state.json`
  and `config.yaml` publication; retired epochs are deduplicated and capped;
  target paths are validated for safe components and existing symlinks;
  `Observe.Connected` reflects whether the target directory exists; and the
  private-endpoint documentation states that sensitive addresses are rejected
  at render time by default, not silently omitted.
- QA follow-up: Praxis rejects a router with no routes. A listener with an
  empty route table now renders a `static_response` 404 filter, verified with
  `praxis -c <file> -t`; a renderer golden file is validated the same way.
- Tests added for empty routes, sensitive endpoints, golden output, fuzzing,
  missing references, wildcard hosts, restart reconstruction, symlinked
  targets, inconsistent-state repair, and a real `Reconciler.applyDesired`
  call with the file adapter proving delivered is not active.
- Atomic multi-writer fencing remains an accepted limitation of the mtime lease
  behind leader election.
- Renderer rejections are reported as `ApplyRejected` on the changed upserts;
  no file or state is modified.
- The renderer rejects redirects and per-rule header modifiers. Option A
  therefore delivers forward-action routes only until a later slice; this is
  recorded in the limitations below.

Validation performed: `go test ./...` (73 tests, 8 packages), `go vet ./...`,
`go test -race ./internal/delivery/... ./internal/praxis/...`, and
controller-gen RBAC output unchanged (SHA-256 identical).

Residual limitations kept from section 8: whole-file reload, restart-required
listener/TLS changes, unproven TLS rotation, capabilities not consumed, 5s
non-active polling, and Praxis schema drift since pinned `d1d8e1f3`.
