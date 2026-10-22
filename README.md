# Praxis Gateway API controller

This repository implements the first mock-backed Gateway API v1.6.3 control-plane slice. It reconciles GatewayClass, Gateway, HTTPRoute, Service and EndpointSlice inputs into typed listener, route, cluster, endpoint and Secret-handle resources. The in-memory configurator journals resource-level changes and simulates activation; it does not serve requests and is not Gateway API conformance evidence.

The current slice covers HTTPRoute attachment/status, HTTP and HTTPS listeners, ReferenceGrant checks, Core path/header matching, Core request-header and redirect filters, and Service endpoint updates. GRPCRoute, TLSRoute, TCPRoute and UDPRoute, a real Praxis connector/provisioner, and request-serving behavior remain unimplemented.

## Run

Install the upstream Gateway API v1.6.3 Standard CRDs and RBAC in `config/rbac`, build with Go 1.27.2 or newer, and run with a Kubernetes credential that has the controller permissions:

```sh
make build
./bin/praxis-controller --metrics-bind-address=0 --leader-elect=false
```

The claimed `GatewayClass.spec.controllerName` defaults to `gateway.praxis-proxy.io/controller`. Mock operation logs include resource identity and revisions, not configuration values. A simulated active receipt can drive Gateway API `Programmed` status in this test implementation; it does not mean Praxis accepted configuration or served traffic.

An opt-in file adapter writes one Praxis YAML document per Gateway:

```sh
./bin/praxis-controller --delivery-adapter=file \
  --praxis-config-root=/var/run/praxis/config --praxis-tls-root=/etc/praxis/tls
```

It reports `delivered`, never `active`, so `Programmed` stays `False/Pending`; a successful write does not prove Praxis parsed, reloaded or served the configuration. Listener and TLS changes require a Praxis restart, loopback/link-local/cloud-metadata endpoints need an explicit `--praxis-allow-private-endpoints` opt-in, and endpoint updates are not independent because Praxis rebuilds pipelines on whole-file reload. See [file delivery](docs/gateway-api/file-delivery.md).

Secret watches use metadata-only objects. For an authorized HTTPS Secret reference, the controller directly reads the TLS Secret to validate its type and certificate/key pair, then passes only its UID, resourceVersion and validity to the compiler. The generated ClusterRole still grants cluster-wide Secret `get/list/watch`; these permissions also allow reading Secret data and require a narrower deployment security review. The normal secure metrics endpoint expects `tls.crt` and `tls.key` mounted at `/var/run/praxis/metrics/tls`; disabling metrics is intended only for local runs.

## Validate

```sh
go test ./...
go vet ./...
go tool controller-gen rbac:roleName=manager-role paths=./internal/controller output:rbac:dir=config/rbac
```

The implementation status and remaining gates are tracked in [the implementation plan](docs/gateway-api/implementation-plan.md), [architecture](docs/gateway-api/architecture.md), and [testing/security matrix](docs/gateway-api/testing-and-security.md). Full Core conformance requires a real Praxis delivery adapter and request-serving dataplane.
