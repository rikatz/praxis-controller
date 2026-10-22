# Testing, security verification and release gates

This document combines current evidence with remaining release gates. The mock-backed HTTP controller has repository-local unit, contract and envtest coverage, but serves no traffic. The [upstream research](../research/gateway-api-conformance.md) supplies the exact tagged conformance tests and runner flags; [architecture](architecture.md) defines the contracts tested here.

## 1. Test layers and evidence

| Layer | What it proves | Required cases | What it cannot prove |
| --- | --- | --- | --- |
| Pure Go tests/fuzzing | Gateway interpretation, canonical resources and invalidation decisions | Attachment, normalization, precedence, grants, backend resolution, deterministic hashes, invalid/empty backend distinctions | API-server schemas, watches, actual forwarding |
| Stateful mock and adapter contract tests | Resource independence, operation logs and application lifecycle | Per-target/root state, listener/route/cluster/endpoint add/update/delete, ordering, simulated active receipts, ACK/NACK, idempotency, reconnect, stale epoch, partial rejection | Real Praxis semantics or traffic forwarding; simulated-active status is not production or conformance evidence |
| envtest with production manager options | API-server behavior and reconciliation | Upstream CRDs/status, conflict retries, generations, dependencies, schema/default handling, finalizers | Deployment/EndpointSlice controllers, garbage collection, networking or conformance traffic |
| Actual Praxis process tests | Lowering and runtime semantics | HTTP, HTTP/2/gRPC, TLS passthrough, TCP, UDP, endpoint replacement, filter state preservation, certificate reload | Kubernetes provisioning/RBAC unless paired with a cluster |
| kind/equivalent cluster end-to-end | Real workloads, routing, RBAC and lifecycle | Pod replacement, Service exposure, listener rollout, Secret updates, leader failover, cleanup and tenant isolation | Production capacity or every cloud integration |
| Unmodified upstream conformance | Required behavior of the pinned Gateway API profiles | All applicable Core tests for the five profiles; raw logs and official reports | AI/policy independence, all security threats, bounded resource usage or production reliability |

Write meaningful expected behavior at the public module interface. Avoid tests that only mirror structs or mock the exact function calls of the implementation. Golden configuration fixtures are useful for schema stability, but every protocol also needs traffic assertions against actual Praxis.

### File delivery adapter evidence

The opt-in file adapter has repository-local coverage for: deterministic
lowering and rejection of unrepresentable input; delivered-but-not-active
revision semantics and no write churn after delivery; duplicate apply; explicit
deletion; state-written/config-missing crash repair; restart reconstruction;
owner-epoch fencing and takeover; file/directory permissions; and Secret
hygiene (paths only, no UID/resourceVersion or bytes in the document). A
controller unit test proves a delivered revision does not satisfy
`allResourcesActive` or `Programmed`. A corrupt `owner.json` fails closed, while unreadable or inconsistent
`state.json` is rebuilt by the next full-snapshot apply without allowing a
partial apply to clobber the document. Unsafe target path components, symlinked
components and a missing target directory are also tested. The renderer golden
files were validated manually with the real `praxis -c <file> -t` binary; no
repository test invokes it, so this is a recorded manual gate rather than
automated evidence.

This is delivery evidence only. It does not prove that Praxis parsed or
accepted the document, that a reload occurred, that traffic is served, that TLS
rotates, or that endpoint updates are independent of pipeline rebuilds. Those
remain gates for a real target and for Option B.

### First HTTP Core milestone gate

The first controller milestone targets `GATEWAY-HTTP` Core through the mock. Require pure compiler tests and envtest reconciliation/status tests using the pinned upstream CRDs, plus the repository's build, test, code-generation-diff, vet/static-analysis and lint gates. The mock must keep per-Gateway target state and independent roots, emit sanitized structured operations, make duplicate applies idempotent, and expose controllable simulated-active, NACK and reconnect outcomes.

Assert that a Gateway/HTTPRoute change emits only its affected typed resource changes; an EndpointSlice change emits only the affected EndpointSet operations and leaves clusters, routes, listeners, AI and policy revisions untouched. A mock Programmed=True assertion is valid only after an explicit simulated-active receipt and must be labeled as mock evidence. Do not run or report the upstream profile as passed in mock mode: there is no serving dataplane.

## 2. Pure semantic test matrix

- Normalize ParentRefs, backend refs, namespaces, default group/kind and omitted fields; distinguish unset and zero values when the upstream schema does.
- Check controller ownership, missing/deleted GatewayClass/Gateway, allowedRoutes kinds/namespaces, Namespace selector transitions, protocol compatibility, hostname intersections and listener conflicts.
- For every route kind test multiple parents and controllers, stale parents, allowed/denied cross-namespace backends, missing objects, Service port changes and object deletion/recreation. Cross-namespace parent attachment and backend/certificate authorization have different rules.
- Verify HTTP path exact/prefix boundaries, host wildcards and precedence. Test every Core match/filter combination from the pinned inventory, stable tie-breaks, no match and invalid actions. Permute input enumeration order to prove deterministic output.
- Verify GRPC service/method matching and response/error semantics, TLS SNI overlap/selection, and TCP/UDP binding/weighted backend semantics. Use upstream type comments and tests together when support annotations disagree.
- Test valid backend with no ready endpoints separately from a missing/unauthorized backend. Preserve invalid weighted shares as prescribed; never renormalize them away simply because resolution failed.
- Backend resolver fixtures cover multiple EndpointSlices, duplicate entries, IPv4/IPv6, named Service ports/resolved target ports, readiness default values, terminating endpoints, invalid/missing ports, address family/protocol mismatch, empty aggregates and old/new Service association. Include the Core headless and selectorless/manual-slice cases; destination validation must allow their legitimate Pod addresses.
- Assert semantic no-ops produce no config delta, status write or deployment change. Invalidating a denied/missing reference must still let later referent/grant creation repair the right route.
- Fuzz normalization, host/path matching, weighted arithmetic, resource identity and canonicalization for crashes, overflows, unstable ordering and unbounded growth. Seed with boundary cases from the pinned CRD schemas.

## 3. envtest setup and integration cases

Follow the mandatory controller skill: Ginkgo/Gomega; `envtest.Environment` with `DownloadBinaryAssets: true`, a cached `BinaryAssetsDirectory`, a pinned full Kubernetes version and `ErrorIfCRDPathMissing: true`. Install the exact upstream standard CRDs. Use the same cache/client/index/manager options as production except test ports and intentional test overrides. Pin tool artifacts; do not use moving latest binaries or a separate setup-envtest download script.

Use a unique namespace per spec and eventual assertions for cached observations. Start the manager, wait for caches, run the behavior, cancel and wait for manager shutdown before stopping the API server. Fail on any recovered reconcile/webhook panic counter. Fake client use is limited to narrow helpers with the real indexes and `WithStatusSubresource` where needed; status merge/SSA behavior must be tested against the real API server.

Required integration scenarios:

| Scenario | Assertion |
| --- | --- |
| Concurrent status writers/parent controllers | Preserve unrelated entries/conditions; conflicts retry safely |
| Generation changes during pending delivery | Old ACK cannot mark a new generation active |
| Secret data changes without generation increase | Authorized consumer re-evaluates; credentials never appear in diagnostics |
| Namespace labels change | Old attachments are removed and new eligible ones appear |
| ReferenceGrant creation/revocation | Only affected consumers change; revoked bindings withdraw |
| EndpointSlice relabel/deletion/tombstone | Consumers of both old and new Service keys recompute correctly |
| User objects recreated with the same name | UID epochs prevent stale ownership/cleanup/application |
| Initial cache sync or list fails | No destructive garbage collection from partial desired inventory |
| Unchanged reconcile/status-only update | Stable conditions/timestamps and zero semantic config writes |
| One invalid Route or target rejection | Unrelated Routes/targets continue to converge |
| Filtered/pruned cache | Required fields remain; no hidden valid references; no full-object Update from pruned data |

envtest does not run Deployment, namespace, EndpointSlice or garbage-collection controllers. Assert ownership fields/finalizer behavior there; use kind for actual Pod/EndpointSlice creation, rollouts and cleanup. A default envtest admin client does not establish RBAC correctness. An impersonated client can check verbs, and deployed real ServiceAccounts must also be exercised.

## 4. Independent-update acceptance suite

This suite is mandatory for every production connector and for runtime changes. Instrument compilation/resource application counts and active runtime identities in tests; do not infer independence from equal final YAML alone.

Start two Gateways with overlapping backend Services, multiple listener/protocol types, an AI-owned root and a policy-owned root. Initially the latter can be opaque contract fixtures; before first AI/policy integration, repeat with real documents and their actual runtime state. Record independent desired/active revisions, resource payloads, Deployment template, process epoch and filter/state identities. Use harmless persistent counters/state handles to detect reconstruction.

| Change | Required effects | Forbidden effects |
| --- | --- | --- |
| Recreate one backend Pod | Kubernetes updates slices; affected EndpointSets converge; new traffic uses ready replacement | Whole-Gateway compile, unchanged Cluster serialization/revision, listener/route/AI/policy regeneration, deployment rollout, unrelated state reset |
| Update slice with same canonical membership | Observation processed, no config write | False revision bump caused by resourceVersion/order |
| Remove final ready endpoint | Affected membership becomes empty; stale backend stops receiving new traffic | Retaining old address, selecting a different unauthorized backend |
| Change one Route rule | Affected merged route table and explicit new dependencies update | Rebuilding unrelated endpoint sets or other domains |
| Rotate TLS Secret | Coherent secret/binding revision activates, correct cert served | Partial key/cert exposure, unrelated route/endpoint/AI/policy regeneration |
| Change AI root only | AI root and declared dependent binding update | Policy or generic config regeneration |
| Change policy root only | Policy root and declared dependent binding update | AI or generic config regeneration |
| Modify listener/bootstrap | Managed lifecycle change, new active listener exposure and accurate status | Endpoint membership/root revision churn caused only by deployment hashing |

Use positive controls: show that an event was consumed and the affected target changes, while asserting unrelated work stays at zero. Test burst coalescing, concurrent unrelated changes, adapter disconnect/reconnect and controller restart. Record endpoint count, update rate, convergence latency, changed resources and runtime identity evidence. Establish workload-specific numerical latency/memory budgets during implementation; independence assertions are exact, not percentile targets.

## 5. Delivery and lifecycle fault injection

- Duplicate/out-of-order submissions and receipts; apply racing with delete; old controller leader or target process ACK; target recreated under the same logical name.
- NACK one resource in a related change; lost receipt after successful activation; accepted write followed by target crash; reconnect with unknown active revisions; no application ACK.
- Crash while staging or activating files, disk full, unreadable path, invalid permissions, malicious symlink, partial certificate pair, ConfigMap projection delay and watched-file replacement. For network adapters: unauthenticated/expired identity, oversized messages, replay and disconnected consumer.
- Initial desired inventory incomplete, API server unavailable, dependency deleted during compilation, authorization revoked during pending apply, finalizer cleanup retry and controller cancellation.
- During listener replacement continuously probe added/removed/unchanged sockets and verify new status only after the required serving instances are correct. Keep old unsafe listeners from remaining exposed. Test gateway Service UDP ports explicitly.
- Delete a Gateway and confirm owned target cleanup on a real cluster without deleting user-owned Services, Secrets, Routes or unrelated targets. Recreate with the same name and prove old cleanup cannot remove the new target.

Pass criteria: eventual convergence after recoverable faults, bounded retries/queues, no false Programmed success, no stale writer changing the active graph, no dangling active dependency and preservation of unrelated valid resources. Define and test a concrete maximum revocation window before production; reporting failure alone does not withdraw traffic.

## 6. Threat model and security verification

Trust subjects are cluster operators, GatewayClass owners, Gateway owners, Route authors, namespace/Secret owners, EndpointSlice writers, connector identities and external clients/backends. A valid Kubernetes object does not automatically authorize references, external destinations or arbitrary Praxis configuration. For each row record the asset, attack fixture, expected denial/withdrawal and passing evidence.

| Threat | Required control | Verification |
| --- | --- | --- |
| Route author crosses tenant boundary | allowedRoutes plus ReferenceGrant checks at their proper reference sites | Deny absent/wrong group-kind/name/namespace grant; allow valid grant; revoke live |
| Controller handles another implementation's objects | Stable controllerName and per-parent status ownership | Two controller names; no spec/status/config writes for unowned objects |
| Secret disclosure via output | Private secret resources, sanitized diagnostics, no raw objects | Canary secret absent from logs/events/status/metrics/ConfigMaps/reports and wrong-target payloads |
| Backend SSRF or credential forwarding | Provenance-aware address validation, scoped Kubernetes trust, protocol controls | Deny loopback/link-local/metadata/reserved destinations, misleading slice association and external redirection; allow legitimate supported Pod CIDRs |
| Stale configuration retains revoked access | Dependency invalidation and bounded fail-closed withdrawal | Revoke grants/delete Secrets during connector failure and verify traffic actually stops |
| Template/config injection | Typed lowering, validated names/paths, operator-owned bootstrap settings | Malicious annotations/hostnames/header values cannot add filters, file references, environment interpolation or insecure flags |
| Connector impersonation/replay | Target-bound authenticated identity and fenced epochs | Wrong-target caller, old writer, old ACK and reconnect cannot overwrite active desired state |
| Resource exhaustion | Bounded compilation, queues, input sizes, network peeking/flow state | Many routes/endpoints, churn, slow TLS ClientHello, UDP flow flood and malformed traffic remain within selected limits |
| Excess Kubernetes authority | Explicit resources/verbs, split duties and no wildcard permissions | Real ServiceAccount cannot read unrelated Secrets or mutate user specs; role diff and denied-operation tests |
| Exposed debug/admin surfaces | TLS/authn/authz metrics, isolated admin/probes, controlled pprof | Metrics 401 without token, 403 without privilege, 200 for authorized scraper; admin unreachable from tenant traffic |
| Unsafe Pod/process privileges | Restricted security context, minimum writable paths, constrained egress | Admission under selected Pod Security level and network-policy reachability tests |
| Supply-chain or dependency flaw | Pinned modules/images/artifacts, vulnerability and provenance review | `govulncheck`, dependency/image scans, SBOM and digest verification; review reachability and fix blockers |

Kubernetes `get/list/watch` on Secrets are all confidentiality-sensitive; namespace scoping reduces authority, and `list`/`watch` expose all Secret data in scope. Decide the viable Secret observation/read model explicitly rather than promising RBAC can dynamically restrict arbitrary referenced names. Keep controller and data-plane ServiceAccounts separate; the proxy should not receive Kubernetes API credentials by default. [Kubernetes RBAC guidance](https://kubernetes.io/docs/concepts/security/rbac-good-practices/).

The controller Deployment should run non-root, disallow privilege escalation, drop capabilities and use RuntimeDefault seccomp; use read-only root filesystem and a dedicated writable staging volume if the connector needs it. Bind data-plane public ports through Services/high container ports where practical, avoiding broad privileges. Protect control-plane and data-plane admin endpoints separately from public listener traffic. [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/).

Do not set `allow_private_upstreams` or broad insecure options globally merely to route Kubernetes addresses. The source research identifies different HTTP literal-IP and TCP checks; validate the complete controller-to-runtime path for each protocol. A secure scoped trust path is a runtime prerequisite for TCP/TLS, and manually managed discovery data needs an explicit trust policy.

Core conformance does not prove this threat model. Require independent review of authorization, credential publication, destination validation, activation/revocation and cleanup before release. Review eventual AI/policy extensions again when their trust inputs are introduced; current placeholders do not establish their security.

The implemented HTTP slice tests allowedRoutes selector/kind decisions, positive and negative ReferenceGrant cases, unauthorized backend rejection, HTTPS Secret authorization and keypair validation, EndpointSlice readiness/serving/terminating filtering, and redaction from compiler resources, Gateway status and mock journal/log output. Deployment security remains open: generated RBAC grants cluster-wide Secret `get/list/watch`, and real ServiceAccount authorization, authenticated metrics integration, scoped endpoint trust, and actual certificate loading/rotation have not been tested. Verify certificate behavior against Praxis in the HTTP data-plane phase.

## 7. Manager, metrics and operational checks

- Secure metrics with controller-runtime `SecureServing: true` and `filters.WithAuthenticationAndAuthorization`, real serving certificates and TokenReview/SubjectAccessReview RBAC. Bind scraper permissions separately. Register custom collectors on controller-runtime's metrics registry and use bounded labels.
- Verify controller leader Lease permissions and actual active worker behavior. Readiness means required caches/startup are ready; a process health check alone does not prove leadership or target programming.
- Log through `ctrl.LoggerFrom(ctx)` with a configured backend. Emit events for actionable state changes, not every reconcile. Match events.k8s.io recorder permissions and legacy leader-election events to their actual groups.
- Observe queue depth, reconcile errors/panics, dependency fanout, compile duration, changed-resource counts, apply/NACK/ACK latency, desired-versus-active lag and endpoint convergence. Avoid endpoint/object UID/secret content labels or raw error messages.
- Test shutdown cancellation and bounded target cleanup; termination grace exceeds the configured shutdown budget. Verify disconnected targets and stuck finalizers are diagnosable without disclosing credentials.

## 8. Upstream Core execution and evidence

Use the exact external-runner recipe in the [Gateway API research](../research/gateway-api-conformance.md). The intended baseline selects all five profiles and the explicit features `Gateway,ReferenceGrant,HTTPRoute,GRPCRoute,TLSRoute,TCPRoute,UDPRoute`. Do not use `all-features`; do not let automatically discovered GatewayClass features add Extended tests. Keep provisional/optional exclusions explicit and review them against the pinned inventory.

Only a run against the actual controller and actual request-serving Praxis dataplane can pass these profiles. Mock compiler/status tests are useful controller evidence, but never count as upstream conformance results.

Before a full run, deploy the release candidate images, provision the selected GatewayClass with the stable controllerName, install matching upstream standard CRDs, and provide a reproducible client-to-Gateway exposure path. Test TCP, HTTP/2, TLS and UDP reachability from the runner environment. Use upstream fixture images and readiness/timeouts as pinned; a cluster cannot pass if its networking cannot expose the required protocols.

Required run artifacts:

1. Controller/Praxis/AI/policy revisions and image digests, build features, Kubernetes and Go versions, connector mode and relevant capability settings.
2. Gateway API tag/commit, standard bundle identity, expected eligible test inventory, profiles and explicit supported feature set.
3. Exact invocation, full raw test output, report YAML, skipped/excluded test list and reason, and test counts. Compare inventory to actual execution; a process exit code alone is insufficient.
4. Sanitized GatewayClass/Gateway/listener/Route statuses, controller/target logs, events, active revisions and cluster/network diagnostics on failure. Retain no private keys or credential-bearing payloads.
5. Independent-update/security evidence, documented scale workload and timings, unresolved issue list and release decision.

The tagged baseline inventory contains 67 combined eligible tests; that is a source-derived count, not an executed result, and runtime filtering must be checked against the actual run. All applicable required tests must execute and pass. If an upstream bug prevents a test, record it and the upstream issue; do not claim the affected profile complete by silently excluding it.

## 9. Suggested CI progression and release checklist

| Trigger | Required future checks |
| --- | --- |
| Every change | Formatting, vet/static analysis, relevant pure/contract/envtest tests; race detection for concurrent controller/state code |
| API/RBAC/tooling change | Regenerate and review owned generated output; kube-api-linter for owned APIs only; actual permission tests |
| Translator/runtime/adapter change | Actual Praxis protocol tests and relevant Core subset; independent-update regressions |
| Main/nightly | Full combined five-profile Core suite, lifecycle/fault tests, security negatives and race suite |
| Release | Full pinned Kubernetes support matrix, all Core tests, threat-model gate, independent root/runtime preservation, vulnerability review and evidence bundle |

Run tests appropriate to the work and broaden after meaningful new changes/failures. Keep envtest separate from conformance because they prove different behavior. A future API upgrade requires re-inventorying Core/Extended/provisional classifications, CRD schemas/status lists, runner/module flags and runtime capabilities before updating the release claim.

Release blockers include any required failing/skipped Core test, whole-config regeneration on endpoint churn, unrelated AI/policy state reset, unsupported listener modification, false applied-state status, stale authorization after revocation, secret leakage, unsafe destination bypass, unbounded UDP/TLS state, or unverified adapter behavior hidden behind a fake.
