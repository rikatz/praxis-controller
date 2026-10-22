package praxis

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/praxis-proxy/controller/internal/config"
)

var updateGolden = flag.Bool("update", false, "update golden files")

var (
	testTarget = config.Target{Namespace: "default", Name: "edge", UID: "uid-1"}
	testEpoch  = "epoch-1"
)

func resource(t *testing.T, kind config.Kind, name string, value config.Value) config.Resource {
	t.Helper()
	result, err := config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: testTarget, OwnerEpoch: testEpoch, Kind: kind, Name: name},
		config.ObjectRef{Kind: string(kind), Name: name},
		value,
	)
	if err != nil {
		t.Fatalf("new resource: %v", err)
	}
	return result
}

func httpResources(t *testing.T) []config.Resource {
	t.Helper()
	return []config.Resource{
		resource(t, config.KindListener, "web", config.Listener{Port: 80, Protocol: config.ListenerHTTP, RouteTable: "route-web"}),
		resource(t, config.KindHTTPRoute, "route-web", config.HTTPRouteTable{Rules: []config.HTTPRule{{
			RuleIndex: 0,
			Matches:   []config.HTTPMatch{{PathType: "PathPrefix", Path: "/"}},
			Action:    config.HTTPAction{Kind: config.ActionForward, Backends: []config.BackendRef{{Cluster: "service-80-tcp-abc", Valid: true}}},
			Priority:  0,
		}}}),
		resource(t, config.KindCluster, "service-80-tcp-abc", config.Cluster{
			ServiceNamespace: "default", ServiceName: "app", Port: 80, Protocol: "TCP", EndpointSet: "eps-1",
		}),
		resource(t, config.KindEndpointSet, "eps-1", config.EndpointSet{
			Endpoints: []config.Endpoint{{Address: "10.0.0.1", Port: 8080, Ready: true, Serving: true}},
		}),
	}
}

func TestRenderHTTP(t *testing.T) {
	out, err := Render(httpResources(t), Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"name: web",
		"address: 0.0.0.0:80",
		"filter: router",
		"multi_level_subdomain_matching: true",
		"path_prefix: /",
		"cluster: service-80-tcp-abc",
		"filter: load_balancer",
		`- 10.0.0.1:8080`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "insecure_options") {
		t.Errorf("private endpoints must be opt-in:\n%s", text)
	}
}

func TestRenderHTTPSPathsOnly(t *testing.T) {
	resources := httpResources(t)
	resources[0] = resource(t, config.KindListener, "web", config.Listener{
		Port: 443, Protocol: config.ListenerHTTPS, RouteTable: "route-web",
		Secrets: []config.SecretRef{{Namespace: "cert-ns", Name: "cert"}},
	})
	resources = append(resources, resource(t, config.KindSecretHandle, "cert-ns/cert", config.SecretHandle{
		Namespace: "cert-ns", Name: "cert", UID: "secret-uid", ResourceVersion: "42",
	}))
	out, err := Render(resources, Options{TLSRoot: "/mounted/tls"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"cert_path: /mounted/tls/cert-ns/cert/tls.crt",
		"key_path: /mounted/tls/cert-ns/cert/tls.key",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered config missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"secret-uid", "resourceVersion", "42"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("rendered config leaked %q:\n%s", forbidden, text)
		}
	}
}

func TestRenderDeterministic(t *testing.T) {
	resources := httpResources(t)
	first, err := Render(resources, Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	reversed := make([]config.Resource, len(resources))
	for i := range resources {
		reversed[len(resources)-1-i] = resources[i]
	}
	second, err := Render(reversed, Options{})
	if err != nil {
		t.Fatalf("render reversed: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("render is not deterministic:\n%s\n---\n%s", first, second)
	}
}

func TestRenderEmptyRoutesUsesStaticResponse(t *testing.T) {
	resources := httpResources(t)
	resources[1] = resource(t, config.KindHTTPRoute, "route-web", config.HTTPRouteTable{})
	out, err := Render(resources, Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "filter: static_response") || !strings.Contains(text, "status: 404") {
		t.Fatalf("empty routes did not produce a fail-closed response:\n%s", text)
	}
	if strings.Contains(text, "filter: router") || strings.Contains(text, "load_balancer") {
		t.Fatalf("empty routes emitted a router or load balancer:\n%s", text)
	}
}

func TestRenderRejectsSensitiveEndpointByDefault(t *testing.T) {
	resources := httpResources(t)
	resources[3] = resource(t, config.KindEndpointSet, "eps-1", config.EndpointSet{
		Endpoints: []config.Endpoint{{Address: "127.0.0.1", Port: 8080, Ready: true, Serving: true}},
	})
	if _, err := Render(resources, Options{}); err == nil || !strings.Contains(err.Error(), "sensitive address") {
		t.Fatalf("sensitive endpoint was accepted: %v", err)
	}
	if _, err := Render(resources, Options{AllowPrivateEndpoints: true}); err != nil {
		t.Fatalf("explicit opt-in failed: %v", err)
	}
}

func TestRenderRequiresAListener(t *testing.T) {
	resources := httpResources(t)[1:]
	if _, err := Render(resources, Options{}); err == nil {
		t.Fatal("render without a listener was accepted")
	}
}

func TestRenderValidatesReferencesAndNames(t *testing.T) {
	tests := []struct {
		name      string
		resources []config.Resource
		want      string
	}{
		{
			name: "missing route table",
			resources: func() []config.Resource {
				resources := httpResources(t)
				resources[0] = resource(t, config.KindListener, "web", config.Listener{Port: 80, Protocol: config.ListenerHTTP, RouteTable: "missing"})
				return resources
			}(),
			want: "missing route table",
		},
		{
			name: "missing endpoint set",
			resources: func() []config.Resource {
				return httpResources(t)[:3]
			}(),
			want: "missing endpoint set",
		},
		{
			name: "invalid secret namespace",
			resources: func() []config.Resource {
				resources := httpResources(t)
				resources[0] = resource(t, config.KindListener, "web", config.Listener{
					Port: 443, Protocol: config.ListenerHTTPS, RouteTable: "route-web",
					Secrets: []config.SecretRef{{Namespace: "bad/name", Name: "cert"}},
				})
				return resources
			}(),
			want: "invalid TLS secret namespace",
		},
		{
			name: "empty path",
			resources: func() []config.Resource {
				resources := httpResources(t)
				resources[1] = resource(t, config.KindHTTPRoute, "route-web", config.HTTPRouteTable{Rules: []config.HTTPRule{{
					Matches: []config.HTTPMatch{{PathType: "PathPrefix", Path: ""}},
					Action:  config.HTTPAction{Kind: config.ActionForward, Backends: []config.BackendRef{{Cluster: "service-80-tcp-abc", Valid: true}}},
				}}})
				return resources
			}(),
			want: "empty path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Render(test.resources, Options{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
		})
	}
}

func TestRenderWildcardHost(t *testing.T) {
	resources := httpResources(t)
	table := resources[1].Value.(config.HTTPRouteTable)
	table.Rules[0].Hostnames = []string{"*.example.com"}
	resources[1] = resource(t, config.KindHTTPRoute, "route-web", table)
	out, err := Render(resources, Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "host: '*.example.com'") && !strings.Contains(string(out), "host: \"*.example.com\"") {
		t.Fatalf("wildcard host missing:\n%s", out)
	}
}

func TestRenderGolden(t *testing.T) {
	out, err := Render(httpResources(t), Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	golden := filepath.Join("testdata", "http.golden.yaml")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(expected) != string(out) {
		t.Fatalf("renderer output differs from golden:\n%s", out)
	}
}

func TestRenderGoldenEmptyRoutes(t *testing.T) {
	resources := httpResources(t)
	resources[1] = resource(t, config.KindHTTPRoute, "route-web", config.HTTPRouteTable{})
	out, err := Render(resources, Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	golden := filepath.Join("testdata", "empty-routes.golden.yaml")
	if *updateGolden {
		if err := os.WriteFile(golden, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(expected) != string(out) {
		t.Fatalf("renderer output differs from golden:\n%s", out)
	}
}

func FuzzRender(f *testing.F) {
	f.Add("10.0.0.1", "user", "/api", "example.com")
	f.Fuzz(func(t *testing.T, address, header, path, host string) {
		resources := httpResources(t)
		resources[3] = resource(t, config.KindEndpointSet, "eps-1", config.EndpointSet{
			Endpoints: []config.Endpoint{{Address: address, Port: 8080, Ready: true, Serving: true}},
		})
		table := resources[1].Value.(config.HTTPRouteTable)
		table.Rules[0].Matches[0].Path = path
		table.Rules[0].Matches[0].Headers = []config.HeaderMatch{{Name: header, Value: header}}
		table.Rules[0].Hostnames = []string{host}
		resources[1] = resource(t, config.KindHTTPRoute, "route-web", table)
		out, err := Render(resources, Options{})
		if err != nil {
			return
		}
		var document map[string]any
		if err := yaml.Unmarshal(out, &document); err != nil {
			t.Fatalf("render produced invalid YAML: %v", err)
		}
	})
}

func TestRenderPrivateEndpointsOptIn(t *testing.T) {
	out, err := Render(httpResources(t), Options{AllowPrivateEndpoints: true})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "allow_private_endpoints: true") {
		t.Fatalf("expected insecure_options opt-in:\n%s", out)
	}
}

func TestRenderRejections(t *testing.T) {
	withRule := func(t *testing.T, mutate func(*config.HTTPRule)) []config.Resource {
		t.Helper()
		resources := httpResources(t)
		table := resources[1].Value.(config.HTTPRouteTable)
		mutate(&table.Rules[0])
		resources[1] = resource(t, config.KindHTTPRoute, "route-web", table)
		return resources
	}

	tests := []struct {
		name      string
		resources []config.Resource
		want      string
	}{
		{
			name: "multiple backends",
			resources: withRule(t, func(rule *config.HTTPRule) {
				rule.Action.Backends = append(rule.Action.Backends, config.BackendRef{Cluster: "service-80-tcp-abc", Valid: true})
			}),
			want: "exactly one is representable",
		},
		{
			name: "redirect",
			resources: withRule(t, func(rule *config.HTTPRule) {
				rule.Action.Redirect = &config.Redirect{StatusCode: 301}
			}),
			want: "not representable",
		},
		{
			name: "header modifier",
			resources: withRule(t, func(rule *config.HTTPRule) {
				rule.Action.RequestHeaders = &config.HeaderModifier{Set: []config.HeaderValue{{Name: "x", Value: "y"}}}
			}),
			want: "not representable",
		},
		{
			name: "unsupported action",
			resources: withRule(t, func(rule *config.HTTPRule) {
				rule.Action.Kind = config.ActionNoBackend
			}),
			want: "not representable",
		},
		{
			name: "unsupported path type",
			resources: withRule(t, func(rule *config.HTTPRule) {
				rule.Matches[0].PathType = "RegularExpression"
			}),
			want: "unsupported path type",
		},
		{
			name: "missing cluster",
			resources: withRule(t, func(rule *config.HTTPRule) {
				rule.Action.Backends[0].Cluster = "missing"
			}),
			want: "missing cluster",
		},
		{
			name: "no ready endpoints",
			resources: func() []config.Resource {
				resources := httpResources(t)
				resources[3] = resource(t, config.KindEndpointSet, "eps-1", config.EndpointSet{
					Endpoints: []config.Endpoint{{Address: "10.0.0.1", Port: 8080, Terminating: true}},
				})
				return resources
			}(),
			want: "no ready endpoints",
		},
		{
			name: "duplicate address",
			resources: func() []config.Resource {
				resources := httpResources(t)
				resources = append(resources, resource(t, config.KindListener, "web2", config.Listener{
					Port: 80, Protocol: config.ListenerHTTP, RouteTable: "route-web",
				}))
				return resources
			}(),
			want: "share address",
		},
		{
			name: "secret handle as direct resource only",
			resources: []config.Resource{
				resource(t, config.KindOpaqueRoot, "root", config.OpaqueRoot{Revision: "1"}),
			},
			want: "unsupported kind",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Render(test.resources, Options{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want error containing %q, got %v", test.want, err)
			}
		})
	}
}

func TestRenderRejectsNonPraxisDomain(t *testing.T) {
	value := config.OpaqueRoot{Revision: "1"}
	res, err := config.NewResource(
		config.Key{Domain: config.DomainAI, Target: testTarget, OwnerEpoch: testEpoch, Kind: config.KindOpaqueRoot, Name: "ai"},
		config.ObjectRef{Kind: "AIRoot", Name: "ai"}, value,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render([]config.Resource{res}, Options{}); err == nil {
		t.Fatal("expected non-praxis domain rejection")
	}
}
