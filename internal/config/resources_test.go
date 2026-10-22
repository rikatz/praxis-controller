package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRevisionAndChangeSetIDIgnoreInputOrdering(t *testing.T) {
	target := Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	epoch := target.UID
	listener, err := NewResource(Key{Domain: DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: KindListener, Name: "web"}, ObjectRef{Kind: "Gateway", Namespace: "edge", Name: "public"}, Listener{Port: 8080, Protocol: ListenerHTTP, RouteTable: "web"})
	if err != nil {
		t.Fatal(err)
	}
	route, err := NewResource(Key{Domain: DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: KindHTTPRoute, Name: "web"}, ObjectRef{Kind: "Gateway", Namespace: "edge", Name: "public"}, HTTPRouteTable{})
	if err != nil {
		t.Fatal(err)
	}
	first := ChangeSet{Target: target, OwnerEpoch: epoch, Upserts: []Resource{listener, route}}
	second := ChangeSet{Target: target, OwnerEpoch: epoch, Upserts: []Resource{route, listener}}
	firstID, err := first.StableID()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := second.StableID()
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID {
		t.Fatalf("stable ID changed with resource order: %s != %s", firstID, secondID)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("valid change set rejected: %v", err)
	}
}

func TestChangeSetRejectsTamperedResourceAndForeignTarget(t *testing.T) {
	target := Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	epoch := target.UID
	resource, err := NewResource(Key{Domain: DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: KindListener, Name: "web"}, ObjectRef{Kind: "Gateway", Namespace: "edge", Name: "public"}, Listener{Port: 8080, Protocol: ListenerHTTP})
	if err != nil {
		t.Fatal(err)
	}
	resource.Value = Listener{Port: 8443, Protocol: ListenerHTTPS}
	if err := (ChangeSet{Target: target, OwnerEpoch: epoch, Upserts: []Resource{resource}}).Validate(); err == nil {
		t.Fatal("expected changed resource payload with stale revision to fail validation")
	}

	foreign := resource
	foreign.Key.Target.Name = "other"
	if err := (ChangeSet{Target: target, OwnerEpoch: epoch, Upserts: []Resource{foreign}}).Validate(); err == nil {
		t.Fatal("expected foreign target resource to fail validation")
	}
}

func TestChangeSetAllowsDeletingOnlyFencedEpochKeysForTheSameTarget(t *testing.T) {
	target := Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	oldKey := Key{Domain: DomainPraxis, Target: target, OwnerEpoch: "old-owner", Kind: KindEndpointSet, Name: "backend"}
	changes := ChangeSet{Target: target, OwnerEpoch: "new-owner", Deletes: []Key{oldKey}}
	if err := changes.Validate(); err != nil {
		t.Fatalf("old owner resource could not be retired by the current owner: %v", err)
	}

	foreign := oldKey
	foreign.Target.Name = "other"
	changes.Deletes = []Key{foreign}
	if err := changes.Validate(); err == nil {
		t.Fatal("accepted deletion for a different target")
	}
}

func TestSecretRotationMetadataChangesOnlySecretHandleRevision(t *testing.T) {
	target := Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	epoch := target.UID
	key := Key{Domain: DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: KindSecretHandle, Name: "edge/cert"}
	source := ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: "edge", Name: "cert", UID: "secret-uid"}
	first, err := NewResource(key, source, SecretHandle{Namespace: "edge", Name: "cert", UID: "secret-uid", ResourceVersion: "12"})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewResource(key, source, SecretHandle{Namespace: "edge", Name: "cert", UID: "secret-uid", ResourceVersion: "13"})
	if err != nil {
		t.Fatal(err)
	}
	listenerKey := Key{Domain: DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: KindListener, Name: "https"}
	listenerValue := Listener{Port: 443, Protocol: ListenerHTTPS, Secrets: []SecretRef{{Namespace: "edge", Name: "cert"}}}
	listener, err := NewResource(listenerKey, source, listenerValue)
	if err != nil {
		t.Fatal(err)
	}
	rotatedListener, err := NewResource(listenerKey, source, listenerValue)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == rotated.Revision {
		t.Fatal("Secret resourceVersion change did not change the metadata-only handle revision")
	}
	if listener.Revision != rotatedListener.Revision {
		t.Fatal("Secret rotation metadata unexpectedly changed the listener reference revision")
	}
	if !hasNoSecretDataFields(t, first.Value) {
		t.Fatal("secret handle contains a data field")
	}
}

func hasNoSecretDataFields(t *testing.T, value Value) bool {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return !bytes.Contains(b, []byte(`"data"`)) && !bytes.Contains(b, []byte(`"stringData"`))
}
