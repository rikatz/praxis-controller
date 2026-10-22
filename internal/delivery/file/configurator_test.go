package file

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
)

var (
	testTarget = config.Target{Namespace: "default", Name: "edge", UID: "uid-1"}
	testEpoch  = "epoch-1"
)

func newConfigurator(t *testing.T, options Options) *Configurator {
	t.Helper()
	if options.Root == "" {
		options.Root = t.TempDir()
	}
	configurator, err := NewConfigurator(options)
	if err != nil {
		t.Fatalf("new configurator: %v", err)
	}
	return configurator
}

func makeResource(t *testing.T, epoch string, kind config.Kind, name string, value config.Value) config.Resource {
	t.Helper()
	resource, err := config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: testTarget, OwnerEpoch: epoch, Kind: kind, Name: name},
		config.ObjectRef{Kind: string(kind), Name: name},
		value,
	)
	if err != nil {
		t.Fatalf("new resource: %v", err)
	}
	return resource
}

func makeResources(t *testing.T, epoch string, address string) []config.Resource {
	t.Helper()
	return []config.Resource{
		makeResource(t, epoch, config.KindListener, "web", config.Listener{Port: 80, Protocol: config.ListenerHTTP, RouteTable: "route-web"}),
		makeResource(t, epoch, config.KindHTTPRoute, "route-web", config.HTTPRouteTable{Rules: []config.HTTPRule{{
			Matches:  []config.HTTPMatch{{PathType: "PathPrefix", Path: "/"}},
			Action:   config.HTTPAction{Kind: config.ActionForward, Backends: []config.BackendRef{{Cluster: "service-80-tcp-abc", Valid: true}}},
			Priority: 0,
		}}}),
		makeResource(t, epoch, config.KindCluster, "service-80-tcp-abc", config.Cluster{EndpointSet: "eps-1"}),
		makeResource(t, epoch, config.KindEndpointSet, "eps-1", config.EndpointSet{
			Endpoints: []config.Endpoint{{Address: address, Port: 8080, Ready: true, Serving: true}},
		}),
	}
}

func changeSet(t *testing.T, resources []config.Resource) config.ChangeSet {
	t.Helper()
	changes := config.ChangeSet{Target: testTarget, OwnerEpoch: testEpoch, Upserts: resources}
	id, err := changes.StableID()
	if err != nil {
		t.Fatal(err)
	}
	changes.ID = id
	if err := changes.Validate(); err != nil {
		t.Fatal(err)
	}
	return changes
}

func targetDir(t *testing.T, configurator *Configurator) string {
	t.Helper()
	dir, err := configurator.targetDir(testTarget)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApplyDeliversWithoutClaimingActive(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	receipt, err := configurator.Apply(ctx, changeSet(t, resources))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, result := range receipt.Resources {
		if result.State != delivery.ApplyDelivered || result.DeliveredRevision == "" || result.ActiveRevision != "" {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	observation, err := configurator.Observe(ctx, testTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Connected || observation.Simulated {
		t.Fatalf("unexpected observation: %+v", observation)
	}
	for _, resource := range resources {
		revision, ok := observation.DeliveredRevision(resource.Key)
		if !ok || revision != resource.Revision {
			t.Fatalf("resource %q not delivered: %+v", resource.Key.String(), observation)
		}
	}
	changes, err := delivery.Diff(testTarget, testEpoch, config.DomainPraxis, resources, observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upserts) != 0 || len(changes.Deletes) != 0 {
		t.Fatalf("delivered config was re-sent: %+v", changes)
	}
}

func TestDuplicateApplyDoesNotRewrite(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	changes := changeSet(t, resources)
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(targetDir(t, configurator), "config.yaml")
	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	receipt, err := configurator.Apply(ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Resources) != len(resources) {
		t.Fatalf("duplicate apply results = %+v", receipt.Resources)
	}
	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || string(first) != string(second) {
		t.Fatal("duplicate apply rewrote the config file")
	}
}

func TestEndpointUpdateRewritesOnlyTheDocument(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changeSet(t, resources)); err != nil {
		t.Fatal(err)
	}
	updated := makeResources(t, testEpoch, "10.0.0.2")
	changes := config.ChangeSet{Target: testTarget, OwnerEpoch: testEpoch, Upserts: []config.Resource{updated[3]}}
	id, err := changes.StableID()
	if err != nil {
		t.Fatal(err)
	}
	changes.ID = id
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(targetDir(t, configurator), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "10.0.0.2:8080") || strings.Contains(string(data), "10.0.0.1:8080") {
		t.Fatalf("endpoint update not reflected:\n%s", data)
	}
}

func TestRenderRejectionLeavesPreviousConfig(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changeSet(t, resources)); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(targetDir(t, configurator), "config.yaml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	broken := makeResources(t, testEpoch, "10.0.0.2")
	broken[0] = makeResource(t, testEpoch, config.KindListener, "web", config.Listener{Port: 0, Protocol: config.ListenerHTTP, RouteTable: "route-web"})
	changes := config.ChangeSet{Target: testTarget, OwnerEpoch: testEpoch, Upserts: []config.Resource{broken[0]}}
	id, err := changes.StableID()
	if err != nil {
		t.Fatal(err)
	}
	changes.ID = id
	receipt, err := configurator.Apply(ctx, changes)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(receipt.Resources) != 1 || receipt.Resources[0].State != delivery.ApplyRejected {
		t.Fatalf("expected rejection, got %+v", receipt.Resources)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected apply modified the previous config")
	}
}

func TestCrashBetweenStateAndConfigIsRepaired(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	changes := changeSet(t, resources)
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(targetDir(t, configurator), "config.yaml")
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}

	observation, err := configurator.Observe(ctx, testTarget)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range observation.Resources {
		if result.State != delivery.ApplyPending {
			t.Fatalf("expected pending after crash, got %+v", result)
		}
	}
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config was not repaired: %v", err)
	}
}

func TestOwnerEpochFencing(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{OwnerStaleAfter: 40 * time.Millisecond})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changeSet(t, resources)); err != nil {
		t.Fatal(err)
	}

	if err := configurator.Claim(ctx, testTarget, "epoch-2"); err == nil {
		t.Fatal("fresh owner epoch was accepted without waiting")
	}
	time.Sleep(60 * time.Millisecond)
	if err := configurator.Claim(ctx, testTarget, "epoch-2"); err != nil {
		t.Fatalf("stale owner epoch was not retired: %v", err)
	}
	if _, err := configurator.Apply(ctx, changeSet(t, resources)); err == nil {
		t.Fatal("old owner epoch still wrote after takeover")
	}
}

func TestPartialFirstApplyIsRejected(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	changes := changeSet(t, []config.Resource{resources[3]})
	receipt, err := configurator.Apply(ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Resources) != 1 || receipt.Resources[0].State != delivery.ApplyRejected {
		t.Fatalf("expected partial first apply rejection, got %+v", receipt.Resources)
	}
	if _, err := os.Stat(filepath.Join(targetDir(t, configurator), "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("partial first apply wrote a config: %v", err)
	}
}

func TestDeletingAllResourcesRemovesTheDocument(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changeSet(t, resources)); err != nil {
		t.Fatal(err)
	}
	changes := config.ChangeSet{Target: testTarget, OwnerEpoch: testEpoch}
	for _, resource := range resources {
		changes.Deletes = append(changes.Deletes, resource.Key)
	}
	id, err := changes.StableID()
	if err != nil {
		t.Fatal(err)
	}
	changes.ID = id
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.json", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(targetDir(t, configurator), name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", name, err)
		}
	}
	observation, err := configurator.Observe(ctx, testTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Resources) != 0 || !observation.Connected {
		t.Fatalf("unexpected observation: %+v", observation)
	}
}

func TestCorruptStateSelfHeals(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	changes := changeSet(t, resources)
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir(t, configurator), "state.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	observation, err := configurator.Observe(ctx, testTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Connected || len(observation.Resources) != 0 {
		t.Fatalf("corrupt state was reported as delivered: %+v", observation)
	}
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(targetDir(t, configurator), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "10.0.0.1:8080") {
		t.Fatalf("state was not repaired:\n%s", data)
	}
}

func TestInconsistentStateSelfHeals(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	changes := changeSet(t, resources)
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	dir := targetDir(t, configurator)
	state, ok, err := loadState(dir)
	if err != nil || !ok {
		t.Fatalf("load state: %v", err)
	}
	state.Resources[0].Value = []byte(`{"port":9999}`)
	if err := writeState(dir, state); err != nil {
		t.Fatal(err)
	}
	observation, err := configurator.Observe(ctx, testTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Resources) != 0 {
		t.Fatalf("inconsistent state was reported as delivered: %+v", observation)
	}
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatalf("inconsistent state wedged the target: %v", err)
	}
}

func TestCorruptOwnerFailsClosed(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir(t, configurator), "owner.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configurator.Claim(ctx, testTarget, "epoch-2"); err == nil {
		t.Fatal("corrupt owner file granted ownership")
	}
}

func TestObserveMissingTargetIsNotConnected(t *testing.T) {
	configurator := newConfigurator(t, Options{})
	observation, err := configurator.Observe(context.Background(), testTarget)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Connected || len(observation.Resources) != 0 {
		t.Fatalf("unexpected observation: %+v", observation)
	}
}

func TestProvisionerRejectsUnsafeTarget(t *testing.T) {
	provisioner, err := NewProvisioner(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provisioner.Ensure(context.Background(), delivery.TargetIntent{
		Gateway: config.ObjectRef{Kind: "Gateway", Namespace: "default", Name: "edge", UID: "../escape"},
	})
	if err == nil {
		t.Fatal("unsafe target UID was accepted")
	}
}

func TestRestartReconstructsDeliveredState(t *testing.T) {
	ctx := context.Background()
	options := Options{Root: t.TempDir()}
	first := newConfigurator(t, options)
	if err := first.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	if _, err := first.Apply(ctx, changeSet(t, resources)); err != nil {
		t.Fatal(err)
	}
	second := newConfigurator(t, options)
	observation, err := second.Observe(ctx, testTarget)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := delivery.Diff(testTarget, testEpoch, config.DomainPraxis, resources, observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upserts) != 0 || len(changes.Deletes) != 0 {
		t.Fatalf("restart resurfaced changes: %+v", changes)
	}
}

func TestSymlinkTargetIsRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, testTarget.Namespace)); err != nil {
		t.Fatal(err)
	}
	configurator := newConfigurator(t, Options{Root: root})
	if err := configurator.Claim(context.Background(), testTarget, testEpoch); err == nil {
		t.Fatal("symlinked target component was accepted")
	}
}

func TestDeletingReferencedClusterIsRejected(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	resources := makeResources(t, testEpoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changeSet(t, resources)); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(targetDir(t, configurator), "config.yaml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	changes := config.ChangeSet{Target: testTarget, OwnerEpoch: testEpoch, Deletes: []config.Key{resources[2].Key}}
	id, err := changes.StableID()
	if err != nil {
		t.Fatal(err)
	}
	changes.ID = id
	receipt, err := configurator.Apply(ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Resources) != 1 || receipt.Resources[0].State != delivery.ApplyPending {
		t.Fatalf("expected a pending delete rejection, got %+v", receipt.Resources)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected delete changed the config")
	}
}

func TestRetiredEpochDeletesAreAccepted(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{OwnerStaleAfter: 40 * time.Millisecond})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	oldResources := makeResources(t, testEpoch, "10.0.0.1")
	if _, err := configurator.Apply(ctx, changeSet(t, oldResources)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	newEpoch := "epoch-2"
	if err := configurator.Claim(ctx, testTarget, newEpoch); err != nil {
		t.Fatal(err)
	}
	newResources := makeResources(t, newEpoch, "10.0.0.2")
	changes := config.ChangeSet{Target: testTarget, OwnerEpoch: newEpoch, Upserts: newResources}
	for _, resource := range oldResources {
		changes.Deletes = append(changes.Deletes, resource.Key)
	}
	id, err := changes.StableID()
	if err != nil {
		t.Fatal(err)
	}
	changes.ID = id
	receipt, err := configurator.Apply(ctx, changes)
	if err != nil {
		t.Fatalf("retired-epoch delete was rejected: %v", err)
	}
	for _, result := range receipt.Resources {
		if result.State == delivery.ApplyRejected {
			t.Fatalf("takeover was rejected: %+v", result)
		}
	}
}

func TestDuplicateApplyLeavesNoTemporaryFiles(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	changes := changeSet(t, makeResources(t, testEpoch, "10.0.0.1"))
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(targetDir(t, configurator))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestTruncatedConfigIsRepaired(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	changes := changeSet(t, makeResources(t, testEpoch, "10.0.0.1"))
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(targetDir(t, configurator), "config.yaml")
	if err := os.WriteFile(configPath, []byte("listeners:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observation, err := configurator.Observe(ctx, testTarget)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range observation.Resources {
		if result.State != delivery.ApplyPending {
			t.Fatalf("truncated config was reported as delivered: %+v", result)
		}
	}
	if _, err := configurator.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "10.0.0.1:8080") {
		t.Fatalf("truncated config was not repaired:\n%s", data)
	}
}

func TestPermissions(t *testing.T) {
	ctx := context.Background()
	configurator := newConfigurator(t, Options{})
	if err := configurator.Claim(ctx, testTarget, testEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := configurator.Apply(ctx, changeSet(t, makeResources(t, testEpoch, "10.0.0.1"))); err != nil {
		t.Fatal(err)
	}
	dir := targetDir(t, configurator)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("target directory permissions = %o", info.Mode().Perm())
	}
	for _, name := range []string{"owner.json", "state.json", "config.yaml"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o", name, info.Mode().Perm())
		}
	}
}

func TestProvisioner(t *testing.T) {
	ctx := context.Background()
	options := Options{Root: t.TempDir(), Addresses: []string{"192.0.2.10"}}
	provisioner, err := NewProvisioner(options)
	if err != nil {
		t.Fatal(err)
	}
	intent := delivery.TargetIntent{
		Gateway:   config.ObjectRef{Kind: "Gateway", Namespace: testTarget.Namespace, Name: testTarget.Name, UID: testTarget.UID},
		Listeners: []delivery.ListenerIntent{{Name: "web", Port: 80, Protocol: "HTTP"}},
	}
	state, err := provisioner.Ensure(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if state.Target != testTarget || !state.Ready || len(state.Addresses) != 1 || state.BootstrapRevision == "" {
		t.Fatalf("unexpected target state: %+v", state)
	}
	if err := provisioner.Release(ctx, testTarget); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Release(ctx, testTarget); err != nil {
		t.Fatalf("release is not idempotent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(options.Root, testTarget.Namespace, testTarget.Name, testTarget.UID)); !os.IsNotExist(err) {
		t.Fatalf("target directory still exists: %v", err)
	}
}
