package mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	operationAdd    = "add"
	operationUpdate = "update"
	operationDelete = "delete"
	mockNACKReason  = "simulated mock rejection"
)

// JournalEntry contains only resource identity and revisions, never resource values.
type JournalEntry struct {
	Sequence    uint64     `json:"sequence"`
	Operation   string     `json:"operation"`
	Key         config.Key `json:"key"`
	OldRevision string     `json:"oldRevision,omitempty"`
	NewRevision string     `json:"newRevision,omitempty"`
	Simulated   bool       `json:"simulated"`
}

type storedResource struct {
	revision     string
	dependencies []config.Key
	secret       *config.SecretHandle
}

type pendingChange struct {
	revision  string
	stored    storedResource
	remaining int
}

type faultKind uint8

const (
	faultReject faultKind = iota + 1
	faultDelay
)

type injectedFault struct {
	kind      faultKind
	remaining int
}

type targetState struct {
	mu              sync.Mutex
	connected       bool
	ownerEpoch      string
	retiredEpochs   map[string]struct{}
	resources       map[config.Key]storedResource
	rejected        map[config.Key]delivery.ResourceResult
	pending         map[config.Key]pendingChange
	faults          map[config.Key][]injectedFault
	journalSequence uint64
}

// Configurator is an in-memory, explicitly simulated delivery adapter.
type Configurator struct {
	targetsMu sync.Mutex
	targets   map[config.Target]*targetState

	journalMu sync.Mutex
	journal   []JournalEntry
}

var _ delivery.Configurator = (*Configurator)(nil)
var _ delivery.Provisioner = (*Provisioner)(nil)

func NewConfigurator() *Configurator {
	return &Configurator{targets: make(map[config.Target]*targetState)}
}

func (c *Configurator) Capabilities(ctx context.Context, target config.Target) (delivery.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return delivery.Capabilities{}, err
	}
	if err := validateTarget(target); err != nil {
		return delivery.Capabilities{}, err
	}
	return delivery.Capabilities{
		ResourceKinds: []config.Kind{
			config.KindListener,
			config.KindHTTPRoute,
			config.KindCluster,
			config.KindEndpointSet,
			config.KindSecretHandle,
			config.KindOpaqueRoot,
		},
		IndependentEndpointSets:     true,
		AtomicRelatedResourceGroups: false,
		Simulated:                   true,
	}, nil
}

// Claim fences prior writers for target and makes ownerEpoch the only accepted epoch.
func (c *Configurator) Claim(ctx context.Context, target config.Target, ownerEpoch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.setCurrentOwnerEpoch(ctx, target, ownerEpoch)
}

func (c *Configurator) Apply(ctx context.Context, changes config.ChangeSet) (delivery.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return delivery.Receipt{}, err
	}
	if err := changes.Validate(); err != nil {
		return delivery.Receipt{}, fmt.Errorf("validate mock change set: %w", err)
	}
	if err := validateSupported(changes); err != nil {
		return delivery.Receipt{}, err
	}
	id, err := changes.StableID()
	if err != nil {
		return delivery.Receipt{}, fmt.Errorf("identify mock change set: %w", err)
	}
	if changes.ID != "" && changes.ID != id {
		return delivery.Receipt{}, errors.New("change set ID does not match its contents")
	}
	if changes.ID != "" {
		id = changes.ID
	}

	state := c.stateFor(changes.Target)
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return delivery.Receipt{}, err
	}
	if !state.connected {
		return delivery.Receipt{}, errors.New("mock target is disconnected")
	}
	if state.ownerEpoch != "" && state.ownerEpoch != changes.OwnerEpoch {
		return delivery.Receipt{}, errors.New("mock target owner epoch is fenced")
	}
	for _, key := range changes.Deletes {
		if key.OwnerEpoch == changes.OwnerEpoch {
			continue
		}
		if _, retired := state.retiredEpochs[key.OwnerEpoch]; !retired {
			return delivery.Receipt{}, errors.New("mock delete key owner epoch has not been fenced")
		}
	}
	if state.ownerEpoch == "" {
		state.ownerEpoch = changes.OwnerEpoch
	}

	receipt := delivery.Receipt{Target: changes.Target, ChangeID: id, Simulated: true}
	upserts := append([]config.Resource(nil), changes.Upserts...)
	sort.Slice(upserts, func(i, j int) bool {
		ri, rj := upsertRank(upserts[i].Key.Kind), upsertRank(upserts[j].Key.Kind)
		if ri != rj {
			return ri < rj
		}
		return upserts[i].Key.String() < upserts[j].Key.String()
	})

	for _, resource := range upserts {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		key := resource.Key
		current, active := state.resources[key]
		if active && current.revision == resource.Revision {
			delete(state.pending, key)
			delete(state.rejected, key)
			receipt.Resources = append(receipt.Resources, activeResult(key, resource.Revision))
			continue
		}
		if pending, ok := state.pending[key]; ok && pending.revision == resource.Revision {
			receipt.Resources = append(receipt.Resources, pendingResult(key, resource.Revision, current, active))
			continue
		}
		delete(state.pending, key)

		fault, injected := state.takeFault(key)
		if injected && fault.kind == faultReject {
			result := rejectedResult(key, resource.Revision, current, active, mockNACKReason)
			state.rejected[key] = result
			receipt.Resources = append(receipt.Resources, result)
			continue
		}

		stored, dependencyErr := resourceState(resource, state.resources)
		if dependencyErr != nil {
			result := rejectedResult(key, resource.Revision, current, active, dependencyErr.Error())
			state.rejected[key] = result
			receipt.Resources = append(receipt.Resources, result)
			continue
		}

		// A delayed update stays pending while its previous active revision remains observable.
		if injected && fault.kind == faultDelay {
			state.pending[key] = pendingChange{revision: resource.Revision, stored: stored, remaining: fault.remaining}
			delete(state.rejected, key)
			receipt.Resources = append(receipt.Resources, pendingResult(key, resource.Revision, current, active))
			continue
		}

		operation := operationAdd
		oldRevision := ""
		if active {
			operation, oldRevision = operationUpdate, current.revision
		}
		state.resources[key] = stored
		delete(state.rejected, key)
		c.record(ctx, state, operation, key, oldRevision, resource.Revision)
		receipt.Resources = append(receipt.Resources, activeResult(key, resource.Revision))
	}

	deletes := append([]config.Key(nil), changes.Deletes...)
	sort.Slice(deletes, func(i, j int) bool {
		ri, rj := deleteRank(deletes[i].Kind), deleteRank(deletes[j].Kind)
		if ri != rj {
			return ri > rj
		}
		return deletes[i].String() < deletes[j].String()
	})
	for _, key := range deletes {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		current, active := state.resources[key]
		if !active {
			delete(state.pending, key)
			delete(state.rejected, key)
			receipt.Resources = append(receipt.Resources, delivery.ResourceResult{Key: key, State: delivery.ApplyDeleted})
			continue
		}
		if fault, ok := state.takeFault(key); ok && fault.kind == faultReject {
			result := rejectedResult(key, "", current, true, mockNACKReason)
			state.rejected[key] = result
			receipt.Resources = append(receipt.Resources, result)
			continue
		}
		if dependent, found := state.referenceTo(key); found {
			result := rejectedResult(key, "", current, true, fmt.Sprintf("resource is still referenced by %s", dependent.String()))
			state.rejected[key] = result
			receipt.Resources = append(receipt.Resources, result)
			continue
		}
		delete(state.resources, key)
		delete(state.pending, key)
		delete(state.rejected, key)
		c.record(ctx, state, operationDelete, key, current.revision, "")
		receipt.Resources = append(receipt.Resources, delivery.ResourceResult{Key: key, State: delivery.ApplyDeleted})
	}
	return receipt, nil
}

func (c *Configurator) Observe(ctx context.Context, target config.Target) (delivery.Observation, error) {
	if err := ctx.Err(); err != nil {
		return delivery.Observation{}, err
	}
	if err := validateTarget(target); err != nil {
		return delivery.Observation{}, err
	}
	state := c.stateFor(target)
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return delivery.Observation{}, err
	}
	if state.connected {
		if err := c.advancePending(ctx, state); err != nil {
			return delivery.Observation{}, err
		}
	}

	observation := delivery.Observation{
		Target:       target,
		Connected:    state.connected,
		Simulated:    true,
		ProcessEpoch: "mock-simulated",
	}
	keys := make([]config.Key, 0, len(state.resources))
	for key := range state.resources {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, key := range keys {
		observation.Resources = append(observation.Resources, activeResult(key, state.resources[key].revision))
	}
	keys = keys[:0]
	for key := range state.rejected {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, key := range keys {
		observation.Rejected = append(observation.Rejected, state.rejected[key])
	}
	return observation, nil
}

// RejectNext NACKs the next non-idempotent operation for key without changing active state.
func (c *Configurator) RejectNext(target config.Target, key config.Key) error {
	return c.inject(target, key, injectedFault{kind: faultReject})
}

// DelayNext leaves the next operation pending for the given number of observations.
func (c *Configurator) DelayNext(target config.Target, key config.Key, observeCalls int) error {
	if observeCalls < 1 {
		return errors.New("observation delay must be at least one")
	}
	return c.inject(target, key, injectedFault{kind: faultDelay, remaining: observeCalls})
}

func (c *Configurator) Disconnect(target config.Target) {
	state := c.stateFor(target)
	state.mu.Lock()
	state.connected = false
	state.mu.Unlock()
}

func (c *Configurator) Reconnect(target config.Target) {
	state := c.stateFor(target)
	state.mu.Lock()
	state.connected = true
	state.mu.Unlock()
}

func (c *Configurator) setCurrentOwnerEpoch(ctx context.Context, target config.Target, ownerEpoch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTarget(target); err != nil {
		return err
	}
	if ownerEpoch == "" {
		return errors.New("owner epoch is required")
	}
	state := c.stateFor(target)
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.ownerEpoch == ownerEpoch {
		return nil
	}
	if _, retired := state.retiredEpochs[ownerEpoch]; retired {
		return errors.New("owner epoch was already fenced")
	}
	if state.ownerEpoch != "" {
		if state.retiredEpochs == nil {
			state.retiredEpochs = make(map[string]struct{})
		}
		state.retiredEpochs[state.ownerEpoch] = struct{}{}
	}
	state.ownerEpoch = ownerEpoch
	for key := range state.pending {
		if key.OwnerEpoch != ownerEpoch {
			delete(state.pending, key)
		}
	}
	for key := range state.rejected {
		if key.OwnerEpoch != ownerEpoch {
			delete(state.rejected, key)
		}
	}
	for key := range state.faults {
		if key.OwnerEpoch != ownerEpoch {
			delete(state.faults, key)
		}
	}
	return nil
}

// SeedAI and SeedPolicy add opaque producer-owned roots without journaling them.
func (c *Configurator) SeedAI(target config.Target, ownerEpoch, name, revision string) error {
	return c.seedRoot(target, ownerEpoch, config.DomainAI, name, revision)
}

func (c *Configurator) SeedPolicy(target config.Target, ownerEpoch, name, revision string) error {
	return c.seedRoot(target, ownerEpoch, config.DomainPolicy, name, revision)
}

// Journal returns a stable copy ordered by target and each target's apply sequence.
func (c *Configurator) Journal() []JournalEntry {
	c.journalMu.Lock()
	defer c.journalMu.Unlock()
	entries := append([]JournalEntry(nil), c.journal...)
	sort.Slice(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.Key.Target.ID() != right.Key.Target.ID() {
			return left.Key.Target.ID() < right.Key.Target.ID()
		}
		return left.Sequence < right.Sequence
	})
	return entries
}

func (c *Configurator) advancePending(ctx context.Context, state *targetState) error {
	keys := make([]config.Key, 0, len(state.pending))
	for key := range state.pending {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := upsertRank(keys[i].Kind), upsertRank(keys[j].Kind)
		if ri != rj {
			return ri < rj
		}
		return keys[i].String() < keys[j].String()
	})
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		pending, ok := state.pending[key]
		if !ok {
			continue
		}
		if pending.remaining > 0 {
			pending.remaining--
			state.pending[key] = pending
			continue
		}
		if dependencyErr := dependenciesActive(pending.stored.dependencies, state.resources); dependencyErr != nil {
			current, active := state.resources[key]
			state.rejected[key] = rejectedResult(key, pending.revision, current, active, dependencyErr.Error())
			delete(state.pending, key)
			continue
		}
		current, active := state.resources[key]
		operation, oldRevision := operationAdd, ""
		if active {
			operation, oldRevision = operationUpdate, current.revision
		}
		state.resources[key] = pending.stored
		delete(state.pending, key)
		delete(state.rejected, key)
		c.record(ctx, state, operation, key, oldRevision, pending.revision)
	}
	return nil
}

func (c *Configurator) record(ctx context.Context, state *targetState, operation string, key config.Key, oldRevision, newRevision string) {
	state.journalSequence++
	entry := JournalEntry{
		Sequence:    state.journalSequence,
		Operation:   operation,
		Key:         key,
		OldRevision: oldRevision,
		NewRevision: newRevision,
		Simulated:   true,
	}
	c.journalMu.Lock()
	c.journal = append(c.journal, entry)
	c.journalMu.Unlock()
	ctrl.LoggerFrom(ctx).WithValues(
		"operation", operation,
		"resourceKind", key.Kind,
		"key", key.String(),
		"oldRevision", oldRevision,
		"newRevision", newRevision,
		"simulated", true,
	).Info("Mock resource applied")
}

func (c *Configurator) seedRoot(target config.Target, ownerEpoch string, domain config.Domain, name, revision string) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	if ownerEpoch == "" || name == "" || revision == "" {
		return errors.New("seed owner epoch, name and revision are required")
	}
	key := config.Key{Domain: domain, Target: target, OwnerEpoch: ownerEpoch, Kind: config.KindOpaqueRoot, Name: name}
	resource, err := config.NewResource(key, config.ObjectRef{}, config.OpaqueRoot{Revision: revision})
	if err != nil {
		return err
	}
	state := c.stateFor(target)
	state.mu.Lock()
	defer state.mu.Unlock()
	if current, exists := state.resources[key]; exists {
		if current.revision == resource.Revision {
			return nil
		}
		return fmt.Errorf("root %q is already seeded with another revision", key.String())
	}
	state.resources[key] = storedResource{revision: resource.Revision}
	return nil
}

func (c *Configurator) inject(target config.Target, key config.Key, fault injectedFault) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	if key.Target != target || key.OwnerEpoch == "" || key.Domain == "" || key.Kind == "" || key.Name == "" {
		return errors.New("fault key must belong to its target and include a domain, epoch, kind and name")
	}
	state := c.stateFor(target)
	state.mu.Lock()
	state.faults[key] = append(state.faults[key], fault)
	state.mu.Unlock()
	return nil
}

func (s *targetState) takeFault(key config.Key) (injectedFault, bool) {
	queue := s.faults[key]
	if len(queue) == 0 {
		return injectedFault{}, false
	}
	fault := queue[0]
	if len(queue) == 1 {
		delete(s.faults, key)
	} else {
		s.faults[key] = queue[1:]
	}
	return fault, true
}

func (c *Configurator) stateFor(target config.Target) *targetState {
	c.targetsMu.Lock()
	defer c.targetsMu.Unlock()
	if c.targets == nil {
		c.targets = make(map[config.Target]*targetState)
	}
	state := c.targets[target]
	if state == nil {
		state = &targetState{
			connected:     true,
			retiredEpochs: make(map[string]struct{}),
			resources:     make(map[config.Key]storedResource),
			rejected:      make(map[config.Key]delivery.ResourceResult),
			pending:       make(map[config.Key]pendingChange),
			faults:        make(map[config.Key][]injectedFault),
		}
		c.targets[target] = state
	}
	return state
}

func resourceState(resource config.Resource, active map[config.Key]storedResource) (storedResource, error) {
	var dependencies []config.Key
	var secret *config.SecretHandle
	switch value := resource.Value.(type) {
	case config.Cluster:
		if value.EndpointSet == "" {
			return storedResource{}, errors.New("cluster endpoint set is required")
		}
		dependencies = append(dependencies, dependencyKey(resource.Key, config.KindEndpointSet, value.EndpointSet))
	case config.HTTPRouteTable:
		for _, rule := range value.Rules {
			for _, backend := range rule.Action.Backends {
				if backend.Valid && backend.Cluster != "" {
					dependencies = append(dependencies, dependencyKey(resource.Key, config.KindCluster, backend.Cluster))
				}
			}
		}
	case config.Listener:
		if value.RouteTable != "" {
			dependencies = append(dependencies, dependencyKey(resource.Key, config.KindHTTPRoute, value.RouteTable))
		}
		for _, ref := range value.Secrets {
			key, found := secretHandleKey(resource.Key, active, ref)
			if !found {
				return storedResource{}, fmt.Errorf("listener secret handle %s/%s is not active", ref.Namespace, ref.Name)
			}
			dependencies = append(dependencies, key)
		}
	case config.SecretHandle:
		secret = &config.SecretHandle{Namespace: value.Namespace, Name: value.Name}
	}
	if err := dependenciesActive(dependencies, active); err != nil {
		return storedResource{}, err
	}
	return storedResource{revision: resource.Revision, dependencies: dependencies, secret: secret}, nil
}

func dependencyKey(key config.Key, kind config.Kind, name string) config.Key {
	key.Kind, key.Name = kind, name
	return key
}

func secretHandleKey(owner config.Key, active map[config.Key]storedResource, ref config.SecretRef) (config.Key, bool) {
	var match config.Key
	for key, resource := range active {
		if key.Domain != owner.Domain || key.Target != owner.Target || key.OwnerEpoch != owner.OwnerEpoch || key.Kind != config.KindSecretHandle || resource.secret == nil {
			continue
		}
		if resource.secret.Namespace != ref.Namespace || resource.secret.Name != ref.Name {
			continue
		}
		if match != (config.Key{}) {
			return config.Key{}, false
		}
		match = key
	}
	return match, match != (config.Key{})
}

func dependenciesActive(dependencies []config.Key, active map[config.Key]storedResource) error {
	for _, dependency := range dependencies {
		if _, exists := active[dependency]; !exists {
			return fmt.Errorf("dependency %s/%q is not active", dependency.Kind, dependency.Name)
		}
	}
	return nil
}

func (s *targetState) referenceTo(key config.Key) (config.Key, bool) {
	references := s.referencesTo(key)
	if len(references) == 0 {
		return config.Key{}, false
	}
	return references[0], true
}

func (s *targetState) referencesTo(key config.Key) []config.Key {
	var references []config.Key
	for dependent, resource := range s.resources {
		for _, dependency := range resource.dependencies {
			if dependency == key {
				references = append(references, dependent)
				break
			}
		}
	}
	for dependent, pending := range s.pending {
		for _, dependency := range pending.stored.dependencies {
			if dependency == key {
				references = append(references, dependent)
				break
			}
		}
	}
	return references
}

func upsertRank(kind config.Kind) int {
	switch kind {
	case config.KindEndpointSet, config.KindOpaqueRoot:
		return 0
	case config.KindSecretHandle, config.KindCluster:
		return 1
	case config.KindHTTPRoute:
		return 2
	case config.KindListener:
		return 3
	default:
		return 0
	}
}

func deleteRank(kind config.Kind) int {
	switch kind {
	case config.KindListener:
		return 4
	case config.KindHTTPRoute:
		return 3
	case config.KindCluster, config.KindSecretHandle:
		return 2
	case config.KindEndpointSet:
		return 1
	default:
		return 0
	}
}

func activeResult(key config.Key, revision string) delivery.ResourceResult {
	return delivery.ResourceResult{Key: key, DesiredRevision: revision, ActiveRevision: revision, State: delivery.ApplyActive}
}

func pendingResult(key config.Key, revision string, current storedResource, active bool) delivery.ResourceResult {
	result := delivery.ResourceResult{Key: key, DesiredRevision: revision, State: delivery.ApplyPending}
	if active {
		result.ActiveRevision = current.revision
	}
	return result
}

func rejectedResult(key config.Key, revision string, current storedResource, active bool, reason string) delivery.ResourceResult {
	result := delivery.ResourceResult{Key: key, DesiredRevision: revision, State: delivery.ApplyRejected, Reason: reason}
	if active {
		result.ActiveRevision = current.revision
	}
	return result
}

func validateTarget(target config.Target) error {
	if target.Namespace == "" || target.Name == "" || target.UID == "" {
		return errors.New("target namespace, name and UID are required")
	}
	return nil
}

func validateSupported(changes config.ChangeSet) error {
	validate := func(key config.Key) error {
		switch key.Domain {
		case config.DomainPraxis, config.DomainAI, config.DomainPolicy:
		default:
			return fmt.Errorf("unsupported mock domain %q", key.Domain)
		}
		switch key.Kind {
		case config.KindListener, config.KindHTTPRoute, config.KindCluster, config.KindEndpointSet, config.KindSecretHandle, config.KindOpaqueRoot:
		default:
			return fmt.Errorf("unsupported mock resource kind %q", key.Kind)
		}
		if key.Domain != config.DomainPraxis && key.Kind != config.KindOpaqueRoot {
			return fmt.Errorf("mock %s domain accepts only opaque roots", key.Domain)
		}
		return nil
	}
	for _, resource := range changes.Upserts {
		if err := validate(resource.Key); err != nil {
			return err
		}
	}
	for _, key := range changes.Deletes {
		if err := validate(key); err != nil {
			return err
		}
	}
	return nil
}

// Provisioner returns a ready, simulated target for each Gateway identity.
type Provisioner struct{}

func NewProvisioner() *Provisioner {
	return &Provisioner{}
}

func (p *Provisioner) Ensure(ctx context.Context, intent delivery.TargetIntent) (delivery.TargetState, error) {
	if err := ctx.Err(); err != nil {
		return delivery.TargetState{}, err
	}
	ref := intent.Gateway
	if ref.Kind != "Gateway" || ref.Namespace == "" || ref.Name == "" || ref.UID == "" {
		return delivery.TargetState{}, errors.New("target intent requires a Gateway kind, namespace, name and UID")
	}
	target := config.Target{Namespace: ref.Namespace, Name: ref.Name, UID: ref.UID}
	listeners := append([]delivery.ListenerIntent(nil), intent.Listeners...)
	sort.Slice(listeners, func(i, j int) bool {
		if listeners[i].Name != listeners[j].Name {
			return listeners[i].Name < listeners[j].Name
		}
		if listeners[i].Port != listeners[j].Port {
			return listeners[i].Port < listeners[j].Port
		}
		return listeners[i].Protocol < listeners[j].Protocol
	})
	encoded, err := json.Marshal(listeners)
	if err != nil {
		return delivery.TargetState{}, fmt.Errorf("identify mock bootstrap: %w", err)
	}
	sum := sha256.Sum256(encoded)
	state := delivery.TargetState{
		Target:            target,
		Ready:             true,
		BootstrapRevision: hex.EncodeToString(sum[:]),
		Simulated:         true,
	}
	return state, nil
}

func (p *Provisioner) Release(ctx context.Context, target config.Target) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTarget(target); err != nil {
		return err
	}
	return nil
}
