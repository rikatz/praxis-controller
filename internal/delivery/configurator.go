package delivery

import (
	"context"
	"fmt"
	"sort"

	"github.com/praxis-proxy/controller/internal/config"
)

type ApplyState string

const (
	ApplyActive    ApplyState = "active"
	ApplyDelivered ApplyState = "delivered"
	ApplyPending   ApplyState = "pending"
	ApplyRejected  ApplyState = "rejected"
	ApplyDeleted   ApplyState = "deleted"
)

type ResourceResult struct {
	Key               config.Key `json:"key"`
	DesiredRevision   string     `json:"desiredRevision,omitempty"`
	DeliveredRevision string     `json:"deliveredRevision,omitempty"`
	ActiveRevision    string     `json:"activeRevision,omitempty"`
	State             ApplyState `json:"state"`
	Reason            string     `json:"reason,omitempty"`
}

// Committed reports whether the adapter has durably taken this revision.
// Active is a stronger claim than Delivered; only Active satisfies Programmed.
func Committed(result ResourceResult, revision string) bool {
	if revision == "" {
		return false
	}
	switch result.State {
	case ApplyActive:
		return result.ActiveRevision == revision
	case ApplyDelivered:
		return result.DeliveredRevision == revision
	default:
		return false
	}
}

type Receipt struct {
	Target    config.Target    `json:"target"`
	ChangeID  string           `json:"changeID"`
	Simulated bool             `json:"simulated"`
	Resources []ResourceResult `json:"resources,omitempty"`
}

type Capabilities struct {
	ResourceKinds               []config.Kind `json:"resourceKinds,omitempty"`
	IndependentEndpointSets     bool          `json:"independentEndpointSets"`
	AtomicRelatedResourceGroups bool          `json:"atomicRelatedResourceGroups"`
	Simulated                   bool          `json:"simulated"`
}

type Observation struct {
	Target       config.Target    `json:"target"`
	Connected    bool             `json:"connected"`
	Simulated    bool             `json:"simulated"`
	ProcessEpoch string           `json:"processEpoch,omitempty"`
	Resources    []ResourceResult `json:"resources,omitempty"`
	Rejected     []ResourceResult `json:"rejected,omitempty"`
}

type Configurator interface {
	Capabilities(context.Context, config.Target) (Capabilities, error)
	// Claim is idempotent for the current owner and fences previous owners before
	// reconciliation. It preserves inventory so the owner can retire stale keys
	// after compiling a complete desired snapshot for the target.
	Claim(context.Context, config.Target, string) error
	Apply(context.Context, config.ChangeSet) (Receipt, error)
	Observe(context.Context, config.Target) (Observation, error)
}

type TargetIntent struct {
	Gateway   config.ObjectRef `json:"gateway"`
	Listeners []ListenerIntent `json:"listeners,omitempty"`
}

type ListenerIntent struct {
	Name     string `json:"name"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"`
}

type TargetState struct {
	Target            config.Target `json:"target"`
	Addresses         []string      `json:"addresses,omitempty"`
	Ready             bool          `json:"ready"`
	BootstrapRevision string        `json:"bootstrapRevision,omitempty"`
	Simulated         bool          `json:"simulated"`
}

type Provisioner interface {
	Ensure(context.Context, TargetIntent) (TargetState, error)
	Release(context.Context, config.Target) error
}

func (r Receipt) ActiveRevision(key config.Key) (string, bool) {
	for _, result := range r.Resources {
		if result.Key == key && result.State == ApplyActive && result.DesiredRevision != "" && result.ActiveRevision == result.DesiredRevision {
			return result.ActiveRevision, true
		}
	}
	return "", false
}

func (o Observation) ActiveRevision(key config.Key) (string, bool) {
	for _, result := range o.Resources {
		if result.Key == key && result.State == ApplyActive && result.ActiveRevision != "" {
			return result.ActiveRevision, true
		}
	}
	return "", false
}

func (r Receipt) DeliveredRevision(key config.Key) (string, bool) {
	for _, result := range r.Resources {
		if result.Key == key && result.State == ApplyDelivered && result.DeliveredRevision != "" {
			return result.DeliveredRevision, true
		}
	}
	return "", false
}

func (o Observation) DeliveredRevision(key config.Key) (string, bool) {
	for _, result := range o.Resources {
		if result.Key == key && result.State == ApplyDelivered && result.DeliveredRevision != "" {
			return result.DeliveredRevision, true
		}
	}
	return "", false
}

// Diff returns only the changed resources in one configuration domain. This
// keeps endpoint churn from causing unrelated roots or resources to be sent.
func Diff(target config.Target, ownerEpoch string, domain config.Domain, desired []config.Resource, observed Observation) (config.ChangeSet, error) {
	if target.Namespace == "" || target.Name == "" || target.UID == "" || ownerEpoch == "" || domain == "" {
		return config.ChangeSet{}, fmt.Errorf("target, owner epoch and configuration domain are required")
	}
	if observed.Target != (config.Target{}) && observed.Target != target {
		return config.ChangeSet{}, fmt.Errorf("observation target %q does not match desired target %q", observed.Target.ID(), target.ID())
	}
	if err := (config.ChangeSet{Target: target, OwnerEpoch: ownerEpoch, Upserts: desired}).Validate(); err != nil {
		return config.ChangeSet{}, fmt.Errorf("validate desired resources: %w", err)
	}
	wanted := make(map[config.Key]config.Resource, len(desired))
	for _, resource := range desired {
		if resource.Key.Domain != domain || resource.Key.Target != target || resource.Key.OwnerEpoch != ownerEpoch {
			return config.ChangeSet{}, fmt.Errorf("desired resource %q is outside requested domain, target or owner epoch", resource.Key.String())
		}
		if _, exists := wanted[resource.Key]; exists {
			return config.ChangeSet{}, fmt.Errorf("duplicate desired resource %q", resource.Key.String())
		}
		wanted[resource.Key] = resource
	}
	current := make(map[config.Key]ResourceResult, len(observed.Resources))
	for _, resource := range observed.Resources {
		if resource.Key.Domain != domain || resource.Key.Target != target || resource.Key.OwnerEpoch == "" || resource.State == ApplyDeleted {
			continue
		}
		if _, exists := current[resource.Key]; exists {
			return config.ChangeSet{}, fmt.Errorf("observation contains duplicate resource %q", resource.Key.String())
		}
		current[resource.Key] = resource
	}
	changes := config.ChangeSet{Target: target, OwnerEpoch: ownerEpoch}
	for key, resource := range wanted {
		active, exists := current[key]
		if !exists || !Committed(active, resource.Revision) {
			changes.Upserts = append(changes.Upserts, resource)
		}
	}
	for key := range current {
		if _, exists := wanted[key]; !exists {
			changes.Deletes = append(changes.Deletes, key)
		}
	}
	sort.Slice(changes.Upserts, func(i, j int) bool { return changes.Upserts[i].Key.String() < changes.Upserts[j].Key.String() })
	sort.Slice(changes.Deletes, func(i, j int) bool { return changes.Deletes[i].String() < changes.Deletes[j].String() })
	if len(changes.Upserts) == 0 && len(changes.Deletes) == 0 {
		return changes, nil
	}
	id, err := changes.StableID()
	if err != nil {
		return config.ChangeSet{}, err
	}
	changes.ID = id
	if err := changes.Validate(); err != nil {
		return config.ChangeSet{}, err
	}
	return changes, nil
}

func ValidateReceipt(changes config.ChangeSet, receipt Receipt) error {
	if receipt.Target != changes.Target {
		return fmt.Errorf("receipt target %q does not match change set target %q", receipt.Target.ID(), changes.Target.ID())
	}
	if receipt.ChangeID != changes.ID {
		return fmt.Errorf("receipt change ID %q does not match change set ID %q", receipt.ChangeID, changes.ID)
	}
	return nil
}
