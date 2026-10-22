// Package praxis lowers typed controller resources into a Praxis YAML
// configuration document. It performs no I/O.
package praxis

import (
	"errors"
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/praxis-proxy/controller/internal/config"
)

type Options struct {
	ListenerBindAddress   string
	TLSRoot               string
	AllowPrivateEndpoints bool
}

// Render produces one deterministic Praxis configuration document for the
// praxis-domain resources of a single target. Unsupported input is rejected
// rather than silently omitted.
func Render(resources []config.Resource, opts Options) ([]byte, error) {
	if opts.ListenerBindAddress == "" {
		opts.ListenerBindAddress = "0.0.0.0"
	}
	if opts.TLSRoot == "" {
		opts.TLSRoot = "/etc/praxis/tls"
	}

	listeners := make(map[string]config.Listener)
	tables := make(map[string]config.HTTPRouteTable)
	clusters := make(map[string]config.Cluster)
	endpointSets := make(map[string]config.EndpointSet)

	for _, resource := range resources {
		if resource.Key.Domain != config.DomainPraxis {
			return nil, fmt.Errorf("resource %q is outside the praxis domain", resource.Key.String())
		}
		name := resource.Key.Name
		switch value := resource.Value.(type) {
		case config.Listener:
			if _, exists := listeners[name]; exists {
				return nil, fmt.Errorf("duplicate listener %q", name)
			}
			listeners[name] = value
		case config.HTTPRouteTable:
			if _, exists := tables[name]; exists {
				return nil, fmt.Errorf("duplicate http route table %q", name)
			}
			tables[name] = value
		case config.Cluster:
			if _, exists := clusters[name]; exists {
				return nil, fmt.Errorf("duplicate cluster %q", name)
			}
			clusters[name] = value
		case config.EndpointSet:
			if _, exists := endpointSets[name]; exists {
				return nil, fmt.Errorf("duplicate endpoint set %q", name)
			}
			endpointSets[name] = value
		case config.SecretHandle:
			// Secret material is delivered separately; only listeners may
			// reference it.
		default:
			return nil, fmt.Errorf("resource %q has unsupported kind %q", resource.Key.String(), resource.Key.Kind)
		}
	}
	document := document{
		Listeners:    make([]listenerConfig, 0, len(listeners)),
		FilterChains: make([]filterChainConfig, 0, len(listeners)),
	}
	if opts.AllowPrivateEndpoints {
		document.InsecureOptions = &insecureOptions{AllowPrivateEndpoints: true}
	}

	if len(listeners) == 0 {
		return nil, errors.New("at least one listener is required")
	}

	names := make([]string, 0, len(listeners))
	for name := range listeners {
		names = append(names, name)
	}
	sort.Strings(names)

	addresses := make(map[string]string, len(listeners))
	for _, name := range names {
		listener := listeners[name]
		if listener.Port <= 0 || listener.Port > 65535 {
			return nil, fmt.Errorf("listener %q has an invalid port %d", name, listener.Port)
		}
		if lists := validation.IsDNS1123Label(name); len(lists) != 0 {
			return nil, fmt.Errorf("listener %q is not a valid name", name)
		}
		address := net.JoinHostPort(opts.ListenerBindAddress, strconv.Itoa(int(listener.Port)))
		if other, exists := addresses[address]; exists {
			return nil, fmt.Errorf("listeners %q and %q share address %q", other, name, address)
		}
		addresses[address] = name

		rendered, chain, err := renderListener(name, listener, tables, clusters, endpointSets, opts)
		if err != nil {
			return nil, err
		}
		document.Listeners = append(document.Listeners, rendered)
		document.FilterChains = append(document.FilterChains, chain)
	}

	return yaml.Marshal(document)
}

func renderListener(name string, listener config.Listener, tables map[string]config.HTTPRouteTable, clusters map[string]config.Cluster, endpointSets map[string]config.EndpointSet, opts Options) (listenerConfig, filterChainConfig, error) {
	rendered := listenerConfig{
		Name:         name,
		Address:      net.JoinHostPort(opts.ListenerBindAddress, strconv.Itoa(int(listener.Port))),
		FilterChains: []string{"route-" + name},
	}

	switch listener.Protocol {
	case config.ListenerHTTP:
		if len(listener.Secrets) != 0 {
			return listenerConfig{}, filterChainConfig{}, fmt.Errorf("listener %q is HTTP but references TLS secrets", name)
		}
	case config.ListenerHTTPS:
		if len(listener.Secrets) != 1 {
			return listenerConfig{}, filterChainConfig{}, fmt.Errorf("listener %q requires exactly one TLS secret", name)
		}
		secret := listener.Secrets[0]
		if lists := validation.IsDNS1123Label(secret.Namespace); len(lists) != 0 {
			return listenerConfig{}, filterChainConfig{}, fmt.Errorf("listener %q has an invalid TLS secret namespace", name)
		}
		if lists := validation.IsDNS1123Label(secret.Name); len(lists) != 0 {
			return listenerConfig{}, filterChainConfig{}, fmt.Errorf("listener %q has an invalid TLS secret name", name)
		}
		rendered.TLS = &listenerTLS{Certificates: []certificate{{
			CertPath: path.Join(opts.TLSRoot, secret.Namespace, secret.Name, "tls.crt"),
			KeyPath:  path.Join(opts.TLSRoot, secret.Namespace, secret.Name, "tls.key"),
		}}}
	default:
		return listenerConfig{}, filterChainConfig{}, fmt.Errorf("listener %q has unsupported protocol %q", name, listener.Protocol)
	}

	tableName := listener.RouteTable
	if tableName == "" {
		return listenerConfig{}, filterChainConfig{}, fmt.Errorf("listener %q has no route table", name)
	}
	table, ok := tables[tableName]
	if !ok {
		return listenerConfig{}, filterChainConfig{}, fmt.Errorf("listener %q references missing route table %q", name, tableName)
	}

	routes, clusterNames, err := renderRoutes(table, clusters, endpointSets, opts.AllowPrivateEndpoints)
	if err != nil {
		return listenerConfig{}, filterChainConfig{}, fmt.Errorf("route table %q: %w", tableName, err)
	}

	chain := filterChainConfig{Name: "route-" + name}
	if len(routes) == 0 {
		// Praxis rejects a router with no routes. A static 404 is a valid
		// fail-closed shape for a listener that has no attached routes.
		chain.Filters = []filterEntry{{Filter: "static_response", Status: 404}}
		return rendered, chain, nil
	}
	chain.Filters = []filterEntry{{
		Filter:                      "router",
		MultiLevelSubdomainMatching: true,
		Routes:                      routes,
	}}
	if len(clusterNames) != 0 {
		clustersConfig, err := renderClusters(clusterNames, clusters, endpointSets, opts.AllowPrivateEndpoints)
		if err != nil {
			return listenerConfig{}, filterChainConfig{}, err
		}
		chain.Filters = append(chain.Filters, filterEntry{
			Filter:   "load_balancer",
			Clusters: clustersConfig,
		})
	}
	return rendered, chain, nil
}

func renderRoutes(table config.HTTPRouteTable, clusters map[string]config.Cluster, endpointSets map[string]config.EndpointSet, allowPrivate bool) ([]routeConfig, []string, error) {
	rules := append([]config.HTTPRule(nil), table.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })

	routes := make([]routeConfig, 0, len(rules))
	referenced := make(map[string]struct{})
	for _, rule := range rules {
		if len(rule.Matches) != 1 {
			return nil, nil, fmt.Errorf("rule %d has %d matches; exactly one is representable", rule.RuleIndex, len(rule.Matches))
		}
		match := rule.Matches[0]
		route := routeConfig{}
		if match.Path == "" {
			return nil, nil, fmt.Errorf("rule %d has an empty path", rule.RuleIndex)
		}
		switch match.PathType {
		case "Exact":
			route.Path = match.Path
		case "PathPrefix":
			route.PathPrefix = match.Path
		default:
			return nil, nil, fmt.Errorf("rule %d has unsupported path type %q", rule.RuleIndex, match.PathType)
		}
		if len(match.Headers) != 0 {
			route.Headers = make(map[string]string, len(match.Headers))
			for _, header := range match.Headers {
				if existing, ok := route.Headers[header.Name]; ok && existing != header.Value {
					return nil, nil, fmt.Errorf("rule %d has conflicting values for header %q", rule.RuleIndex, header.Name)
				}
				route.Headers[header.Name] = header.Value
			}
		}
		switch len(rule.Hostnames) {
		case 0:
		case 1:
			route.Host = rule.Hostnames[0]
		default:
			return nil, nil, fmt.Errorf("rule %d has multiple hostnames", rule.RuleIndex)
		}
		if rule.Action.Kind != config.ActionForward {
			return nil, nil, fmt.Errorf("rule %d action %q is not representable", rule.RuleIndex, rule.Action.Kind)
		}
		if rule.Action.Redirect != nil || rule.Action.RequestHeaders != nil {
			return nil, nil, fmt.Errorf("rule %d filters are not representable", rule.RuleIndex)
		}
		if len(rule.Action.Backends) != 1 {
			return nil, nil, fmt.Errorf("rule %d has %d backends; exactly one is representable", rule.RuleIndex, len(rule.Action.Backends))
		}
		backend := rule.Action.Backends[0]
		if !backend.Valid || backend.Cluster == "" {
			return nil, nil, fmt.Errorf("rule %d has no valid backend", rule.RuleIndex)
		}
		cluster, ok := clusters[backend.Cluster]
		if !ok {
			return nil, nil, fmt.Errorf("rule %d references missing cluster %q", rule.RuleIndex, backend.Cluster)
		}
		if _, ok := endpointSets[cluster.EndpointSet]; !ok {
			return nil, nil, fmt.Errorf("cluster %q references missing endpoint set %q", backend.Cluster, cluster.EndpointSet)
		}
		endpoints, err := eligibleEndpoints(endpointSets[cluster.EndpointSet], allowPrivate)
		if err != nil {
			return nil, nil, fmt.Errorf("cluster %q: %w", backend.Cluster, err)
		}
		if len(endpoints) == 0 {
			return nil, nil, fmt.Errorf("cluster %q has no ready endpoints", backend.Cluster)
		}
		route.Cluster = backend.Cluster
		referenced[backend.Cluster] = struct{}{}
		routes = append(routes, route)
	}

	clusterNames := make([]string, 0, len(referenced))
	for name := range referenced {
		clusterNames = append(clusterNames, name)
	}
	sort.Strings(clusterNames)
	return routes, clusterNames, nil
}

func renderClusters(names []string, clusters map[string]config.Cluster, endpointSets map[string]config.EndpointSet, allowPrivate bool) ([]clusterConfig, error) {
	rendered := make([]clusterConfig, 0, len(names))
	for _, name := range names {
		cluster := clusters[name]
		endpoints, err := eligibleEndpoints(endpointSets[cluster.EndpointSet], allowPrivate)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", name, err)
		}
		rendered = append(rendered, clusterConfig{Name: name, Endpoints: endpoints})
	}
	return rendered, nil
}

func eligibleEndpoints(set config.EndpointSet, allowPrivate bool) ([]string, error) {
	seen := make(map[string]struct{}, len(set.Endpoints))
	endpoints := make([]string, 0, len(set.Endpoints))
	for _, endpoint := range set.Endpoints {
		if !endpoint.Ready || !endpoint.Serving || endpoint.Terminating {
			continue
		}
		if endpoint.Address == "" || endpoint.Port <= 0 || endpoint.Port > 65535 {
			return nil, fmt.Errorf("endpoint %q:%d is invalid", endpoint.Address, endpoint.Port)
		}
		ip := net.ParseIP(endpoint.Address)
		if ip == nil {
			return nil, fmt.Errorf("endpoint %q is not an IP address", endpoint.Address)
		}
		if !allowPrivate && sensitiveAddress(ip) {
			return nil, fmt.Errorf("endpoint %q is a sensitive address; enable private endpoints explicitly", endpoint.Address)
		}
		address := net.JoinHostPort(endpoint.Address, strconv.Itoa(int(endpoint.Port)))
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		endpoints = append(endpoints, address)
	}
	sort.Strings(endpoints)
	return endpoints, nil
}

// sensitiveAddress mirrors Praxis's default SSRF rejection for IP literals:
// loopback, link-local, this-host, unspecified, and known cloud metadata.
func sensitiveAddress(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 || v4.IsLoopback() || v4.IsLinkLocalUnicast() || v4.IsLinkLocalMulticast() || v4.IsUnspecified() {
			return true
		}
		metadata := [][4]byte{
			{169, 254, 169, 254}, {169, 254, 170, 2}, {169, 254, 170, 23},
			{100, 100, 100, 200}, {169, 254, 0, 23}, {169, 254, 10, 10},
		}
		for _, candidate := range metadata {
			if v4[0] == candidate[0] && v4[1] == candidate[1] && v4[2] == candidate[2] && v4[3] == candidate[3] {
				return true
			}
		}
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	metadata := []string{"fd00:ec2::254", "fd00:ec2::23", "fd20:ce::254"}
	for _, candidate := range metadata {
		if ip.Equal(net.ParseIP(candidate)) {
			return true
		}
	}
	return false
}

type document struct {
	Listeners       []listenerConfig    `yaml:"listeners"`
	FilterChains    []filterChainConfig `yaml:"filter_chains"`
	InsecureOptions *insecureOptions    `yaml:"insecure_options,omitempty"`
}

type insecureOptions struct {
	AllowPrivateEndpoints bool `yaml:"allow_private_endpoints"`
}

type listenerConfig struct {
	Name         string       `yaml:"name"`
	Address      string       `yaml:"address"`
	FilterChains []string     `yaml:"filter_chains"`
	TLS          *listenerTLS `yaml:"tls,omitempty"`
}

type listenerTLS struct {
	Certificates []certificate `yaml:"certificates"`
}

type certificate struct {
	CertPath string `yaml:"cert_path"`
	KeyPath  string `yaml:"key_path"`
}

type filterChainConfig struct {
	Name    string        `yaml:"name"`
	Filters []filterEntry `yaml:"filters"`
}

type filterEntry struct {
	Filter                      string          `yaml:"filter"`
	Status                      int             `yaml:"status,omitempty"`
	MultiLevelSubdomainMatching bool            `yaml:"multi_level_subdomain_matching,omitempty"`
	Routes                      []routeConfig   `yaml:"routes,omitempty"`
	Clusters                    []clusterConfig `yaml:"clusters,omitempty"`
}

type routeConfig struct {
	Path       string            `yaml:"path,omitempty"`
	PathPrefix string            `yaml:"path_prefix,omitempty"`
	Cluster    string            `yaml:"cluster"`
	Headers    map[string]string `yaml:"headers,omitempty"`
	Host       string            `yaml:"host,omitempty"`
}

type clusterConfig struct {
	Name      string   `yaml:"name"`
	Endpoints []string `yaml:"endpoints"`
}
