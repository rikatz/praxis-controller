package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	controllerlog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestApplyOrdersDependenciesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	target := testTarget()
	resources, _ := baseResources(t, target, "epoch-1", "10.0.0.1")

	change := changesFor(target, "epoch-1", resources)
	receipt, err := configurator.Apply(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Simulated || len(receipt.Resources) != 4 {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	wantOrder := []config.Kind{config.KindEndpointSet, config.KindCluster, config.KindHTTPRoute, config.KindListener}
	for i, result := range receipt.Resources {
		if result.Key.Kind != wantOrder[i] || result.State != delivery.ApplyActive || result.ActiveRevision != result.DesiredRevision {
			t.Fatalf("receipt resource %d = %+v, want active %s", i, result, wantOrder[i])
		}
	}

	firstJournal := configurator.Journal()
	if len(firstJournal) != 4 {
		t.Fatalf("journal length = %d, want 4", len(firstJournal))
	}
	for i, entry := range firstJournal {
		if entry.Operation != operationAdd || entry.Key.Kind != wantOrder[i] || !entry.Simulated {
			t.Fatalf("journal entry %d = %+v", i, entry)
		}
	}

	second, err := configurator.Apply(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	if len(configurator.Journal()) != len(firstJournal) {
		t.Fatal("reapplying unchanged resources added journal entries")
	}
	for _, result := range second.Resources {
		if result.State != delivery.ApplyActive {
			t.Fatalf("replay did not return active receipt: %+v", result)
		}
	}
}

func TestEndpointOnlyUpdatePreservesOtherResourcesAndRoots(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	target := testTarget()
	const epoch = "epoch-1"
	if err := configurator.SeedAI(target, epoch, "ai-root", "fixture-ai"); err != nil {
		t.Fatal(err)
	}
	if err := configurator.SeedPolicy(target, epoch, "policy-root", "fixture-policy"); err != nil {
		t.Fatal(err)
	}
	resources, keys := baseResources(t, target, epoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changesFor(target, epoch, resources)); err != nil {
		t.Fatal(err)
	}
	before := observe(t, configurator, target)
	journalLen := len(configurator.Journal())

	updatedEndpoint, err := config.NewResource(keys.endpoint, config.ObjectRef{}, config.EndpointSet{
		ServiceNamespace: "backends",
		ServiceName:      "web",
		ServiceUID:       "service-uid",
		Port:             8080,
		Protocol:         "TCP",
		Endpoints:        []config.Endpoint{{Address: "10.0.0.2", Port: 8080, Ready: true, Serving: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := configurator.Apply(ctx, changesFor(target, epoch, []config.Resource{updatedEndpoint}))
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Resources) != 1 || receipt.Resources[0].Key != keys.endpoint || receipt.Resources[0].State != delivery.ApplyActive {
		t.Fatalf("endpoint update receipt = %+v", receipt)
	}
	after := observe(t, configurator, target)
	for key, revision := range before {
		if key == keys.endpoint {
			continue
		}
		if after[key] != revision {
			t.Errorf("unrelated resource %s changed revision: before %s, after %s", key, revision, after[key])
		}
	}
	if after[keys.endpoint] == before[keys.endpoint] {
		t.Fatal("endpoint revision did not change")
	}
	entries := configurator.Journal()
	if len(entries) != journalLen+1 {
		t.Fatalf("endpoint-only update added %d journal entries, want 1", len(entries)-journalLen)
	}
	entry := entries[len(entries)-1]
	if entry.Operation != operationUpdate || entry.Key != keys.endpoint || entry.OldRevision != before[keys.endpoint] || entry.NewRevision != after[keys.endpoint] {
		t.Fatalf("endpoint journal entry = %+v", entry)
	}
}

func TestApplyDeletesReferencesBeforeDependencies(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	target := testTarget()
	resources, keys := baseResources(t, target, "epoch-1", "10.0.0.1")
	if _, err := configurator.Apply(ctx, changesFor(target, "epoch-1", resources)); err != nil {
		t.Fatal(err)
	}

	deletes := []config.Key{keys.endpoint, keys.cluster, keys.route, keys.listener}
	receipt, err := configurator.Apply(ctx, config.ChangeSet{
		Target:     target,
		OwnerEpoch: "epoch-1",
		Deletes:    deletes,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []config.Key{keys.listener, keys.route, keys.cluster, keys.endpoint}
	if len(receipt.Resources) != len(wantOrder) {
		t.Fatalf("delete receipt = %+v", receipt)
	}
	for i, result := range receipt.Resources {
		if result.Key != wantOrder[i] || result.State != delivery.ApplyDeleted {
			t.Fatalf("delete result %d = %+v, want deleted %s", i, result, wantOrder[i])
		}
	}
	entries := configurator.Journal()
	for i, key := range wantOrder {
		entry := entries[len(entries)-len(wantOrder)+i]
		if entry.Operation != operationDelete || entry.Key != key || entry.NewRevision != "" || entry.OldRevision == "" {
			t.Fatalf("delete journal entry %d = %+v", i, entry)
		}
	}
	if len(observe(t, configurator, target)) != 0 {
		t.Fatal("deleted resources remain active")
	}
}

func TestNACKDisconnectAndRetryPreserveActiveRevision(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	target := testTarget()
	const epoch = "epoch-1"
	resources, keys := baseResources(t, target, epoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changesFor(target, epoch, resources)); err != nil {
		t.Fatal(err)
	}
	before := observe(t, configurator, target)[keys.endpoint]
	updated := endpointResource(t, keys.endpoint, "10.0.0.2")
	change := changesFor(target, epoch, []config.Resource{updated})
	if err := configurator.RejectNext(target, keys.endpoint); err != nil {
		t.Fatal(err)
	}
	rejected, err := configurator.Apply(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected.Resources) != 1 || rejected.Resources[0].State != delivery.ApplyRejected || rejected.Resources[0].ActiveRevision != before {
		t.Fatalf("NACK receipt = %+v", rejected)
	}
	observed := observe(t, configurator, target)
	if observed[keys.endpoint] != before {
		t.Fatal("NACK advanced the active endpoint revision")
	}
	if len(observeRejected(t, configurator, target)) != 1 {
		t.Fatal("observation did not retain the rejection")
	}

	recovered, err := configurator.Apply(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Resources[0].State != delivery.ApplyActive || observe(t, configurator, target)[keys.endpoint] != updated.Revision {
		t.Fatalf("retry did not recover: receipt=%+v", recovered)
	}

	third := endpointResource(t, keys.endpoint, "10.0.0.3")
	configurator.Disconnect(target)
	if _, err := configurator.Apply(ctx, changesFor(target, epoch, []config.Resource{third})); err == nil {
		t.Fatal("Apply succeeded while target was disconnected")
	}
	observation, err := configurator.Observe(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Connected || observe(t, configurator, target)[keys.endpoint] != updated.Revision {
		t.Fatal("disconnect changed active state or was not observed")
	}
	configurator.Reconnect(target)
	if _, err := configurator.Apply(ctx, changesFor(target, epoch, []config.Resource{third})); err != nil {
		t.Fatal(err)
	}
	if observe(t, configurator, target)[keys.endpoint] != third.Revision {
		t.Fatal("reconnected target did not apply the latest revision")
	}
}

func TestOwnerEpochFenceRejectsStaleWritesAndDeletes(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	target := testTarget()
	old, keys := baseResources(t, target, "epoch-1", "10.0.0.1")
	if _, err := configurator.Apply(ctx, changesFor(target, "epoch-1", []config.Resource{old[3]})); err != nil {
		t.Fatal(err)
	}
	activeBefore := observe(t, configurator, target)[keys.endpoint]

	if err := configurator.DelayNext(target, keys.endpoint, 1); err != nil {
		t.Fatal(err)
	}
	pending := endpointResource(t, keys.endpoint, "10.0.0.2")
	if receipt, err := configurator.Apply(ctx, changesFor(target, "epoch-1", []config.Resource{pending})); err != nil || receipt.Resources[0].State != delivery.ApplyPending {
		t.Fatalf("expected delayed old-epoch update, receipt=%+v err=%v", receipt, err)
	}
	observe(t, configurator, target) // leave the old ACK one observation away

	if err := configurator.Claim(ctx, target, "epoch-2"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []config.ChangeSet{
		changesFor(target, "epoch-1", []config.Resource{endpointResource(t, keys.endpoint, "10.0.0.3")}),
		{Target: target, OwnerEpoch: "epoch-1", Deletes: []config.Key{keys.endpoint}},
	} {
		if _, err := configurator.Apply(ctx, change); err == nil {
			t.Fatal("stale owner epoch changed target state")
		}
	}
	if got := observe(t, configurator, target)[keys.endpoint]; got != activeBefore {
		t.Fatalf("stale or delayed old-epoch write advanced active revision to %s", got)
	}
	if len(configurator.Journal()) != 1 {
		t.Fatal("stale or pending old-epoch changes were journaled")
	}
	if err := configurator.Claim(ctx, target, "epoch-1"); err == nil {
		t.Fatal("a retired owner epoch became current again")
	}

	newKey := keys.endpoint
	newKey.OwnerEpoch = "epoch-2"
	newResource := endpointResource(t, newKey, "10.0.0.4")
	if _, err := configurator.Apply(ctx, changesFor(target, "epoch-2", []config.Resource{newResource})); err != nil {
		t.Fatal(err)
	}
	active := observe(t, configurator, target)
	if active[keys.endpoint] != activeBefore || active[newKey] != newResource.Revision {
		t.Fatalf("epoch rotation did not preserve old observation and accept new writer: %v", active)
	}
}

func TestClaimPreservesOldInventoryAndCurrentEpochCanDeleteIt(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	target := testTarget()
	oldResources, oldKeys := baseResources(t, target, "epoch-1", "10.0.0.1")
	if _, err := configurator.Apply(ctx, changesFor(target, "epoch-1", oldResources)); err != nil {
		t.Fatal(err)
	}
	oldRevisions := observe(t, configurator, target)

	if err := configurator.Claim(ctx, target, "epoch-2"); err != nil {
		t.Fatal(err)
	}
	if active := observe(t, configurator, target); len(active) != len(oldResources) {
		t.Fatalf("claim discarded old active inventory: %v", active)
	}
	newResources, newKeys := baseResources(t, target, "epoch-2", "10.0.0.2")
	if _, err := configurator.Apply(ctx, changesFor(target, "epoch-2", newResources)); err != nil {
		t.Fatal(err)
	}
	journalBeforeDelete := len(configurator.Journal())
	unfencedKey := oldKeys.endpoint
	unfencedKey.OwnerEpoch = "epoch-3"
	if _, err := configurator.Apply(ctx, config.ChangeSet{
		Target: target, OwnerEpoch: "epoch-2", Deletes: []config.Key{unfencedKey},
	}); err == nil {
		t.Fatal("current epoch deleted a key from an epoch that was never fenced")
	}
	if len(configurator.Journal()) != journalBeforeDelete {
		t.Fatal("rejected unfenced delete changed the journal")
	}

	deletes := []config.Key{oldKeys.listener, oldKeys.route, oldKeys.cluster, oldKeys.endpoint}
	currentChange := config.ChangeSet{Target: target, OwnerEpoch: "epoch-2", Deletes: deletes}
	if _, err := configurator.Apply(ctx, currentChange); err != nil {
		t.Fatal(err)
	}
	active := observe(t, configurator, target)
	if len(active) != len(newResources) {
		t.Fatalf("active resources after retirement = %v, want only the replacement inventory", active)
	}
	for _, resource := range newResources {
		if active[resource.Key] != resource.Revision {
			t.Errorf("replacement resource %s revision = %q, want %q", resource.Key, active[resource.Key], resource.Revision)
		}
	}
	for _, key := range []config.Key{oldKeys.listener, oldKeys.route, oldKeys.cluster, oldKeys.endpoint} {
		if _, exists := active[key]; exists {
			t.Errorf("retired resource %s remains active", key)
		}
	}

	wantDeleteOrder := []config.Key{oldKeys.listener, oldKeys.route, oldKeys.cluster, oldKeys.endpoint}
	entries := configurator.Journal()
	if len(entries) != journalBeforeDelete+len(wantDeleteOrder) {
		t.Fatalf("cross-epoch delete added %d journal entries, want %d", len(entries)-journalBeforeDelete, len(wantDeleteOrder))
	}
	for i, key := range wantDeleteOrder {
		entry := entries[journalBeforeDelete+i]
		if entry.Operation != operationDelete || entry.Key != key || entry.OldRevision != oldRevisions[key] || entry.NewRevision != "" || !entry.Simulated {
			t.Errorf("cross-epoch delete journal entry %d = %+v, want delete of %s revision %q", i, entry, key, oldRevisions[key])
		}
	}

	staleChanges := []config.ChangeSet{
		changesFor(target, "epoch-1", []config.Resource{endpointResource(t, oldKeys.endpoint, "10.0.0.3")}),
		{Target: target, OwnerEpoch: "epoch-1", Deletes: []config.Key{oldKeys.listener}},
	}
	for _, change := range staleChanges {
		if _, err := configurator.Apply(ctx, change); err == nil {
			t.Fatal("stale owner epoch was accepted after retirement")
		}
	}
	if len(configurator.Journal()) != len(entries) {
		t.Fatal("stale changes after retirement modified the journal")
	}
	if _, err := configurator.Apply(ctx, currentChange); err != nil {
		t.Fatalf("repeating cross-epoch deletion should be idempotent: %v", err)
	}
	if len(configurator.Journal()) != len(entries) {
		t.Fatal("repeated cross-epoch deletion added journal entries")
	}
	if got := observe(t, configurator, target); len(got) != len(newResources) || got[newKeys.endpoint] == "" {
		t.Fatalf("idempotent deletion changed the replacement inventory: %v", got)
	}
}

func TestGatewayUIDReplacementIsASeparateTarget(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	oldTarget := testTarget()
	oldResources, oldKeys := baseResources(t, oldTarget, "epoch-1", "10.0.0.1")
	oldResource := oldResources[3]
	if _, err := configurator.Apply(ctx, changesFor(oldTarget, "epoch-1", []config.Resource{oldResource})); err != nil {
		t.Fatal(err)
	}

	newTarget := oldTarget
	newTarget.UID = "replacement-gateway-uid"
	if active := observe(t, configurator, newTarget); len(active) != 0 {
		t.Fatalf("replacement target inherited old Gateway resources: %v", active)
	}
	if _, err := configurator.Apply(ctx, config.ChangeSet{
		Target: newTarget, OwnerEpoch: "epoch-1", Deletes: []config.Key{oldKeys.endpoint},
	}); err == nil {
		t.Fatal("replacement Gateway deleted a prior UID's resource")
	}

	newKey := oldKeys.endpoint
	newKey.Target = newTarget
	newResource := endpointResource(t, newKey, "10.0.0.2")
	if _, err := configurator.Apply(ctx, changesFor(newTarget, "epoch-1", []config.Resource{newResource})); err != nil {
		t.Fatal(err)
	}
	if got := observe(t, configurator, oldTarget)[oldKeys.endpoint]; got != oldResource.Revision {
		t.Fatal("replacement target mutated the prior Gateway state")
	}
	if got := observe(t, configurator, newTarget)[newKey]; got != newResource.Revision {
		t.Fatal("new Gateway target did not accept its own resource")
	}
}

func TestDelayedReceiptBecomesSimulatedActiveOnlyAfterObservation(t *testing.T) {
	ctx := context.Background()
	configurator := NewConfigurator()
	target := testTarget()
	key := config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: "epoch-1", Kind: config.KindListener, Name: "http"}
	resource, err := config.NewResource(key, config.ObjectRef{}, config.Listener{Port: 8080, Protocol: config.ListenerHTTP})
	if err != nil {
		t.Fatal(err)
	}
	if err := configurator.DelayNext(target, key, 1); err != nil {
		t.Fatal(err)
	}
	receipt, err := configurator.Apply(ctx, changesFor(target, "epoch-1", []config.Resource{resource}))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Resources[0].State != delivery.ApplyPending || !receipt.Simulated {
		t.Fatalf("delayed receipt = %+v", receipt)
	}
	if got := observe(t, configurator, target); len(got) != 0 || len(configurator.Journal()) != 0 {
		t.Fatal("delayed resource became active before its observation delay elapsed")
	}
	if got := observe(t, configurator, target); got[key] != resource.Revision {
		t.Fatalf("delayed resource did not activate: %v", got)
	}
	if entries := configurator.Journal(); len(entries) != 1 || entries[0].Operation != operationAdd {
		t.Fatalf("delayed activation journal = %+v", entries)
	}
}

func TestApplyRejectsUnsupportedDomainAndKind(t *testing.T) {
	configurator := NewConfigurator()
	target := testTarget()
	cases := []struct {
		key   config.Key
		value config.Value
	}{
		{
			key:   config.Key{Domain: "other", Target: target, OwnerEpoch: "epoch-1", Kind: config.KindOpaqueRoot, Name: "root"},
			value: config.OpaqueRoot{Revision: "root"},
		},
		{
			key:   config.Key{Domain: config.DomainAI, Target: target, OwnerEpoch: "epoch-1", Kind: config.KindCluster, Name: "cluster"},
			value: config.Cluster{EndpointSet: "endpoints"},
		},
	}
	for _, tc := range cases {
		resource, err := config.NewResource(tc.key, config.ObjectRef{}, tc.value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := configurator.Apply(context.Background(), changesFor(target, "epoch-1", []config.Resource{resource})); err == nil {
			t.Fatalf("accepted unsupported %s/%s resource", tc.key.Domain, tc.key.Kind)
		}
	}
	if len(configurator.Journal()) != 0 {
		t.Fatal("invalid resources changed target state")
	}
}

func TestJournalAndErrorsDoNotExposeSecretLikeMetadata(t *testing.T) {
	ctx := context.Background()
	capture := &logCapture{}
	ctx = controllerlog.IntoContext(ctx, logr.New(&captureSink{capture: capture}))
	configurator := NewConfigurator()
	target := testTarget()
	const canary = "PRIVATE_KEY_CANARY_not_for_logs"
	key := config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: "epoch-1", Kind: config.KindSecretHandle, Name: "tls-cert"}
	resource, err := config.NewResource(key, config.ObjectRef{}, config.SecretHandle{
		Namespace:       target.Namespace,
		Name:            "tls-cert",
		UID:             "secret-uid",
		ResourceVersion: canary,
	})
	if err != nil {
		t.Fatal(err)
	}
	listenerKey := config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: "epoch-1", Kind: config.KindListener, Name: "https"}
	listener, err := config.NewResource(listenerKey, config.ObjectRef{}, config.Listener{
		Port:     8443,
		Protocol: config.ListenerHTTPS,
		Secrets:  []config.SecretRef{{Namespace: target.Namespace, Name: "tls-cert"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := configurator.Apply(ctx, changesFor(target, "epoch-1", []config.Resource{listener, resource}))
	if err != nil {
		t.Fatal(err)
	}
	listenerRevision := listener.Revision
	observation, err := configurator.Observe(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	journalJSON, err := json.Marshal(configurator.Journal())
	if err != nil {
		t.Fatal(err)
	}
	outputJSON, err := json.Marshal(struct {
		Receipt     delivery.Receipt     `json:"receipt"`
		Observation delivery.Observation `json:"observation"`
	}{receipt, observation})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journalJSON), canary) || strings.Contains(string(outputJSON), canary) || strings.Contains(capture.String(), canary) {
		t.Fatal("secret-like metadata appeared in the journal, receipt, observation or log")
	}

	rotated, err := config.NewResource(key, config.ObjectRef{}, config.SecretHandle{
		Namespace:       target.Namespace,
		Name:            "tls-cert",
		UID:             "secret-uid",
		ResourceVersion: "rv-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configurator.Apply(ctx, changesFor(target, "epoch-1", []config.Resource{rotated})); err != nil {
		t.Fatal(err)
	}
	active := observe(t, configurator, target)
	if active[listenerKey] != listenerRevision || active[key] != rotated.Revision {
		t.Fatal("secret rotation changed the listener or failed to update its handle")
	}
	entries := configurator.Journal()
	if len(entries) != 3 || entries[2].Operation != operationUpdate || entries[2].Key != key {
		t.Fatalf("secret rotation journal = %+v", entries)
	}
	if strings.Contains(capture.String(), canary) {
		t.Fatal("secret-like metadata appeared in logs after rotation")
	}

	bad := rotated
	bad.Revision = canary
	_, err = configurator.Apply(ctx, changesFor(target, "epoch-1", []config.Resource{bad}))
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("invalid change error leaked input: %v", err)
	}
}

func TestProvisionerReturnsGatewayScopedSimulatedTargets(t *testing.T) {
	ctx := context.Background()
	provisioner := NewProvisioner()
	intent := delivery.TargetIntent{
		Gateway:   config.ObjectRef{Kind: "Gateway", Namespace: "app", Name: "edge", UID: "gateway-uid"},
		Listeners: []delivery.ListenerIntent{{Name: "web", Port: 8080, Protocol: "HTTP"}},
	}
	first, err := provisioner.Ensure(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provisioner.Ensure(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if first.Target != second.Target || first.BootstrapRevision != second.BootstrapRevision || !first.Ready || !first.Simulated {
		t.Fatalf("Ensure was not stable and simulated: first=%+v second=%+v", first, second)
	}
	intent.Gateway.UID = "replacement-uid"
	replacement, err := provisioner.Ensure(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Target == first.Target {
		t.Fatal("Gateway UID replacement reused the old synthetic target")
	}
}

type fixtureKeys struct {
	endpoint config.Key
	cluster  config.Key
	route    config.Key
	listener config.Key
}

func baseResources(t *testing.T, target config.Target, epoch, endpointAddress string) ([]config.Resource, fixtureKeys) {
	t.Helper()
	keys := fixtureKeys{
		endpoint: config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindEndpointSet, Name: "endpoints"},
		cluster:  config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindCluster, Name: "cluster"},
		route:    config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindHTTPRoute, Name: "routes"},
		listener: config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: epoch, Kind: config.KindListener, Name: "listener"},
	}
	endpoint, err := config.NewResource(keys.endpoint, config.ObjectRef{}, config.EndpointSet{
		ServiceNamespace: "backends",
		ServiceName:      "web",
		ServiceUID:       "service-uid",
		Port:             8080,
		Protocol:         "TCP",
		Endpoints:        []config.Endpoint{{Address: endpointAddress, Port: 8080, Ready: true, Serving: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := config.NewResource(keys.cluster, config.ObjectRef{}, config.Cluster{
		ServiceNamespace: "backends",
		ServiceName:      "web",
		ServiceUID:       "service-uid",
		Port:             8080,
		Protocol:         "TCP",
		EndpointSet:      keys.endpoint.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	route, err := config.NewResource(keys.route, config.ObjectRef{}, config.HTTPRouteTable{Rules: []config.HTTPRule{{
		Source: config.ObjectRef{Kind: "HTTPRoute", Namespace: "app", Name: "route"},
		Action: config.HTTPAction{Kind: config.ActionForward, Backends: []config.BackendRef{{Cluster: keys.cluster.Name, Weight: 1, Valid: true}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := config.NewResource(keys.listener, config.ObjectRef{}, config.Listener{
		Port:       8080,
		Protocol:   config.ListenerHTTP,
		RouteTable: keys.route.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return []config.Resource{listener, route, cluster, endpoint}, keys
}

func endpointResource(t *testing.T, key config.Key, address string) config.Resource {
	t.Helper()
	resource, err := config.NewResource(key, config.ObjectRef{}, config.EndpointSet{
		ServiceNamespace: "backends",
		ServiceName:      "web",
		ServiceUID:       "service-uid",
		Port:             8080,
		Protocol:         "TCP",
		Endpoints:        []config.Endpoint{{Address: address, Port: 8080, Ready: true, Serving: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resource
}

func changesFor(target config.Target, epoch string, resources []config.Resource) config.ChangeSet {
	return config.ChangeSet{Target: target, OwnerEpoch: epoch, Upserts: resources}
}

func observe(t *testing.T, configurator *Configurator, target config.Target) map[config.Key]string {
	t.Helper()
	observation, err := configurator.Observe(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	active := make(map[config.Key]string, len(observation.Resources))
	for _, resource := range observation.Resources {
		active[resource.Key] = resource.ActiveRevision
	}
	return active
}

func observeRejected(t *testing.T, configurator *Configurator, target config.Target) []delivery.ResourceResult {
	t.Helper()
	observation, err := configurator.Observe(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	return observation.Rejected
}

func testTarget() config.Target {
	return config.Target{Namespace: "app", Name: "edge", UID: "gateway-uid"}
}

type logCapture struct {
	mu      sync.Mutex
	entries []string
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.entries, "\n")
}

type captureSink struct {
	capture *logCapture
	values  []any
	names   []string
}

func (s *captureSink) Init(logr.RuntimeInfo) {}
func (s *captureSink) Enabled(int) bool      { return true }
func (s *captureSink) Info(_ int, message string, values ...any) {
	s.write(message, nil, values...)
}
func (s *captureSink) Error(err error, message string, values ...any) {
	s.write(message, err, values...)
}
func (s *captureSink) WithValues(values ...any) logr.LogSink {
	combined := append(append([]any(nil), s.values...), values...)
	return &captureSink{capture: s.capture, values: combined, names: append([]string(nil), s.names...)}
}
func (s *captureSink) WithName(name string) logr.LogSink {
	names := append(append([]string(nil), s.names...), name)
	return &captureSink{capture: s.capture, values: append([]any(nil), s.values...), names: names}
}
func (s *captureSink) write(message string, err error, values ...any) {
	var line bytes.Buffer
	fmt.Fprint(&line, s.names, " ", message, " ", s.values, " ", values)
	if err != nil {
		fmt.Fprint(&line, " error=", err)
	}
	s.capture.mu.Lock()
	s.capture.entries = append(s.capture.entries, line.String())
	s.capture.mu.Unlock()
}
