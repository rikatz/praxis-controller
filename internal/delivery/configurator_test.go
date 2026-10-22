package delivery

import (
	"testing"

	"github.com/praxis-proxy/controller/internal/config"
)

func TestDiffIsScopedToOneDomainAndOnlyChangedResources(t *testing.T) {
	target := config.Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	epoch := target.UID
	listener, err := config.NewResource(config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindListener, Name: "web"}, config.ObjectRef{Kind: "Gateway", Namespace: "edge", Name: "public"}, config.Listener{Port: 8080, Protocol: config.ListenerHTTP})
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := config.NewResource(config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindCluster, Name: "api"}, config.ObjectRef{Kind: "Service", Namespace: "app", Name: "api"}, config.Cluster{ServiceNamespace: "app", ServiceName: "api", Port: 8080, EndpointSet: "api"})
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := config.NewResource(config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindEndpointSet, Name: "api"}, config.ObjectRef{Kind: "Service", Namespace: "app", Name: "api"}, config.EndpointSet{ServiceNamespace: "app", ServiceName: "api", Port: 8080, Endpoints: []config.Endpoint{{Address: "192.0.2.10", Port: 8080, Ready: true, Serving: true}}})
	if err != nil {
		t.Fatal(err)
	}
	aiRoot, err := config.NewResource(config.Key{Domain: config.DomainAI, Target: target, OwnerEpoch: epoch, Kind: config.KindOpaqueRoot, Name: "ai"}, config.ObjectRef{Kind: "AIRoot", Name: "ai"}, config.OpaqueRoot{Revision: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	policyRoot, err := config.NewResource(config.Key{Domain: config.DomainPolicy, Target: target, OwnerEpoch: epoch, Kind: config.KindOpaqueRoot, Name: "policy"}, config.ObjectRef{Kind: "PolicyRoot", Name: "policy"}, config.OpaqueRoot{Revision: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	changedEndpoints, err := config.NewResource(endpoints.Key, endpoints.Source, config.EndpointSet{ServiceNamespace: "app", ServiceName: "api", Port: 8080, Endpoints: []config.Endpoint{{Address: "192.0.2.11", Port: 8080, Ready: true, Serving: true}}})
	if err != nil {
		t.Fatal(err)
	}
	observed := Observation{Target: target, Resources: []ResourceResult{
		{Key: listener.Key, DesiredRevision: listener.Revision, ActiveRevision: listener.Revision, State: ApplyActive},
		{Key: cluster.Key, DesiredRevision: cluster.Revision, ActiveRevision: cluster.Revision, State: ApplyActive},
		{Key: endpoints.Key, DesiredRevision: endpoints.Revision, ActiveRevision: endpoints.Revision, State: ApplyActive},
		{Key: aiRoot.Key, DesiredRevision: aiRoot.Revision, ActiveRevision: aiRoot.Revision, State: ApplyActive},
		{Key: policyRoot.Key, DesiredRevision: policyRoot.Revision, ActiveRevision: policyRoot.Revision, State: ApplyActive},
	}}
	changes, err := Diff(target, epoch, config.DomainPraxis, []config.Resource{listener, cluster, changedEndpoints}, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upserts) != 1 || changes.Upserts[0].Key.Kind != config.KindEndpointSet {
		t.Fatalf("expected only endpoint set update, got %#v", changes.Upserts)
	}
	if len(changes.Deletes) != 0 {
		t.Fatalf("unexpected deletes: %#v", changes.Deletes)
	}
	if err := changes.Validate(); err != nil {
		t.Fatalf("invalid delta: %v", err)
	}
}

func TestDiffAcceptsDeliveredButNotPending(t *testing.T) {
	target := config.Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	epoch := "owner"
	resource, err := config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindListener, Name: "web"},
		config.ObjectRef{}, config.Listener{Port: 80, Protocol: config.ListenerHTTP, RouteTable: "route"},
	)
	if err != nil {
		t.Fatal(err)
	}

	delivered := Observation{Target: target, Resources: []ResourceResult{{
		Key: resource.Key, DesiredRevision: resource.Revision, DeliveredRevision: resource.Revision, State: ApplyDelivered,
	}}}
	changes, err := Diff(target, epoch, config.DomainPraxis, []config.Resource{resource}, delivered)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upserts) != 0 {
		t.Fatalf("delivered revision was re-sent: %+v", changes.Upserts)
	}

	pending := Observation{Target: target, Resources: []ResourceResult{{
		Key: resource.Key, DesiredRevision: resource.Revision, State: ApplyPending,
	}}}
	changes, err = Diff(target, epoch, config.DomainPraxis, []config.Resource{resource}, pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upserts) != 1 {
		t.Fatalf("pending revision was not re-sent: %+v", changes.Upserts)
	}
}

func TestRevisionHelpersAndReceiptValidation(t *testing.T) {
	target := config.Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	activeKey := config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: "e", Kind: config.KindListener, Name: "web"}
	deliveredKey := config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: "e", Kind: config.KindEndpointSet, Name: "api"}

	receipt := Receipt{Target: target, ChangeID: "change", Resources: []ResourceResult{
		{Key: activeKey, DesiredRevision: "a1", ActiveRevision: "a1", State: ApplyActive},
		{Key: deliveredKey, DeliveredRevision: "d1", State: ApplyDelivered},
		{Key: deliveredKey, DesiredRevision: "a2", ActiveRevision: "a2", State: ApplyActive},
	}}
	if revision, ok := receipt.ActiveRevision(activeKey); !ok || revision != "a1" {
		t.Fatalf("receipt active revision = %q, %v", revision, ok)
	}
	if revision, ok := receipt.DeliveredRevision(deliveredKey); !ok || revision != "d1" {
		t.Fatalf("receipt delivered revision = %q, %v", revision, ok)
	}

	observation := Observation{Target: target, Resources: []ResourceResult{
		{Key: activeKey, ActiveRevision: "a1", State: ApplyActive},
		{Key: deliveredKey, DeliveredRevision: "d1", State: ApplyDelivered},
	}}
	if revision, ok := observation.ActiveRevision(activeKey); !ok || revision != "a1" {
		t.Fatalf("observation active revision = %q, %v", revision, ok)
	}
	if revision, ok := observation.DeliveredRevision(deliveredKey); !ok || revision != "d1" {
		t.Fatalf("observation delivered revision = %q, %v", revision, ok)
	}
	if _, ok := observation.ActiveRevision(deliveredKey); ok {
		t.Fatal("delivered revision was reported as active")
	}

	changes := config.ChangeSet{Target: target, OwnerEpoch: "e", ID: "change"}
	if err := ValidateReceipt(changes, receipt); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	if err := ValidateReceipt(config.ChangeSet{Target: target, OwnerEpoch: "e", ID: "other"}, receipt); err == nil {
		t.Fatal("receipt with a mismatched change ID was accepted")
	}
	if err := ValidateReceipt(changes, Receipt{Target: config.Target{Namespace: "other"}, ChangeID: "change"}); err == nil {
		t.Fatal("receipt for another target was accepted")
	}
}

func TestDiffRetiresOnlyPriorEpochResourcesInTheRequestedDomain(t *testing.T) {
	target := config.Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	oldEpoch, currentEpoch := "old-owner", "new-owner"
	oldKey := config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: oldEpoch, Kind: config.KindEndpointSet, Name: "backend"}
	current, err := config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: currentEpoch, Kind: config.KindEndpointSet, Name: "backend"},
		config.ObjectRef{},
		config.EndpointSet{ServiceNamespace: "app", ServiceName: "backend", ServiceUID: "service-uid", Port: 8080, Protocol: "TCP"},
	)
	if err != nil {
		t.Fatal(err)
	}
	aiRoot, err := config.NewResource(
		config.Key{Domain: config.DomainAI, Target: target, OwnerEpoch: oldEpoch, Kind: config.KindOpaqueRoot, Name: "ai"},
		config.ObjectRef{}, config.OpaqueRoot{Revision: "ai-root"},
	)
	if err != nil {
		t.Fatal(err)
	}
	policyRoot, err := config.NewResource(
		config.Key{Domain: config.DomainPolicy, Target: target, OwnerEpoch: oldEpoch, Kind: config.KindOpaqueRoot, Name: "policy"},
		config.ObjectRef{}, config.OpaqueRoot{Revision: "policy-root"},
	)
	if err != nil {
		t.Fatal(err)
	}
	observed := Observation{Target: target, Resources: []ResourceResult{
		{Key: oldKey, ActiveRevision: "old-endpoints", State: ApplyActive},
		{Key: aiRoot.Key, ActiveRevision: aiRoot.Revision, State: ApplyActive},
		{Key: policyRoot.Key, ActiveRevision: policyRoot.Revision, State: ApplyActive},
	}}

	changes, err := Diff(target, currentEpoch, config.DomainPraxis, []config.Resource{current}, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upserts) != 1 || changes.Upserts[0].Key != current.Key {
		t.Fatalf("current epoch upserts = %+v, want only current EndpointSet", changes.Upserts)
	}
	if len(changes.Deletes) != 1 || changes.Deletes[0] != oldKey {
		t.Fatalf("stale Praxis deletes = %+v, want only old EndpointSet", changes.Deletes)
	}
	if err := changes.Validate(); err != nil {
		t.Fatalf("epoch-transition change set is invalid: %v", err)
	}
}
