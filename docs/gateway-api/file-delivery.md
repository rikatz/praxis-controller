# File delivery adapter (Option A)

Status: implemented for the HTTP/HTTPS Core slice. It delivers configuration;
it does not prove that a Praxis process parsed, reloaded or served it.

## What it does

The adapter lowers the compiled `praxis`-domain resources into one YAML
document per Gateway and replaces it atomically. The mock adapter remains the
default; the file adapter is opt-in in the same binary.

```sh
./bin/praxis-controller \
  --delivery-adapter=file \
  --praxis-config-root=/var/run/praxis/config \
  --praxis-tls-root=/etc/praxis/tls \
  --leader-elect=true
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--delivery-adapter` | `mock` | `mock` or `file` |
| `--praxis-config-root` | `/var/run/praxis/config` | Target root directory; must be writable |
| `--praxis-listener-bind-address` | `0.0.0.0` | Bind address used for every rendered listener |
| `--praxis-tls-root` | `/etc/praxis/tls` | Mount root for `tls.crt` / `tls.key` paths |
| `--praxis-gateway-address` | empty, repeatable | Static `Gateway.status.addresses` values |
| `--praxis-allow-private-endpoints` | `false` | Emit `insecure_options.allow_private_endpoints: true` |
| `--praxis-owner-stale-after` | `1m` | Owner-epoch takeover threshold |

## On-disk layout

```
<root>/<namespace>/<gateway>/<gateway-uid>/
    owner.json     0600   writer epoch, retired epochs, refresh time
    state.json     0600   target, epoch, desired resources and config digest
    config.yaml    0600   rendered Praxis configuration
```

Directories are `0700`. The Gateway UID is part of the path, so a recreated
Gateway never inherits or deletes the previous incarnation's files. An
externally started Praxis process must be pointed at the new path after a
Gateway is recreated.

Writes use a temporary file in the same directory, `fsync`, `rename` and a
directory `fsync`. `state.json` is written before `config.yaml`; a crash
between them is detected by digest mismatch and repaired on the next apply.
Duplicate applies do not rewrite the document.

## Observation and status

When no state exists (a fresh target), an apply that does not include a
listener is rejected rather than written. This prevents an EndpointSlice-only
event from replacing the document before the Gateway has been compiled. A
Gateway apply then rewrites the complete desired set.

An unreadable or inconsistent `state.json` is treated as absent, so the next
full-snapshot apply repairs it; a partial apply without a listener is still
rejected, so it cannot clobber a good document. An unparseable `owner.json` is
an error: ownership is never granted from a corrupt file. On a fresh target the
adapter trusts the change set to be the complete desired set; the controller
satisfies this because `Diff` against an empty observation sends every
resource. Removing a corrupt `state.json` makes the next Gateway apply rewrite
the complete desired set; removing a corrupt `owner.json` requires that no
active writer owns the target.

`Apply` and `Observe` report `delivered`, never `active`. `Programmed` therefore
remains `False/Pending` with this adapter, and `Gateway.status.addresses` is
empty unless `--praxis-gateway-address` is set. This is deliberate: a file
write, a rename or a file existence check is not runtime acknowledgement.
`Connected` means the target directory exists and is readable; it is `false`
for a missing target and never implies that Praxis is healthy.

A future control-plane/data-plane protocol (Option B) can implement the same
`delivery.Configurator` seam and report active revisions, at which point
`Programmed` becomes meaningful for that adapter.

## Representable configuration

Supported: HTTP and HTTPS listeners, exact and prefix paths, hostname, exact
header matches, one valid backend per rule (forward actions only), and clusters
with ready/serving/non-terminating endpoints. A listener whose route table is
empty renders a `static_response` 404, because Praxis rejects a router with no
routes. TLS is emitted as file paths; certificate bytes, Secret UIDs and
resourceVersions never enter the document.

Rejected at table or document granularity (no partial file is written):

- redirects and per-rule request-header modifiers;
- weighted backends across clusters, or more than one valid backend per rule;
- clusters with no ready endpoints (Praxis rejects empty clusters);
- two listeners sharing the same bind address and port;
- unsupported kinds, non-Praxis domains, missing references and invalid names.

## Limitations

- Whole-file reload: endpoint changes rewrite the document and Praxis rebuilds
  pipelines. Independent endpoint updates are not satisfied.
- Listener add/remove/rebind and any `tls` block change require a Praxis
  restart. Certificate content rotation is unproven in this slice.
- The provisioner is static: it prepares the directory, echoes the target
  identity and reports configured addresses. It does not create workloads,
  expose Services or observe a process.
- Sensitive endpoint addresses (loopback, link-local, this-host, unspecified
  and known cloud-metadata addresses) are rejected at render time by default.
  Normal RFC1918 Pod IPs are emitted and accepted. `--praxis-allow-private-endpoints`
  permits sensitive literals and is global: it turns off that guard for every
  Gateway and backend, so define scoped trust or network enforcement before
  enabling it in production.
- Owner fencing is an mtime lease with a TOCTOU window and clock/NFS
  assumptions. It is acceptable behind leader election, not for concurrent
  multi-writer operation. `Release` removes the UID directory including
  `owner.json`, so a stale writer for the same Gateway UID can re-adopt it and
  restore withdrawn configuration; a tombstone outside the directory is a
  followup for shared-root deployments.
- Capabilities are advertised (`IndependentEndpointSets=false`) but the
  controller does not consume them yet.

## Validation

```sh
go test ./...
go vet ./...
go test -race ./internal/delivery/... ./internal/praxis/...
go tool controller-gen rbac:roleName=manager-role paths=./internal/controller output:rbac:dir=config/rbac
```

RBAC is unchanged by the adapter. File mode needs a writable configuration
volume and a TLS mount supplied by the deployment; it adds no Kubernetes
permissions.
