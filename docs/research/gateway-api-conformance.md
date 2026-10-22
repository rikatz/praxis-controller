# Gateway API conformance research for the Praxis controller

Research date: 2026-10-19. Planning and source inspection only; no controller implementation or conformance execution.

## 1. Pinned upstream baseline

Use **Gateway API v1.6.3**, commit `c0836133e24073fcdcdbb1713aba3cc73a89d0b5`, as the initial research and implementation target. GitHub identifies it as the latest stable release, published 2026-10-06. Main was at `5a79c1a6307154a2fc76e3d6da3ea43956d346e5` during research; it is not the conformance contract. Every source link below pins v1.6.3 unless it is a release page. Recheck the release before implementation and record any deliberate baseline change. [Release](https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.3), [tagged changelog](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/CHANGELOG/1.6-CHANGELOG.md).

| Resource | Initial Go/API version | Release channel | Relevant conformance profile |
|---|---|---|---|
| GatewayClass, Gateway | `gateway.networking.k8s.io/v1` | Standard | Shared by all five Gateway profiles |
| HTTPRoute | `gateway.networking.k8s.io/v1` | Standard | `GATEWAY-HTTP` |
| GRPCRoute | `gateway.networking.k8s.io/v1` | Standard | `GATEWAY-GRPC` |
| TLSRoute | `gateway.networking.k8s.io/v1` | Standard | `GATEWAY-TLS` |
| TCPRoute | `gateway.networking.k8s.io/v1` | Standard | `GATEWAY-TCP` |
| UDPRoute | `gateway.networking.k8s.io/v1` | Standard | `GATEWAY-UDP` |
| ReferenceGrant | `gateway.networking.k8s.io/v1` | Standard | Core dependency of every Gateway profile |

The tagged Standard CRDs are authoritative for served/storage versions. TCPRoute and UDPRoute graduated to GA in v1.6; their old v1alpha2 versions are present in the v1.6.3 CRD definitions with `served: false`. TLSRoute v1alpha2 and v1alpha3 are also unserved; HTTPRoute v1beta1 remains served. ReferenceGrant v1 is served, while its storage version remains v1beta1 in this bundle; importing the served v1 type does not require changing upstream storage settings. Do not use pre-v1.6 examples to conclude TCP/UDP have no named profiles. Conversely, do not generalize the five-profile claim to an earlier release. [Standard CRDs](https://github.com/kubernetes-sigs/gateway-api/tree/v1.6.3/config/crd/standard), [profiles](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/utils/suite/profiles.go), [v1.6 graduation notes](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/CHANGELOG/1.6-CHANGELOG.md).

**Three separate axes:** release channel (Standard/Experimental), API version (v1/v1alpha2/etc.), and feature support level (Core/Extended/Implementation-specific). Installing Standard CRDs does not restrict objects to Core fields. Supporting a GA resource does not mean every feature of it is Core. Scope is the five named profiles' Core requirements and required rejection behavior, not every Standard feature. [Conformance definitions](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/site/content/en/docs/concepts/conformance.md).

All five profiles include `Gateway`, `ReferenceGrant`, and their respective route feature in Core. `BackendTLSPolicy` remains Extended relative to the Gateway HTTP/GRPC/TLS profiles even though it has its own Core feature set. Mesh profiles, ListenerSet, backend TLS policy, client certificate validation, retries, timeouts, query/method matching, regex matching, rewrites, mirroring, CORS, and implementation policy APIs are outside the initial feature claim. Required error handling when those inputs are unsupported remains in scope. [Profile definitions](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/utils/suite/profiles.go), [HTTP feature metadata](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/pkg/features/httproute.go), [Gateway feature metadata](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/pkg/features/gateway.go).

## 2. What Core behavior requires

### Shared ownership, attachment, and references

- Watch GatewayClasses and claim only the configured domain-qualified `controllerName`. Set `Accepted` on the matching classes, and `SupportedVersion` based on supported installed CRD bundle/channel. Reconcile a Gateway only when its referenced class exists and is accepted. Read CRD bundle/channel annotations or adopt a documented equivalent compatibility check. Treat missing or unsupported CRDs as an explicit startup/readiness failure, rather than an informer hang. [Implementer's guide](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/site/content/en/guides/implementers-guide/_index.md).
- Resolve ParentRef defaults, group, kind, namespace, optional listener section, supported route kinds, namespace attachment permissions, and hostname intersection. Without sectionName, attachment can succeed on more than one compatible listener; one successful listener suffices for parent acceptance. When port and sectionName are both provided, both must match. Preserve separate status for each parent/controller. [Shared route types](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/shared_types.go), [Gateway/listener types](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/gateway_types.go).
- Route-to-Gateway authorization comes from the listener's `allowedRoutes`; Route-to-Service and Gateway-to-Secret cross-namespace authorization comes from ReferenceGrants in the target namespace. Do not substitute one handshake for the other. Match grant source group/kind/namespace and target group/kind/optional name exactly; grants are additive. Removing a grant or changing namespace labels must invalidate existing attachments/references promptly. [Security model](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/site/content/en/docs/concepts/security.md), [ReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/referencegrant_types.go).
- Resolve numeric backend Service ports to the correct protocol and EndpointSlice ports/addresses; handle nonexistent Services, missing ports, unsupported kinds, empty endpoints, and weight zero deliberately. Core backend kinds do not authorize arbitrary object kinds or arbitrary egress addresses. Service lifecycle and EndpointSlice changes must update only dependent backend state. The choice to use ClusterIPs or explicit endpoints is an implementation decision; the final behavior must pass the Service-type tests. [Backend reference types](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/shared_types.go), [Service-type test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-service-types.go).

### HTTPRoute

Core includes exact and path-element prefix matching, exact header matching (case-insensitive header names), hostname intersection/wildcards, OR between matches and AND within a match, default `/` prefix, forwarding to weighted Service backends, rule-level request header modification, and Core redirects (hostname and 301/302 status). Select applicable Routes by hostname specificity first (matching non-wildcard hostname length, then matching hostname length); ties use exact path, longest prefix, supported additional match specificity, then oldest creation timestamp and lexical namespace/name. Within a tied Route, first matching rule wins. A path prefix `/foo` matches `/foo/bar`, not `/foobar`. Return 404 when no attached rule matches. Invalid backend references retain their traffic weight and cause 500 responses, rather than silently redistributing traffic; a valid backend with no ready endpoints should produce 503 under the spec's allowed behavior. Omitted backendRefs without a response-producing filter is an error response. Preserve valid parts of partially invalid configuration. [HTTPRoute specification comments](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/httproute_types.go), [matching tests](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-matching.go), [redirect test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-redirect-host-and-status.go).

HTTPS listener termination using Kubernetes TLS Secrets is needed for HTTP Core. Validate Secret type/data, certificate/key parsing and match, and authorization before programming; missing or invalid TLS config must not silently fall back to plaintext. Handle Secret rotation and revocation. TLS termination for an HTTPS listener carrying HTTPRoute is distinct from Extended TLSRoute termination. [TLS configuration](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/gateway_types.go), [HTTPS Core test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-https-listener.go), [invalid TLS Core test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-invalid-tls-configuration.go).

### GRPCRoute

Core requires gRPC over HTTP/2: HTTP prior knowledge, HTTPS ALPN `h2`, exact fully specified service/method matching, exact header matching, hostnames, weighted Services, and rule-level request header modification. The current four Core-gated GRPC tests do not cover every normative Core behavior. Preserve protobuf/gRPC messages, trailers, streaming, and gRPC error semantics; invalid backend weight must yield the corresponding proportion of `UNAVAILABLE`, never disappear from the distribution. HTTPRoute and GRPCRoute sharing intersecting hostnames on a listener have deterministic conflict semantics; do not merge their rules indiscriminately. [GRPCRoute types and normative comments](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/grpcroute_types.go).

### TLSRoute

Core is TLS passthrough: inspect SNI without terminating encryption, apply listener/route hostname intersection, forward unchanged TLS streams to Services, reject invalid references, and report unsupported termination capability accurately. `TLSRouteModeTerminate` and `TLSRouteModeMixed` are Extended features. A TLSRoute attaches to a TLS listener, not an HTTPS listener. Read the TLSRoute precedence and hostname rules together with listener selection; routing by untrusted SNI must not bypass attachment authorization. [TLSRoute types](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/tlsroute_types.go), [TLS feature metadata](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/pkg/features/tlsroute.go), [unsupported termination test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-listener-terminate-not-supported.go).

### TCPRoute and UDPRoute

Core profiles require raw stream/datagram routing, weighted backends including zero weight, parent attachment, multiple-route conflict resolution, namespace permissions, and ReferenceGrant behavior. These are concrete v1.6.3 tests, not future work. With invalid TCP backends, actively reject the corresponding weighted fraction of connections; with invalid UDP backends, drop the corresponding fraction of packets. Do not route rejected references to a fallback backend. Both kinds currently have a single rule in the CRD schema. Test connection/session behavior, long-lived traffic, IPv4/IPv6 where supported, payload limits, and cleanup independently of upstream Core tests. [TCPRoute types](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/tcproute_types.go), [UDPRoute types](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/udproute_types.go), [TCP weighted test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-weighted-routing.go), [UDP weighted test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-weighted-routing.go).

### Upstream inconsistencies to track explicitly

The tagged UDPRoute `BackendRefs` field comment still labels Service support Extended, while the named `GATEWAY-UDP` Core profile requires UDPRoute and its backend-routing tests. Shared ParentReference.Port is labelled Extended, but TCP/UDP `ParentRefPortAndSectionName` tests require only their Core route feature plus Gateway. Implement those behaviors for the chosen Core test target; record clarification issues upstream instead of silently exempting tests. Pinned source/test metadata takes precedence over a stale general website summary. [UDP type](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/udproute_types.go), [shared field](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/shared_types.go), [TCP parent test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-parentref-port-and-section-name.go), [UDP parent test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-parentref-port-and-section-name.go).

## 3. Status is part of the contract

Use upstream types/constants; do not modify their CRDs or add invented conditions to make generic skill examples fit. Set condition `observedGeneration` to the resource generation actually evaluated. GatewayClass uses `Accepted` and `SupportedVersion`; Gateway and listener status include the prescribed acceptance/programming/reference conditions, listener `supportedKinds` and `attachedRoutes`, and Gateway addresses. Route status is per parent and `controllerName`; write only entries owned by this controller, preserving other implementations. Use conflict-safe status writes and avoid writes when content is unchanged. `Accepted=True` can coexist with `ResolvedRefs=False` for partially usable configurations. `Programmed=True` requires completed programming, not merely enqueueing desired state; it does not promise traffic was already ready at that exact instant. Acknowledgements must identify the desired generation/revision so a stale completion cannot mark a newer intent programmed. [Implementer's guide](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/site/content/en/guides/implementers-guide/_index.md), [shared route status](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/shared_types.go), [Gateway status](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/gateway_types.go), [GatewayClass status](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/gatewayclass_types.go).

Use reasons required by the relevant field semantics: examples include `InvalidKind`, `BackendNotFound`, `RefNotPermitted`, `NotAllowedByListeners`, `NoMatchingListenerHostname`, and `UnsupportedProtocol`. Test exact expected conditions/reasons from the pinned manifest and test source. Missing references are user configuration errors with deterministic status; infrastructure failures are retryable operational failures. A vanished watched object must trigger dependent reconciliation even when its spec generation never changed. [Core test sources](https://github.com/kubernetes-sigs/gateway-api/tree/v1.6.3/conformance/tests), [shared condition constants](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/shared_types.go).

## 4. Conformance execution and report recipe

This is a **future execution recipe**, not an assertion that Praxis passed. Use a disposable Kubernetes cluster with the exact CRD bundle, an installed controller and reachable Praxis dataplanes, a pre-created accepted Praxis GatewayClass, and network connectivity from the test runner to Gateway status addresses. The upstream suite creates its infrastructure namespaces, workloads, TLS Secrets, Routes, Gateways, and echo servers. Ensure CNI policies and TCP/UDP reachability allow those flows. Pin controller/proxy image digests and test-image versions alongside the suite revision. [Conformance guide](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/site/content/en/docs/concepts/conformance.md), [suite base resources](https://github.com/kubernetes-sigs/gateway-api/tree/v1.6.3/conformance/base).

```bash
# Work in a separate checkout of kubernetes-sigs/gateway-api at v1.6.3.
# Install the matching Standard CRDs in a disposable cluster.
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml

# From the gateway-api/conformance directory, using Go 1.26+:
go test . -run '^TestConformance$' -v -timeout=3h -args \
  --gateway-class=praxis \
  --conformance-profiles=GATEWAY-HTTP,GATEWAY-GRPC,GATEWAY-TLS,GATEWAY-TCP,GATEWAY-UDP \
  --supported-features=Gateway,ReferenceGrant,HTTPRoute,GRPCRoute,TLSRoute,TCPRoute,UDPRoute \
  --organization=praxis-proxy \
  --project=controller \
  --url=https://github.com/praxis-proxy/controller \
  --version=REPLACE_WITH_RELEASE_OR_COMMIT \
  --contact=REPLACE_WITH_MAINTAINER_GITHUB_OR_SUPPORT_URL \
  --mode=REPLACE_WITH_TESTED_DEPLOYMENT_MODE \
  --report-output=REPLACE_WITH_REPORT_PATH
```

The explicit supported-features list matters: otherwise v1.6.3 can infer features from GatewayClass status, potentially enabling Extended tests. The named profiles automatically add their Core features. Do not pass `--all-features`, `--allow-crds-mismatch`, `--skip-tests`, `--run-test`, or `--skip-provisional-tests` for a release claim. A focused `--run-test=ShortName` is appropriate during development only. The v1.6.3 release removed provisional marking from stable tests including TCP/UDP, so skipping provisional tests is unnecessary for this target. [Flags](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/utils/flags/flags.go), [feature inference/profile selection](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/utils/suite/suite.go), [release](https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.3).

The conformance suite is now a separate Go module: `sigs.k8s.io/gateway-api/conformance`, with a local replace to its parent checkout. Its v1.6.3 manifest uses Go 1.26, Kubernetes v0.36.1, and controller-runtime v0.24.1. Run it in its pinned external checkout; avoid importing its testing dependencies into the production controller's required v0.25.2/v0.37 stack. For repository-local harness tests, verify module compatibility explicitly. [Conformance go.mod](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/go.mod), [workspace](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/go.work).

Release gates:

1. All applicable Core tests pass with no deliberately skipped Core tests, unchanged upstream manifests, and matching bundle/channel. Unsupported Extended tests are expected to be excluded by feature gating; this is distinct from skipping required tests.
2. Capture full runner logs, exact command/options, image digests, controller/proxy logs with redaction, relevant status/manifests/events, and an unmodified generated report. Publish enough reproducible setup to identify dataplane deployment mode and network exposure.
3. Report fields identify actual implementation organization/project/contact/URL/version, Gateway API version/channel, and deployment mode. Check every requested profile's Core result. Do not mistake `partial` for complete Core success.
4. Submit a report upstream only for a real tested snapshot, in `conformance/reports/v1.6/<implementation>/`, with README reproduction instructions and `<channel>-<implementation-version>-<mode>-report.yaml`. Upstream's README has a version wording inconsistency (table asks for semver; later validation allows commit/tag snapshots): use a released semver for the public report to satisfy both.

[Reporting requirements](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/reports/README.md), [implementer no-skip rule](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/site/content/en/guides/implementers-guide/_index.md).

## 5. Required tests beyond upstream conformance

Conformance covers the tested interoperability contract, not complete correctness or security. Plan the following complementary checks:

| Layer | Evidence required |
|---|---|
| Pure translation | Table tests for defaults, precedence and ties, path boundaries, hostname/SNI intersection, header case rules, malformed/unsupported refs and filters, weighted invalid backends, deterministic output under input ordering, and equivalent updates producing no change |
| Domain update isolation | One EndpointSlice replacement changes only its service/port endpoint resource; cluster identity, listeners, unrelated routes, AI roots, and policy roots retain revisions; zero unrelated serializer/configurator calls |
| envtest | Actual v1.6.3 CRDs, schema defaults/CEL, production manager caches/indexes/watch mapping, Namespace-label and ReferenceGrant revocation, Secret/ConfigMap data changes, status ownership/conflicts/generation, cancellation and manager shutdown, no recovered panics |
| Real cluster | Generated RBAC under controller ServiceAccount, leader election, multiple replicas, provisioning and rollouts, restart recovery, deletion/finalizers, actual EndpointSlices and Pod churn, connection draining, Secret rotation/revocation |
| Dataplane contract | ACK/NACK and stale revisions, partial failures, retry/idempotence, replay after controller/dataplane restart, dependency ordering, no dangling active route, listener restart transition, capability rejection, invalid references and weighted error responses |
| Security | Namespace escape attempts, grant removal while traffic is live, unauthorized backend/Secret refs, unknown extension kinds, restricted RBAC and negative permissions, authenticated TLS metrics, no sensitive material in logs/events/status/artifacts, payload/header/SNI fuzzing and resource budgets |
| Load/availability | Large route and EndpointSlice sets, one Pod replacement, sustained churn, bounded queue/cache/resource growth, connection continuity for mutable updates, restart behavior for immutable listener changes, bounded convergence to acknowledged revision |

The k8s-controller-runtime skill requires Ginkgo/Gomega envtest, downloading pinned control-plane assets, and checking panic metrics; envtest has no workload controllers, garbage collector, or meaningful default RBAC coverage. Test Deployment rollout/GC and normal in-cluster identity on a real cluster, and use an explicitly impersonated envtest user only for targeted authorization checks. Choose test Kubernetes versions compatible with the pinned Gateway CRDs and the production v0.37 client stack; a local API server version does not establish broader support.

## 6. Security review gates and unresolved prerequisites

The security model is a three-way separation of infrastructure, Gateway owners, and application Route owners. Test authorization at every reference edge before exposing Secret or endpoint data to the proxy. Namespace labels are security-sensitive when used by allowedRoutes selectors. Hostname precedence is deterministic but permits an older Route to add a hostname later; recommend explicit namespace/hostname delegation or admission policy in multitenant deployments, while preserving upstream routing semantics. [Upstream security model and hostname hijacking discussion](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/site/content/en/docs/concepts/security.md).

Before release, independently review generated RBAC, cache scope (especially Secret list/watch), controller ServiceAccount, authenticated TLS metrics, container hardening, deployment/config transport authentication, certificate storage/file permissions, cross-namespace status redaction, dependency CVEs, and route input amplification. Keep security policy revocation on the critical update path: stale last-known-good routing must not preserve a now-forbidden backend or Secret. Decide the exact fail-closed behavior for each authorization revocation and connector failure, and prove it with live traffic. These connector/revocation requirements are architecture recommendations inferred from the namespace trust model, not claims that upstream conformance already tests each case.

**Praxis prerequisite:** `GatewayModifyListeners` is a Core `Gateway` test. Praxis listener addresses/protocol/TLS enablement are not hot-reloadable per this repository's AGENTS.md. A final Core implementation therefore needs a real listener provisioning/restart strategy, or future native listener mutability. Rejecting every listener edit prevents the full Core goal. A transport-neutral interface alone cannot pass dataplane conformance; at least one working connector/deployment mode is needed before running the complete suite. [Core listener-change test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-modify-listeners.go).

## 7. Tagged Core feature-gated test inventory

The following inventory was extracted by source inspection, without running tests: select v1.6.3 `ConformanceTest` declarations whose complete `Features` list is a subset of `Gateway, ReferenceGrant, HTTPRoute, GRPCRoute, TLSRoute, TCPRoute, UDPRoute`. All 131 declarations in that test directory use the inspected literal Features form; 67 qualify and none of those 67 is marked provisional. These are combined-run eligibility counts, not a substitute for the runner's per-profile report classification. A test requiring more than one route kind (for example HTTPRouteDisallowedKind) needs the combined feature union. Each declaration also points to its YAML manifests and nested test cases; later implementation tickets must read those assertions, not infer behavior solely from a ShortName. [Entire pinned test directory](https://github.com/kubernetes-sigs/gateway-api/tree/v1.6.3/conformance/tests), [suite selection logic](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/utils/suite/suite.go).

### Gateway/shared: 10 tests

| ShortName / pinned source | Required features |
|---|---|
| [GatewayListenerUnsupportedProtocol](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-invalid-listeners-unsupported-protocol.go) | Gateway |
| [GatewayInvalidRouteKind](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-invalid-route-kind.go) | Gateway |
| [GatewayInvalidTLSConfiguration](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-invalid-tls-configuration.go) | Gateway |
| [GatewayModifyListeners](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-modify-listeners.go) | Gateway |
| [GatewayObservedGenerationBump](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-observed-generation-bump.go) | Gateway |
| [GatewaySecretInvalidReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-secret-invalid-reference-grant.go) | Gateway, ReferenceGrant |
| [GatewaySecretMissingReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-secret-missing-reference-grant.go) | Gateway, ReferenceGrant |
| [GatewaySecretReferenceGrantAllInNamespace](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-secret-reference-grant-all-in-namespace.go) | Gateway, ReferenceGrant |
| [GatewaySecretReferenceGrantSpecific](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-secret-reference-grant-specific.go) | Gateway, ReferenceGrant |
| [GatewayClassObservedGenerationBump](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gatewayclass-observed-generation-bump.go) | Gateway |

### HTTPRoute: 27 tests

| ShortName / pinned source | Required features |
|---|---|
| [GatewayWithAttachedRoutes](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/gateway-with-attached-routes.go) | Gateway, HTTPRoute |
| [HTTPRouteCrossNamespace](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-cross-namespace.go) | Gateway, HTTPRoute |
| [HTTPRouteDisallowedKind](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-disallowed-kind.go) | Gateway, HTTPRoute, TLSRoute |
| [HTTPRouteExactPathMatching](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-exact-path-matching.go) | Gateway, HTTPRoute |
| [HTTPRouteHeaderMatching](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-header-matching.go) | Gateway, HTTPRoute |
| [HTTPRouteHostnameIntersection](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-hostname-intersection.go) | Gateway, HTTPRoute |
| [HTTPRouteHTTPSListener](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-https-listener.go) | Gateway, HTTPRoute |
| [HTTPRouteInvalidBackendRefUnknownKind](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-invalid-backendref-unknown-kind.go) | Gateway, HTTPRoute |
| [HTTPRouteInvalidCrossNamespaceBackendRef](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-invalid-cross-namespace-backend-ref.go) | Gateway, HTTPRoute, ReferenceGrant |
| [HTTPRouteInvalidCrossNamespaceParentRef](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-invalid-cross-namespace-parent-ref.go) | Gateway, HTTPRoute |
| [HTTPRouteInvalidNonExistentBackendRef](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-invalid-nonexistent-backendref.go) | Gateway, HTTPRoute |
| [HTTPRouteInvalidParentRefNotMatchingSectionName](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-invalid-parentref-not-matching-section-name.go) | Gateway, HTTPRoute |
| [HTTPRouteInvalidReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-invalid-reference-grant.go) | Gateway, HTTPRoute, ReferenceGrant |
| [HTTPRouteListenerHostnameMatching](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-listener-hostname-matching.go) | Gateway, HTTPRoute |
| [HTTPRouteMatchingAcrossRoutes](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-matching-across-routes.go) | Gateway, HTTPRoute |
| [HTTPRouteMatching](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-matching.go) | Gateway, HTTPRoute |
| [HTTPRouteMultipleGateways](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-multiple-gateways.go) | Gateway, HTTPRoute |
| [HTTPRouteObservedGenerationBump](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-observed-generation-bump.go) | Gateway, HTTPRoute |
| [HTTPRouteNoBackendRefs](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-omitted-backendrefs.go) | Gateway, HTTPRoute |
| [HTTPRoutePartiallyInvalidViaInvalidReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-partially-invalid-via-invalid-reference-grant.go) | Gateway, HTTPRoute, ReferenceGrant |
| [HTTPRoutePathMatchOrder](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-path-match-order.go) | Gateway, HTTPRoute |
| [HTTPRouteRedirectHostAndStatus](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-redirect-host-and-status.go) | Gateway, HTTPRoute |
| [HTTPRouteReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-reference-grant.go) | Gateway, HTTPRoute, ReferenceGrant |
| [HTTPRouteRequestHeaderModifier](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-request-header-modifier.go) | Gateway, HTTPRoute |
| [HTTPRouteServiceTypes](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-service-types.go) | Gateway, HTTPRoute |
| [HTTPRouteSimpleSameNamespace](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-simple-same-namespace.go) | Gateway, HTTPRoute |
| [HTTPRouteWeight](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/httproute-weight.go) | Gateway, HTTPRoute |

### GRPCRoute: 4 tests

| ShortName / pinned source | Required features |
|---|---|
| [GRPCExactMethodMatching](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/grpcroute-exact-method-matching.go) | Gateway, GRPCRoute |
| [GRPCRouteHeaderMatching](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/grpcroute-header-matching.go) | Gateway, GRPCRoute |
| [GRPCRouteListenerHostnameMatching](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/grpcroute-listener-hostname-matching.go) | Gateway, GRPCRoute |
| [GRPCRouteWeight](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/grpcroute-weight.go) | Gateway, GRPCRoute |

### TLSRoute: 9 tests

| ShortName / pinned source | Required features |
|---|---|
| [TLSRouteHostnameIntersection](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-hostname-intersection.go) | Gateway, TLSRoute |
| [TLSRouteInvalidBackendRefNonexistent](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-invalid-backendref-nonexistent.go) | Gateway, TLSRoute |
| [TLSRouteInvalidBackendRefUnknownKind](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-invalid-backendref-unknown-kind.go) | Gateway, TLSRoute |
| [TLSRouteInvalidNoMatchingListenerHostname](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-invalid-no-matching-listener-hostname.go) | Gateway, TLSRoute |
| [TLSRouteInvalidNoMatchingListener](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-invalid-no-matching-listener.go) | Gateway, TLSRoute |
| [TLSRouteInvalidReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-invalid-reference-grant.go) | Gateway, TLSRoute, ReferenceGrant |
| [TLSRouteListenerPassthroughSupportedKinds](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-listener-passthrough-supported-kinds.go) | Gateway, TLSRoute |
| [TLSRouteListenerTerminateNotSupported](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-listener-terminate-not-supported.go) | Gateway, TLSRoute |
| [TLSRouteSimpleSameNamespace](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tlsroute-simple-same-namespace.go) | Gateway, TLSRoute |

### TCPRoute: 8 tests

| ShortName / pinned source | Required features |
|---|---|
| [TCPRouteInvalidBackendRefNonexistent](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-invalid-backendref-nonexistent.go) | Gateway, TCPRoute |
| [TCPRouteInvalidCrossNamespaceBackendRef](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-invalid-cross-namespace-backend-ref.go) | Gateway, TCPRoute, ReferenceGrant |
| [TCPRouteInvalidNonTCPListener](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-invalid-non-tcp-listener.go) | Gateway, TCPRoute |
| [TCPRouteMultipleRoutesAttachment](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-multiple-routes-attachment.go) | Gateway, TCPRoute |
| [TCPRouteParentRefAttachAll](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-parentref-attach-all.go) | Gateway, TCPRoute |
| [TCPRouteParentRefPortAndSectionName](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-parentref-port-and-section-name.go) | Gateway, TCPRoute |
| [TCPRouteReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-reference-grant.go) | Gateway, TCPRoute, ReferenceGrant |
| [TCPRouteWeightedRouting](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/tcproute-weighted-routing.go) | Gateway, TCPRoute |

### UDPRoute: 9 tests

| ShortName / pinned source | Required features |
|---|---|
| [UDPRouteInvalidBackendRefNonexistent](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-invalid-backendref-nonexistent.go) | Gateway, UDPRoute |
| [UDPRouteInvalidCrossNamespaceBackendRef](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-invalid-cross-namespace-backend-ref.go) | Gateway, UDPRoute, ReferenceGrant |
| [UDPRouteMultipleRoutesAttachment](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-multiple-routes-attachment.go) | Gateway, UDPRoute |
| [UDPRouteNotAllowedByListeners](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-not-allowed-by-listeners.go) | Gateway, UDPRoute |
| [UDPRouteParentRefAttachAllListeners](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-parentref-attach-all-listeners.go) | Gateway, UDPRoute |
| [UDPRouteParentRefPortAndSectionName](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-parentref-port-and-section-name.go) | Gateway, UDPRoute |
| [UDPRouteReferenceGrant](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-reference-grant.go) | Gateway, UDPRoute, ReferenceGrant |
| [UDPRoute](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-simple.go) | UDPRoute, Gateway |
| [UDPRouteWeightedRouting](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/conformance/tests/udproute-weighted-routing.go) | Gateway, UDPRoute |

## 8. Evidence limits

No conformance tests, Kubernetes clusters, controller code, proxy behavior, or connector prototypes were run for this research. Feature inventory and version claims come from tagged source and release metadata; the behavioral recommendations still require implementation and real traffic evidence. Online documentation and some tagged prose lag the GA API state; retain this pin and record each future upgrade explicitly.
