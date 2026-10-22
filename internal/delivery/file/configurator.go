// Package file delivers configuration to a Praxis instance through an
// atomically replaced YAML file. It proves delivery, never runtime application.
package file

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	"github.com/praxis-proxy/controller/internal/praxis"
)

type Options struct {
	Root                  string
	TLSRoot               string
	ListenerBindAddress   string
	AllowPrivateEndpoints bool
	OwnerStaleAfter       time.Duration
	Addresses             []string
}

type Configurator struct {
	options Options
	mu      sync.Mutex
}

var _ delivery.Configurator = (*Configurator)(nil)

func NewConfigurator(options Options) (*Configurator, error) {
	if options.Root == "" {
		return nil, errors.New("praxis config root is required")
	}
	if options.OwnerStaleAfter <= 0 {
		options.OwnerStaleAfter = time.Minute
	}
	if err := ensureDir(options.Root); err != nil {
		return nil, fmt.Errorf("create praxis config root: %w", err)
	}
	return &Configurator{options: options}, nil
}

func (c *Configurator) Capabilities(ctx context.Context, target config.Target) (delivery.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return delivery.Capabilities{}, err
	}
	if _, err := c.targetDir(target); err != nil {
		return delivery.Capabilities{}, err
	}
	return delivery.Capabilities{
		ResourceKinds: []config.Kind{
			config.KindListener,
			config.KindHTTPRoute,
			config.KindCluster,
			config.KindEndpointSet,
			config.KindSecretHandle,
		},
		IndependentEndpointSets:     false,
		AtomicRelatedResourceGroups: true,
		Simulated:                   false,
	}, nil
}

// Claim fences other writers by owner epoch. The epoch is refreshed at most
// once per half of OwnerStaleAfter so periodic reconciliation does not write
// on every pass.
func (c *Configurator) Claim(ctx context.Context, target config.Target, ownerEpoch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerEpoch == "" {
		return errors.New("owner epoch is required")
	}
	dir, err := c.targetDir(target)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ensureDir(dir); err != nil {
		return err
	}
	owner, exists, err := loadOwner(dir)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	switch {
	case !exists || owner.Epoch == "":
		owner.Epoch = ownerEpoch
	case owner.Epoch == ownerEpoch:
		if now.Sub(owner.UpdatedAt) < c.options.OwnerStaleAfter/2 {
			return nil
		}
	case now.Sub(owner.UpdatedAt) < c.options.OwnerStaleAfter:
		return errors.New("praxis config target is owned by an active writer")
	default:
		owner.RetiredEpochs = append(owner.RetiredEpochs, owner.Epoch)
		sort.Strings(owner.RetiredEpochs)
		owner.Epoch = ownerEpoch
	}
	owner.RetiredEpochs = boundedEpochs(owner.RetiredEpochs)
	owner.UpdatedAt = now
	return writeJSON(filepath.Join(dir, "owner.json"), owner)
}

// boundedEpochs deduplicates and caps the retired epoch history so repeated
// failovers cannot grow owner.json without limit.
func boundedEpochs(epochs []string) []string {
	const limit = 16
	seen := make(map[string]struct{}, len(epochs))
	unique := make([]string, 0, len(epochs))
	for _, epoch := range epochs {
		if _, exists := seen[epoch]; exists {
			continue
		}
		seen[epoch] = struct{}{}
		unique = append(unique, epoch)
	}
	sort.Strings(unique)
	if len(unique) > limit {
		unique = unique[len(unique)-limit:]
	}
	return unique
}

func (c *Configurator) Apply(ctx context.Context, changes config.ChangeSet) (delivery.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return delivery.Receipt{}, err
	}
	if err := changes.Validate(); err != nil {
		return delivery.Receipt{}, fmt.Errorf("validate change set: %w", err)
	}
	id, err := changes.StableID()
	if err != nil {
		return delivery.Receipt{}, err
	}
	if changes.ID != "" && changes.ID != id {
		return delivery.Receipt{}, errors.New("change set ID does not match its contents")
	}
	dir, err := c.targetDir(changes.Target)
	if err != nil {
		return delivery.Receipt{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	owner, exists, err := loadOwner(dir)
	if err != nil {
		return delivery.Receipt{}, err
	}
	if !exists || owner.Epoch != changes.OwnerEpoch {
		return delivery.Receipt{}, errors.New("praxis config target owner epoch is fenced")
	}

	state, ok, err := loadState(dir)
	if err != nil {
		return delivery.Receipt{}, err
	}
	stored := make(map[string]storedResource)
	if ok {
		for _, resource := range state.Resources {
			stored[resource.Key.String()] = resource
		}
	}

	for _, key := range changes.Deletes {
		if !owner.retired(key.OwnerEpoch) && key.OwnerEpoch != changes.OwnerEpoch {
			return delivery.Receipt{}, errors.New("delete key owner epoch has not been fenced")
		}
		delete(stored, key.String())
	}
	for _, resource := range changes.Upserts {
		entry, err := toStored(resource)
		if err != nil {
			return delivery.Receipt{}, err
		}
		stored[resource.Key.String()] = entry
	}

	if !ok && !hasListener(stored) {
		// A partial first apply (for example an EndpointSlice-only update)
		// must not replace the document before the Gateway has been compiled.
		return rejectedReceipt(changes, id, "initial target state requires a listener"), nil
	}
	if len(stored) == 0 {
		// Nothing desired remains: remove the document rather than writing an
		// empty configuration Praxis may reject.
		for _, name := range []string{"state.json", "config.yaml"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				return delivery.Receipt{}, err
			}
		}
		return deliveredReceipt(changes, id), nil
	}

	resources := make([]config.Resource, 0, len(stored))
	for _, entry := range stored {
		resource, err := fromStored(entry)
		if err != nil {
			return delivery.Receipt{}, err
		}
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Key.String() < resources[j].Key.String() })

	configBytes, renderErr := praxis.Render(resources, praxis.Options{
		ListenerBindAddress:   c.options.ListenerBindAddress,
		TLSRoot:               c.options.TLSRoot,
		AllowPrivateEndpoints: c.options.AllowPrivateEndpoints,
	})
	if renderErr != nil {
		return rejectedReceipt(changes, id, sanitize(renderErr.Error())), nil
	}

	configDigest := digest(configBytes)
	configPath := filepath.Join(dir, "config.yaml")
	unchanged := ok && state.ConfigDigest == configDigest && sameResources(state.Resources, stored)
	if unchanged {
		if onDisk, err := fileDigest(configPath); err == nil && onDisk == configDigest {
			return deliveredReceipt(changes, id), nil
		}
	}

	next := storedState{
		Version:      stateVersion,
		Target:       changes.Target,
		OwnerEpoch:   changes.OwnerEpoch,
		ConfigDigest: configDigest,
		Resources:    sortedStored(stored),
	}
	// State first: a crash before the config write is detected by digest
	// mismatch in Observe and repaired by the next Apply.
	if err := writeState(dir, next); err != nil {
		return delivery.Receipt{}, err
	}
	// Re-check before publishing the document to narrow the takeover window.
	updated, exists, err := loadOwner(dir)
	if err != nil {
		return delivery.Receipt{}, err
	}
	if !exists || updated.Epoch != changes.OwnerEpoch {
		return delivery.Receipt{}, errors.New("praxis config target owner changed during write")
	}
	if err := writeAtomic(configPath, configBytes); err != nil {
		return delivery.Receipt{}, err
	}
	updated, exists, err = loadOwner(dir)
	if err != nil {
		return delivery.Receipt{}, err
	}
	if !exists || updated.Epoch != changes.OwnerEpoch {
		return delivery.Receipt{}, errors.New("praxis config target owner changed during write")
	}
	return deliveredReceipt(changes, id), nil
}

func (c *Configurator) Observe(ctx context.Context, target config.Target) (delivery.Observation, error) {
	if err := ctx.Err(); err != nil {
		return delivery.Observation{}, err
	}
	dir, err := c.targetDir(target)
	if err != nil {
		return delivery.Observation{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	observation := delivery.Observation{Target: target}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return observation, nil
	} else if err != nil {
		return delivery.Observation{}, err
	}
	observation.Connected = true
	state, ok, err := loadState(dir)
	if err != nil {
		return delivery.Observation{}, err
	}
	if !ok || state.Target != target {
		return observation, nil
	}
	delivered := false
	if onDisk, err := fileDigest(filepath.Join(dir, "config.yaml")); err == nil && onDisk == state.ConfigDigest {
		delivered = true
	}
	for _, resource := range state.Resources {
		result := delivery.ResourceResult{Key: resource.Key, DesiredRevision: resource.Revision}
		if delivered {
			result.State = delivery.ApplyDelivered
			result.DeliveredRevision = resource.Revision
		} else {
			result.State = delivery.ApplyPending
		}
		observation.Resources = append(observation.Resources, result)
	}
	return observation, nil
}

func (c *Configurator) targetDir(target config.Target) (string, error) {
	return secureJoin(c.options.Root, target.Namespace, target.Name, target.UID)
}

func (o ownerState) retired(epoch string) bool {
	for _, candidate := range o.RetiredEpochs {
		if candidate == epoch {
			return true
		}
	}
	return false
}

func sameResources(current []storedResource, wanted map[string]storedResource) bool {
	if len(current) != len(wanted) {
		return false
	}
	for _, resource := range current {
		other, ok := wanted[resource.Key.String()]
		if !ok || other.Revision != resource.Revision {
			return false
		}
	}
	return true
}

func sortedStored(stored map[string]storedResource) []storedResource {
	keys := make([]string, 0, len(stored))
	for key := range stored {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	resources := make([]storedResource, 0, len(keys))
	for _, key := range keys {
		resources = append(resources, stored[key])
	}
	return resources
}

func hasListener(stored map[string]storedResource) bool {
	for _, resource := range stored {
		if resource.ValueKind == config.KindListener {
			return true
		}
	}
	return false
}

func rejectedReceipt(changes config.ChangeSet, id, reason string) delivery.Receipt {
	receipt := delivery.Receipt{Target: changes.Target, ChangeID: id}
	for _, resource := range changes.Upserts {
		receipt.Resources = append(receipt.Resources, delivery.ResourceResult{
			Key:             resource.Key,
			DesiredRevision: resource.Revision,
			State:           delivery.ApplyRejected,
			Reason:          reason,
		})
	}
	for _, key := range changes.Deletes {
		receipt.Resources = append(receipt.Resources, delivery.ResourceResult{Key: key, State: delivery.ApplyPending})
	}
	sortReceipt(&receipt)
	return receipt
}

func deliveredReceipt(changes config.ChangeSet, id string) delivery.Receipt {
	receipt := delivery.Receipt{Target: changes.Target, ChangeID: id}
	for _, resource := range changes.Upserts {
		receipt.Resources = append(receipt.Resources, delivery.ResourceResult{
			Key:               resource.Key,
			DesiredRevision:   resource.Revision,
			DeliveredRevision: resource.Revision,
			State:             delivery.ApplyDelivered,
		})
	}
	for _, key := range changes.Deletes {
		receipt.Resources = append(receipt.Resources, delivery.ResourceResult{Key: key, State: delivery.ApplyDeleted})
	}
	sortReceipt(&receipt)
	return receipt
}

func sortReceipt(receipt *delivery.Receipt) {
	sort.Slice(receipt.Resources, func(i, j int) bool {
		return receipt.Resources[i].Key.String() < receipt.Resources[j].Key.String()
	})
}

func sanitize(reason string) string {
	reason = strings.ReplaceAll(reason, "\n", " ")
	if len(reason) > 256 {
		reason = reason[:256]
	}
	return reason
}
