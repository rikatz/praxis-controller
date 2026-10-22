package file

import (
	"context"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
)

// Provisioner represents a pre-existing Praxis target whose configuration
// directory this controller may write. It does not create workloads or
// observe a running process.
type Provisioner struct {
	root      string
	addresses []string
}

var _ delivery.Provisioner = (*Provisioner)(nil)

func NewProvisioner(options Options) (*Provisioner, error) {
	if options.Root == "" {
		return nil, errors.New("praxis config root is required")
	}
	if err := ensureDir(options.Root); err != nil {
		return nil, err
	}
	return &Provisioner{
		root:      options.Root,
		addresses: append([]string(nil), options.Addresses...),
	}, nil
}

func (p *Provisioner) Ensure(ctx context.Context, intent delivery.TargetIntent) (delivery.TargetState, error) {
	if err := ctx.Err(); err != nil {
		return delivery.TargetState{}, err
	}
	target, err := targetFromIntent(intent)
	if err != nil {
		return delivery.TargetState{}, err
	}
	dir, err := secureJoin(p.root, target.Namespace, target.Name, target.UID)
	if err != nil {
		return delivery.TargetState{}, err
	}
	if err := ensureDir(dir); err != nil {
		return delivery.TargetState{}, err
	}
	return delivery.TargetState{
		Target:            target,
		Addresses:         append([]string(nil), p.addresses...),
		Ready:             true,
		BootstrapRevision: bootstrapRevision(intent),
	}, nil
}

// Release removes only the UID-scoped configuration directory. It never
// deletes Kubernetes objects and never signals the external Praxis process.
func (p *Provisioner) Release(ctx context.Context, target config.Target) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := secureJoin(p.root, target.Namespace, target.Name, target.UID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func targetFromIntent(intent delivery.TargetIntent) (config.Target, error) {
	gateway := intent.Gateway
	if gateway.Kind != "Gateway" || !validComponent(gateway.Namespace) || !validComponent(gateway.Name) || !validComponent(gateway.UID) {
		return config.Target{}, errors.New("target intent requires a Gateway namespace, name and UID that are safe path components")
	}
	return config.Target{Namespace: gateway.Namespace, Name: gateway.Name, UID: gateway.UID}, nil
}

func bootstrapRevision(intent delivery.TargetIntent) string {
	listeners := append([]delivery.ListenerIntent(nil), intent.Listeners...)
	sort.Slice(listeners, func(i, j int) bool {
		if listeners[i].Name != listeners[j].Name {
			return listeners[i].Name < listeners[j].Name
		}
		return listeners[i].Port < listeners[j].Port
	})
	var builder strings.Builder
	for _, listener := range listeners {
		builder.WriteString(listener.Name)
		builder.WriteByte(':')
		builder.WriteString(strconv.Itoa(int(listener.Port)))
		builder.WriteByte(':')
		builder.WriteString(listener.Protocol)
		builder.WriteByte('\n')
	}
	return digest([]byte(builder.String()))
}
