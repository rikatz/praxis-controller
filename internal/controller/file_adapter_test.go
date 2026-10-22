package controller

import (
	"context"
	"testing"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	"github.com/praxis-proxy/controller/internal/delivery/file"
)

func TestFileAdapterDeliveryNeverBecomesActive(t *testing.T) {
	target := config.Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	epoch := "test-owner-epoch"
	configurator, err := file.NewConfigurator(file.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	resources := []config.Resource{
		mustResource(t, target, epoch, config.KindListener, "web", config.Listener{Port: 80, Protocol: config.ListenerHTTP, RouteTable: "route-web"}),
		mustResource(t, target, epoch, config.KindHTTPRoute, "route-web", config.HTTPRouteTable{Rules: []config.HTTPRule{{
			Matches: []config.HTTPMatch{{PathType: "PathPrefix", Path: "/"}},
			Action:  config.HTTPAction{Kind: config.ActionForward, Backends: []config.BackendRef{{Cluster: "service-80-tcp-abc", Valid: true}}},
		}}}),
		mustResource(t, target, epoch, config.KindCluster, "service-80-tcp-abc", config.Cluster{EndpointSet: "eps-1"}),
		mustResource(t, target, epoch, config.KindEndpointSet, "eps-1", config.EndpointSet{
			Endpoints: []config.Endpoint{{Address: "10.0.0.1", Port: 8080, Ready: true, Serving: true}},
		}),
	}

	reconciler := &Reconciler{Configurator: configurator, OwnerEpoch: epoch}
	var receipt *delivery.Receipt
	observation, err := reconciler.applyDesired(context.Background(), target, epoch, resources, false, &receipt)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || len(receipt.Resources) == 0 {
		t.Fatal("file adapter did not return a receipt")
	}
	if allResourcesActive(target, resources, receipt, observation) {
		t.Fatal("delivered file configuration was treated as active")
	}
}

func mustResource(t *testing.T, target config.Target, epoch string, kind config.Kind, name string, value config.Value) config.Resource {
	t.Helper()
	resource, err := config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: kind, Name: name},
		config.ObjectRef{Kind: string(kind), Name: name},
		value,
	)
	if err != nil {
		t.Fatal(err)
	}
	return resource
}
