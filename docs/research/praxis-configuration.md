# Praxis configuration and runtime evidence

Research date: 2026-10-19. This is an implementation prerequisite inventory,
not a claim of Gateway API conformance. No controller or proxy implementation was
changed, and no runtime or conformance tests were executed for this research.

## Source baseline

The following repositories were freshly cloned and inspected at these revisions.
Links below are pinned to the inspected source, rather than moving `main` URLs.

| Repository | Inspected revision |
| --- | --- |
| `praxis-proxy/praxis` | `d1d8e1f3d2cae000d16bd34f385733ff2b620ff6` |
| `praxis-proxy/ai` | `5502cd9719c10d50ba13e5a57a38305765a650dd` |
| `praxis-proxy/policy` | `b006806ef1e7acebfdf148d1c623fb730a507227` |

Local source inspection is the evidence for this document; GitHub page rendering
was unavailable to the research browser. Revalidate these findings against the
exact proxy and AI binary dependencies selected for implementation: the AI server
has its own reload path and must not be assumed to inherit every change in the
current standalone proxy server.

## Findings that determine the controller architecture

1. **Existing whole-file reload does not satisfy independent endpoint updates.**
   Proxy `handle_reload` rereads and reparses the main YAML after a main or
   referenced-file hash change. `reload_pipelines` builds every listener pipeline,
   applies composition extension factories again, swaps existing listener slots,
   and respawns affected health work. It explicitly warns about stateful filter
   resets. Splitting generated YAML into files without changing this runtime path
   would not satisfy the user's requirement. [Watcher and hash handling][P-watch],
   [pipeline rebuild and publication][P-reload], [listener pipeline construction][P-pipelines].
2. **Serving clusters currently live inside load-balancer filter configuration.**
   Root `Config.clusters` supplies the health registry and administrative metadata.
   HTTP `LoadBalancerConfig` separately deserializes its own `clusters`; its
   constructor builds a private cluster map and strategies. TCP load balancing
   does the same. Updating only root `clusters` does not update those serving
   endpoint sets. The controller model must contain one canonical cluster and
   endpoint set; a connector must correctly wire both health and serving state.
   [Root configuration][P-config], [HTTP load balancer][P-lb],
   [TCP load balancer][P-tcp-lb], [reload registry wiring][P-reload].
3. **No selected transport can be assumed yet.** The inspected file watcher is
   a configuration ingestion mechanism, not an xDS server or a delta resource
   protocol. Define the controller's desired resources and acknowledgement
   contract first. A file agent, direct embedded runtime connector, xDS transport,
   or another authenticated control channel can then implement that contract.
4. **Bootstrap and live resources have different lifecycles.** Listener protocol
   changes and TCP listener group topology changes are rejected on live reload.
   Other startup changes are diagnosed as requiring restart. A successful file
   write or general reload result cannot establish that a new socket, listener
   address or TLS mode is serving. [Reload guards][P-reload],
   [restart diagnostics][P-diagnostics].
5. **UDP is a proxy prerequisite.** `ProtocolKind` contains only `Http` and `Tcp`.
   An `UDPRoute` translator alone cannot deliver UDP traffic. A UDP runtime,
   listener registration, routing/endpoint selection, session lifecycle, bounded
   buffers, metrics and application acknowledgement are needed before claiming
   support. [Protocol enum][P-listener].

## Available protocol building blocks and gaps

| Requirement | Evidence in current Praxis | Required design or validation work |
| --- | --- | --- |
| HTTP path matches | Native route supports exact and segment-aware prefix path matches. | Validate encoded paths, slash boundaries and complete Gateway API tie breaking; preserve original request path while matching. |
| Hostnames | Native router supports exact and wildcard names; wildcard multi-level matching is an explicit option, off by default. | Set Gateway suffix wildcard semantics explicitly; implement listener/route hostname intersection and hostname precedence. |
| HTTP precedence | Native router compares `(exact, path length, host plus header constraint count)`, keeping the earlier route on equal specificity. | Gateway precedence cannot be obtained merely by sorting routes if the runtime's comparator overrides that order. Implement or prove a lowering that handles hostname priority and the pinned Gateway spec's remaining tie breakers. |
| HTTP methods and query matching (Extended, excluded initially) | Native `Route` carries path, cluster, header equality constraints and host; no method/query fields. | Reject unsupported matches accurately for the Core-only implementation. A later Extended feature would need proven composition or a new runtime match representation. |
| Backend selection | Native HTTP route selects one cluster. Endpoint weights exist inside a cluster. | Service/backend-reference weight and invalid-backend weight are distinct from endpoint weight. Prove the translation against required Core tests rather than flattening them together. |
| Request/response header filters and redirects | Praxis provides filter pipeline composition. | Compile only required Core operations, including per-rule scope, order, multiplicity and terminal redirect behavior. Existence of a filter name is not evidence that its semantics match Gateway API. |
| gRPC | Cluster HTTP options support `h2`, using h2c prior knowledge for cleartext, and document trailers as necessary for `grpc-status`. | Force correct upstream HTTP/2 behavior, test downstream HTTP/2 and trailers, and compile service/method and header matches with exact Gateway precedence. Ordinary HTTP forwarding alone does not establish GRPCRoute conformance. |
| TLS passthrough | TCP protocol can inspect a bounded ClientHello and an `sni_router` routes exact/longest suffix names without HTTP parsing. | Test malformed/fragmented hellos, no-SNI rejection and Gateway attachment precedence. Add/prove routing through named cluster endpoint handles. |
| TLSRoute endpoint selection | `sni_router` writes `ctx.upstream_addr`; `tcp_load_balancer` reads `ctx.cluster`. | These existing contracts do not directly compose into SNI-selected dynamic clusters. A stable cluster selection seam is required; embedding a Pod address into SNI routes would couple route regeneration to Pod churn. |
| TCPRoute | TCP listener and TCP load balancer exist. | Bind a route to the selected cluster without HTTP dependencies; prove required connection/error behavior and backend weight semantics. |
| UDPRoute | No UDP protocol enum variant in this snapshot. | Proxy runtime work is a release blocker for UDP traffic support. |
| TLS certificate updates | Listener certificate watcher is enabled only for one certificate and the `config-reload` build feature; multiple certificates disable this path. | Prove Secret rotation and last-good behavior. A changed certificate list or multiple SNI certificates needs a new runtime update path or controlled restart. |

Sources: [route fields][P-route], [matching and specificity][P-match],
[wildcard option][P-router-config], [cluster HTTP/2 options][P-cluster],
[SNI routing contract][P-sni], [TCP load balancing contract][P-tcp-lb],
[ClientHello inspection][P-tcp-proxy], [certificate selection][P-cert],
[TLS watcher construction][P-tls-setup].

The table identifies source-level gaps and items to prove; it does not prescribe
adding every extended Gateway feature. The pinned Core inventory in the main plan
determines what is actually implemented.

## Independent resources and ownership

The controller should own a transport-neutral, deterministic desired-state graph.
These are conceptual internal resources, not a proposal to add Kubernetes CRDs for
each row. Freeze concrete Go interfaces only when implementing the first connector.

| Independent resource | Identity and content | Expected invalidation boundary |
| --- | --- | --- |
| Bootstrap/listener | Gateway UID, listener identity, bind address, protocol, TLS mode and runtime topology. | Listener/provisioning changes; restart where the selected runtime requires it. |
| Route table | Listener identity, ordered matches/actions, stable backend cluster references. | Route content, permitted attachment, hostname intersection or relevant Service metadata changes. |
| Cluster definition | Stable backend identity including Service UID, port and protocol; connection properties and endpoint-set reference. | Backend transport properties or Service identity changes. |
| Endpoint set | Cluster identity, canonical deduplicated addresses and ports, readiness/serving policy and address family. | EndpointSlice content affecting that cluster, including Pod replacement. |
| TLS material | Authorized Secret UID and version, keypair/certificate content digest, consumers. | That Secret's data changes or authorization is revoked. |
| AI root | Explicitly owned AI routing/overlay configuration, its own revision, stable cluster references. | AI configuration or AI ownership/dependency changes. |
| Policy root | Explicitly owned policy document, its own revision and references. | Policy changes or authorization changes, independently of endpoint revisions. |

Keep Gateway compilation, endpoint discovery, AI configuration and policy
configuration as separate producers with explicit ownership. Do not regenerate
AI/provider catalogs or policy documents by traversing arbitrary Gateway object
updates. Do not overwrite user-authored roots. AI/policy Kubernetes API shapes and
attachment policy remain a separate design decision; Core Gateway implementation
only needs to preserve these boundaries and existing externally supplied roots.

The core graph records each resource's semantic digest and dependencies. Source
resourceVersion supports observation/reconciliation but must not force output when
the relevant fields are unchanged. Preserve UID in identities to prevent a deleted
Service recreated under the same name from inheriting stale state.

### Pod replacement acceptance example

Given a Service-backed cluster and an unchanged route, AI root and policy root:

1. Aggregate all relevant EndpointSlices, resolve the named Service port, filter
   eligibility and address family, deduplicate and sort addresses.
2. Compare the endpoint set digest with the last desired endpoint revision.
3. Publish exactly that endpoint set's replacement under the stable cluster key.
4. Apply it atomically to new selections while allowing in-flight requests and
   connections to follow the chosen drain policy. Do not rebuild listener filters,
   route tables, AI snapshot, policy engine, rate limits or unrelated clusters.
5. Acknowledge the specific applied endpoint revision. A duplicate event produces
   no publication. A failed update preserves the previous usable revision until a
   successful replacement, except explicit security withdrawal which fails closed.

EndpointSlice events should be the serving membership source; watching every Pod
is unnecessary unless a later feature needs Pod fields absent from EndpointSlice.
The planner must not infer readiness solely from Pod lifecycle events.

### Connector contract to settle before runtime integration

The minimal contract needs resource identity/type, target identity, content digest,
upsert/delete operation, prerequisite resource versions and a desired revision. It
must return actual application acknowledgement or rejection with an actionable
reason; delivery acknowledgement alone is insufficient. Define whether atomicity
is per resource or a dependency-consistent bundle, and require resource versions
and stale-write rejection so concurrent reconcilers cannot restore withdrawn state.

Create prerequisite cluster/endpoint resources before publishing routes referring
to them; withdraw route references before collecting unused clusters. Where the
connector cannot transact related resources together, keep old compatible
resources until the new referring revision is acknowledged. Explicitly define
revocation behavior: authorization removal must stop traffic and cannot use
indefinite last-good fallback.

A connector must advertise capabilities such as independent endpoint updates,
listener restart/reprovisioning, multi-certificate rotation and protocol support.
Select capabilities at deployment time and gate status on them. Do not create a
generic plugin system or several transports in the initial work: one proven
connector plus a test implementation is enough, while the compiler remains free
of transport details.

For a file connector, separate artifacts, content hashes and atomic renames are
necessary but insufficient. The consumer must watch and apply each domain
independently and provide applied revision evidence. The current main-config
watcher cannot serve as that endpoint consumer without upstream runtime changes.

## AI and policy roots: existing independence and its limits

The AI server uses the Praxis `Config` plus its own registered filters. Its
whole-config reload also builds new pipelines. Existing store reload handling
stages, commits or aborts store generations; preserve that lifecycle if integrating
this binary. Do not assume a file rewrite is harmless to stores or filters.
[AI reload][A-reload], [AI startup and watchers][A-server].

AI intelligent routing already supports an independently watched overlay file,
scoped validation and an atomically swapped `RouteSnapshot`. It can constrain
overlay cluster names against the downstream load balancer's declared cluster
allowlist and retain a previous snapshot on invalid reload. These are useful
existing boundaries, not proof that endpoint updates are independent: downstream
load-balancer cluster definitions still belong to the broader pipeline. Credential
injection has its own watcher lifecycle, which should survive unrelated endpoint
updates. [Intelligent routing configuration][A-routing],
[overlay watcher][A-overlay], [credential watcher][A-credentials].

Proxy policy filters reference an external `config_path` document and load it at
construction. `referenced_files` exposes that document to the general config
watcher, so editing policy currently triggers whole-pipeline reload. That provides
external file separation but not independent policy application. The policy engine
itself has `load_config` and a coupled atomic runtime snapshot/generation; it
rebuilds plugins/executor and clears the routing cache, and refuses changes to
already resolved secret declarations. A future connector must deliberately use
and test that lifecycle, including initialization and secret rotation limitations.
It must not call policy reload during endpoint churn. [Policy filter configuration][P-policy-config],
[policy file declaration][P-policy-filter], [policy engine reload][E-engine].

## Security findings for the runtime connector

* Validate endpoint provenance and destination policy at the controller boundary.
  HTTP `resolve_checked` immediately accepts literal socket addresses; its DNS
  private-range restrictions do not protect generated literal Pod-IP destinations.
  A malicious or manually managed EndpointSlice must not become an unrestricted
  path to loopback, node-local services, cloud metadata or cross-tenant services.
  Define the supported selectorless-Service/manual-EndpointSlice behavior and
  deployment CIDR/identity boundary explicitly. [HTTP resolution][P-peer].
* TCP resolution applies the private/reserved-address guard after `lookup_host`,
  including literal private destinations. Its current narrow
  `trusted_private_endpoints` configuration is rejected by TCP load balancing
  because it only applies to HTTP. Private Pod destinations therefore need an
  upstream scoped trust mechanism, or a deployment-specific security decision
  backed by network enforcement; do not silently turn on global
  `allow_private_upstreams`. [TCP dial path][P-tcp-proxy], [TCP config validation][P-tcp-lb].
* Policy callouts and ordinary proxy upstreams have separate trust settings.
  Preserve that separation. A Gateway backend reference must never grant policy
  callouts access to arbitrary private destinations. [Policy trust settings][P-policy-config].
* Keep Secret values out of the general graph/debug output, events and metrics.
  Use references/digests in generic state; put material only in the authorized
  connector channel and tightly permissioned files. Explicitly zero/delete retired
  files where possible, and account for open descriptors and old process state.
* Authenticate and authorize any remote control channel, bind desired state to an
  intended dataplane identity and tenant, reject stale epochs/replayed deletes or
  upserts, bound resource sizes, and test partial publication and process crashes.

## Required verification before declaring the connector usable

The following are future implementation gates, not tests run by this research:

1. Instrument constructors and applied resource revisions. Replace one backend
   Pod and prove that only its endpoint-set digest changes; route tables,
   unrelated clusters, AI overlay, policy generation and listener identity remain
   unchanged. Assert retained rate-limit/circuit-breaker/session state where the
   chosen runtime provides it, and measure CPU/allocation work during churn.
2. Exercise no-ready-endpoint, restored endpoint, duplicate event, slice split/
   merge, duplicate address, IPv4/IPv6, named port, Service recreation and deletion.
   Confirm rejected endpoint updates do not make partial serving tables visible.
3. Hold HTTP requests, gRPC streams, TCP connections and UDP sessions across
   updates. Verify the documented draining rule and that new traffic stops
   selecting removed endpoints within the required reconciliation bound.
4. Rotate one Secret without replacing unrelated resources. Verify old/new
   handshakes, corrupt keypair rejection, multi-cert behavior, Secret deletion and
   authorization revocation. Exercise the actual proxy build features.
5. Test every required Core route semantic on the real Praxis binary: hostname/path
   precedence, exact header matching, backend weights and invalid backend
   responses, header mutations/redirects, HTTP/2 trailers and SNI passthrough.
   Upstream Gateway conformance remains the release gate; controller unit tests
   cannot prove transport semantics.
6. Fault-inject file rename, symlink rotation, connector disconnect, delayed ACK,
   stale revision, concurrent writers, crash between prepare/publish/ACK, and
   controller failover. After recovery, compare desired, published and applied
   digests rather than relying on a successful write counter.
7. Attempt malicious EndpointSlices, private/reserved targets, cross-namespace
   references, control-channel replay, certificate leakage and oversized resources.
   Confirm denial or bounded failure without global security bypasses.

[P-config]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/core/src/config.rs#L104
[P-watch]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/server/src/watcher.rs#L312
[P-reload]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/server/src/reload.rs#L60
[P-pipelines]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/server/src/pipelines.rs#L188
[P-diagnostics]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/server/src/reload_diagnostics.rs#L16
[P-listener]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/core/src/config/listener.rs#L158
[P-lb]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/filter/src/builtins/http/traffic_management/load_balancer/mod.rs#L134
[P-tcp-lb]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/filter/src/builtins/tcp/traffic_management/tcp_load_balancer.rs#L32
[P-route]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/core/src/config/route.rs#L214
[P-match]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/filter/src/builtins/http/traffic_management/router/matching.rs#L130
[P-router-config]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/filter/src/builtins/http/traffic_management/router/config.rs#L34
[P-cluster]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/core/src/config/cluster.rs#L34
[P-sni]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/filter/src/builtins/tcp/traffic_management/sni_router.rs#L74
[P-tcp-proxy]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/protocol/src/tcp/proxy.rs#L883
[P-cert]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/tls/src/config/listener.rs#L333
[P-tls-setup]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/protocol/src/tls_setup.rs#L83
[P-peer]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/core/src/connectivity/peer.rs#L826
[P-policy-config]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/filter/src/builtins/http/security/policy/config.rs#L32
[P-policy-filter]: https://github.com/praxis-proxy/praxis/blob/d1d8e1f3d2cae000d16bd34f385733ff2b620ff6/crates/filter/src/builtins/http/security/policy/filter.rs#L1710
[A-reload]: https://github.com/praxis-proxy/ai/blob/5502cd9719c10d50ba13e5a57a38305765a650dd/server/src/reload.rs#L48
[A-server]: https://github.com/praxis-proxy/ai/blob/5502cd9719c10d50ba13e5a57a38305765a650dd/server/src/server.rs#L90
[A-routing]: https://github.com/praxis-proxy/ai/blob/5502cd9719c10d50ba13e5a57a38305765a650dd/filters/src/routing/intelligent_route.rs#L112
[A-overlay]: https://github.com/praxis-proxy/ai/blob/5502cd9719c10d50ba13e5a57a38305765a650dd/filters/src/routing/overlay.rs#L946
[A-credentials]: https://github.com/praxis-proxy/ai/blob/5502cd9719c10d50ba13e5a57a38305765a650dd/filters/src/routing/credential_inject.rs#L387
[E-engine]: https://github.com/praxis-proxy/policy/blob/b006806ef1e7acebfdf148d1c623fb730a507227/crates/ppe-core/src/engine.rs#L1383

## Revision drift addendum (2026-10-22)

The local Praxis clone inspected while implementing the file adapter is at
`1ea7a79a`, not the pinned `d1d8e1f3` above. `d1d8e1f3` is not present in that
clone's history, so this is a spot-check, not a full diff. Facts verified
against the local HEAD and used by the renderer:

- `insecure_options.allow_private_endpoints` governs private literal data-plane
  addresses; `allow_private_upstreams` is the TCP-scoped option and
  `trusted_private_endpoints` is a hostname allowlist. The renderer emits none
  of them by default.
- A listener requires `name` and `address`; there is no port-only form.
- The router supports `path`, `path_prefix`, `host`, `headers` (exact,
  case-sensitive) and `multi_level_subdomain_matching` (needed for Gateway API
  wildcard semantics).
- A router route selects exactly one cluster. The load balancer supports
  endpoint weights inside a cluster, not route weights across clusters.
- Empty clusters are rejected at configuration validation.
- Listener add/remove/rebind and any `tls` block change require restart;
  certificate file content may hot-reload only in the single-certificate
  `config-reload` configuration.
- Dynamic reload covers router routes and load-balancer endpoints/weights but
  rebuilds pipelines. It does not satisfy independent endpoint updates.

Re-fetch the pinned revision and repeat this comparison before making a
conformance or release claim.
