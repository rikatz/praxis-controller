package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

type Domain string

const (
	DomainPraxis Domain = "praxis"
	DomainAI     Domain = "ai"
	DomainPolicy Domain = "policy"
)

type Kind string

const (
	KindListener     Kind = "listener"
	KindHTTPRoute    Kind = "http-route-table"
	KindCluster      Kind = "cluster"
	KindEndpointSet  Kind = "endpoint-set"
	KindSecretHandle Kind = "secret-handle"
	KindOpaqueRoot   Kind = "opaque-root"
)

// Target identifies an independently configured Praxis instance. UID fences
// replacement of a Gateway that reuses the same namespace and name.
type Target struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

func (t Target) ID() string {
	return t.Namespace + "/" + t.Name + "@" + t.UID
}

type Key struct {
	Domain     Domain `json:"domain"`
	Target     Target `json:"target"`
	OwnerEpoch string `json:"ownerEpoch"`
	Kind       Kind   `json:"kind"`
	Name       string `json:"name"`
}

func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", k.Domain, k.Target.ID(), k.OwnerEpoch, k.Kind, k.Name)
}

type ObjectRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

type Value interface {
	ResourceKind() Kind
}

type Resource struct {
	Key      Key       `json:"key"`
	Revision string    `json:"revision"`
	Source   ObjectRef `json:"source,omitempty"`
	Value    Value     `json:"value"`
}

func NewResource(key Key, source ObjectRef, value Value) (Resource, error) {
	if value == nil {
		return Resource{}, errors.New("resource value is required")
	}
	if key.Kind != value.ResourceKind() {
		return Resource{}, fmt.Errorf("resource key kind %q does not match value kind %q", key.Kind, value.ResourceKind())
	}
	revision, err := Revision(value)
	if err != nil {
		return Resource{}, err
	}
	return Resource{Key: key, Revision: revision, Source: source, Value: value}, nil
}

func Revision(value Value) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal config resource: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type ListenerProtocol string

const (
	ListenerHTTP  ListenerProtocol = "HTTP"
	ListenerHTTPS ListenerProtocol = "HTTPS"
)

type Listener struct {
	Port       int32            `json:"port"`
	Protocol   ListenerProtocol `json:"protocol"`
	Hostname   string           `json:"hostname,omitempty"`
	RouteTable string           `json:"routeTable,omitempty"`
	Secrets    []SecretRef      `json:"secrets,omitempty"`
}

func (Listener) ResourceKind() Kind { return KindListener }

type SecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type HTTPRouteTable struct {
	Rules []HTTPRule `json:"rules"`
}

func (HTTPRouteTable) ResourceKind() Kind { return KindHTTPRoute }

type HTTPRule struct {
	Source    ObjectRef   `json:"source"`
	RuleIndex int         `json:"ruleIndex"`
	Hostnames []string    `json:"hostnames,omitempty"`
	Matches   []HTTPMatch `json:"matches"`
	Action    HTTPAction  `json:"action"`
	Priority  int         `json:"priority"`
}

type HTTPMatch struct {
	PathType string        `json:"pathType"`
	Path     string        `json:"path"`
	Headers  []HeaderMatch `json:"headers,omitempty"`
}

type HeaderMatch struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HTTPActionKind string

const (
	ActionForward      HTTPActionKind = "forward"
	ActionRedirect     HTTPActionKind = "redirect"
	ActionUnsupported  HTTPActionKind = "unsupported"
	ActionBackendError HTTPActionKind = "backend-error"
	ActionNoBackend    HTTPActionKind = "no-backend"
)

type HTTPAction struct {
	Kind              HTTPActionKind  `json:"kind"`
	Backends          []BackendRef    `json:"backends,omitempty"`
	Redirect          *Redirect       `json:"redirect,omitempty"`
	RequestHeaders    *HeaderModifier `json:"requestHeaders,omitempty"`
	UnsupportedReason string          `json:"unsupportedReason,omitempty"`
}

type BackendRef struct {
	Cluster string `json:"cluster,omitempty"`
	Weight  int32  `json:"weight"`
	Valid   bool   `json:"valid"`
	Reason  string `json:"reason,omitempty"`
}

type Redirect struct {
	Scheme     string `json:"scheme,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	Port       *int32 `json:"port,omitempty"`
	StatusCode int32  `json:"statusCode"`
	Path       string `json:"path,omitempty"`
}

type HeaderModifier struct {
	Add    []HeaderValue `json:"add,omitempty"`
	Set    []HeaderValue `json:"set,omitempty"`
	Remove []string      `json:"remove,omitempty"`
}

type HeaderValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Cluster struct {
	ServiceNamespace string `json:"serviceNamespace"`
	ServiceName      string `json:"serviceName"`
	ServiceUID       string `json:"serviceUID"`
	Port             int32  `json:"port"`
	Protocol         string `json:"protocol"`
	EndpointSet      string `json:"endpointSet"`
}

func (Cluster) ResourceKind() Kind { return KindCluster }

type EndpointSet struct {
	ServiceNamespace string     `json:"serviceNamespace"`
	ServiceName      string     `json:"serviceName"`
	ServiceUID       string     `json:"serviceUID"`
	Port             int32      `json:"port"`
	Protocol         string     `json:"protocol"`
	Endpoints        []Endpoint `json:"endpoints"`
}

func (EndpointSet) ResourceKind() Kind { return KindEndpointSet }

type Endpoint struct {
	Address     string `json:"address"`
	Port        int32  `json:"port"`
	Ready       bool   `json:"ready"`
	Serving     bool   `json:"serving"`
	Terminating bool   `json:"terminating"`
}

type SecretHandle struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

func (SecretHandle) ResourceKind() Kind { return KindSecretHandle }

type OpaqueRoot struct {
	Revision string `json:"revision"`
}

func (OpaqueRoot) ResourceKind() Kind { return KindOpaqueRoot }

type ChangeSet struct {
	Target     Target     `json:"target"`
	OwnerEpoch string     `json:"ownerEpoch"`
	ID         string     `json:"id,omitempty"`
	Upserts    []Resource `json:"upserts,omitempty"`
	Deletes    []Key      `json:"deletes,omitempty"`
}

func (c ChangeSet) Validate() error {
	if c.Target.Namespace == "" || c.Target.Name == "" || c.Target.UID == "" {
		return errors.New("change set target namespace, name and UID are required")
	}
	if c.OwnerEpoch == "" {
		return errors.New("change set owner epoch is required")
	}
	seen := make(map[Key]string, len(c.Upserts)+len(c.Deletes))
	for _, resource := range c.Upserts {
		if err := c.validateUpsertKey(resource.Key); err != nil {
			return err
		}
		if resource.Value == nil || resource.Key.Kind != resource.Value.ResourceKind() {
			return fmt.Errorf("resource %q has a missing or mismatched value", resource.Key.String())
		}
		revision, err := Revision(resource.Value)
		if err != nil {
			return err
		}
		if revision != resource.Revision {
			return fmt.Errorf("resource %q revision does not match its value", resource.Key.String())
		}
		if _, ok := seen[resource.Key]; ok {
			return fmt.Errorf("change set contains duplicate key %q", resource.Key.String())
		}
		seen[resource.Key] = "upsert"
	}
	for _, key := range c.Deletes {
		if err := c.validateDeleteKey(key); err != nil {
			return err
		}
		if operation, ok := seen[key]; ok {
			return fmt.Errorf("change set contains both %s and delete for %q", operation, key.String())
		}
		seen[key] = "delete"
	}
	return nil
}

func (c ChangeSet) validateUpsertKey(key Key) error {
	if key.Domain == "" || key.Target != c.Target || key.OwnerEpoch != c.OwnerEpoch || key.Kind == "" || key.Name == "" {
		return fmt.Errorf("resource key %q does not belong to change set target and owner epoch", key.String())
	}
	return nil
}

func (c ChangeSet) validateDeleteKey(key Key) error {
	if key.Domain == "" || key.Target != c.Target || key.OwnerEpoch == "" || key.Kind == "" || key.Name == "" {
		return fmt.Errorf("resource key %q does not belong to change set target", key.String())
	}
	return nil
}

func (c ChangeSet) StableID() (string, error) {
	keys := make([]string, 0, len(c.Upserts)+len(c.Deletes))
	for _, resource := range c.Upserts {
		keys = append(keys, "upsert:"+resource.Key.String()+":"+resource.Revision)
	}
	for _, key := range c.Deletes {
		keys = append(keys, "delete:"+key.String())
	}
	sort.Strings(keys)
	b, err := json.Marshal(struct {
		Target     Target   `json:"target"`
		OwnerEpoch string   `json:"ownerEpoch"`
		Changes    []string `json:"changes"`
	}{Target: c.Target, OwnerEpoch: c.OwnerEpoch, Changes: keys})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
