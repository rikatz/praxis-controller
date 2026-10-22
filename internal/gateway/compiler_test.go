package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/config"
)

func TestResolveBackendRequiresCrossNamespaceGrant(t *testing.T) {
	input := backendTestInput()
	backendNS := gatewayv1.Namespace("backend")
	input.Reference.Namespace = &backendNS
	input.Service.Namespace = "backend"

	denied := ResolveBackend(input)
	if denied.Reference.Valid || denied.Reference.Reason != string(gatewayv1.RouteReasonRefNotPermitted) {
		t.Fatalf("cross-namespace backend without a grant was not denied: valid=%t reason=%q", denied.Reference.Valid, denied.Reference.Reason)
	}

	input.Permitted = true
	allowed := ResolveBackend(input)
	if !allowed.Reference.Valid || allowed.Cluster == nil || allowed.EndpointSet == nil {
		t.Fatalf("granted cross-namespace backend did not resolve: valid=%t reason=%q", allowed.Reference.Valid, allowed.Reference.Reason)
	}
	if allowed.EndpointSet.Key.Name != allowed.Cluster.Value.(config.Cluster).EndpointSet {
		t.Fatal("Cluster and EndpointSet use different identities")
	}
}

func TestResolveEndpointSetCanonicalizesAndFiltersSlices(t *testing.T) {
	service := testService("backend", "api", "service-uid")
	service.Spec.ClusterIP = corev1.ClusterIPNone
	port := service.Spec.Ports[0]
	readyFalse := false
	validOwner := metav1.OwnerReference{APIVersion: "v1", Kind: "Service", Name: service.Name, UID: service.UID}
	staleOwner := validOwner
	staleOwner.UID = "old-service-uid"
	wrongOwnerVersion := validOwner
	wrongOwnerVersion.APIVersion = "example.net/v1"
	wrongProtocol := corev1.ProtocolUDP
	portName := "http"
	goodPort := int32(9000)
	badPort := int32(0)
	wrongFamily := endpointSlice("backend", service.Name, "2001:db8::16", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), nil)
	wrongFamily.AddressType = discoveryv1.AddressTypeIPv4

	slices := []discoveryv1.EndpointSlice{
		endpointSlice("backend", service.Name, "192.0.2.10", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), nil),
		endpointSlice("backend", service.Name, "2001:0db8:0000:0000:0000:0000:0000:0002", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), &validOwner),
		endpointSlice("backend", service.Name, "192.0.2.11", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), &staleOwner),
		endpointSlice("backend", service.Name, "192.0.2.17", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), &wrongOwnerVersion),
		endpointSlice("backend", "other-service", "192.0.2.12", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), nil),
		endpointSlice("other", service.Name, "192.0.2.13", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), nil),
		endpointSlice("backend", service.Name, "192.0.2.14", discoveryPort(portName, &wrongProtocol, &goodPort), nil),
		endpointSlice("backend", service.Name, "192.0.2.15", discoveryPort(portName, pointer(corev1.ProtocolTCP), &badPort), nil),
		wrongFamily,
		endpointSlice("backend", service.Name, "not-an-ip", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), nil),
		endpointSliceWithReady("backend", service.Name, "192.0.2.10", discoveryPort(portName, pointer(corev1.ProtocolTCP), &goodPort), nil, &readyFalse),
	}

	target := config.Target{Namespace: "gateway", Name: "edge", UID: "gateway-uid"}
	const ownerEpoch = "controller-epoch"
	first, err := ResolveEndpointSet(target, ownerEpoch, service, port, slices)
	if err != nil {
		t.Fatalf("ResolveEndpointSet failed: %v", err)
	}
	reversed := append([]discoveryv1.EndpointSlice(nil), slices...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	second, err := ResolveEndpointSet(target, ownerEpoch, service, port, reversed)
	if err != nil {
		t.Fatalf("ResolveEndpointSet on reordered slices failed: %v", err)
	}
	if first.Key != second.Key || first.Revision != second.Revision {
		t.Fatal("EndpointSet key or revision changed when input slices were permuted")
	}

	value := first.Value.(config.EndpointSet)
	if len(value.Endpoints) != 2 {
		t.Fatalf("got %d endpoints after filtering; want 2", len(value.Endpoints))
	}
	if value.Endpoints[0].Address != "192.0.2.10" || value.Endpoints[0].Ready {
		t.Fatal("duplicate endpoint was not canonicalized with conservative readiness")
	}
	if value.Endpoints[1].Address != "2001:db8::2" || !value.Endpoints[1].Ready {
		t.Fatal("IPv6 endpoint was not canonicalized or defaulted to ready")
	}

	changedUID := service.DeepCopy()
	changedUID.UID = "replacement-uid"
	third, err := ResolveEndpointSet(target, ownerEpoch, *changedUID, port, slices)
	if err != nil {
		t.Fatalf("ResolveEndpointSet for replacement Service failed: %v", err)
	}
	if first.Key.Name == third.Key.Name {
		t.Fatal("EndpointSet key did not change when Service UID changed")
	}
}

func TestCompileHTTPRouteTranslationAndCrossNamespaceGrant(t *testing.T) {
	input := compilerTestInput()
	service := testService("backend", "api", "backend-uid")
	input.Services[types.NamespacedName{Namespace: service.Namespace, Name: service.Name}] = service
	input.EndpointSlices[types.NamespacedName{Namespace: service.Namespace, Name: service.Name}] = []discoveryv1.EndpointSlice{
		endpointSlice("backend", service.Name, "192.0.2.20", discoveryPort("http", pointer(corev1.ProtocolTCP), pointer(int32(9000))), nil),
	}

	route := testRoute("apps", "web")
	match := gatewayv1.HTTPRouteMatch{
		Path:    &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/items")},
		Headers: []gatewayv1.HTTPHeaderMatch{{Name: "X-Request-ID", Value: "fixed"}},
	}
	serviceName := gatewayv1.ObjectName(service.Name)
	serviceNamespace := gatewayv1.Namespace(service.Namespace)
	servicePort := gatewayv1.PortNumber(8080)
	weight := int32(70)
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
		Matches: []gatewayv1.HTTPRouteMatch{match},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: serviceName, Namespace: &serviceNamespace, Port: &servicePort},
			Weight:                 &weight,
		}},
	}}
	input.Routes = []gatewayv1.HTTPRoute{route}
	input.ReferenceGrants = []gatewayv1.ReferenceGrant{serviceGrant("backend", "apps", "api")}

	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	routeStatus := result.RouteStatuses[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
	if !conditionIs(routeStatus.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue) {
		t.Fatal("route was not accepted")
	}
	if !conditionIs(routeStatus.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionTrue) {
		t.Fatal("granted cross-namespace Service reference was not resolved")
	}

	table := findValue[config.HTTPRouteTable](result.Resources, config.KindHTTPRoute)
	if table == nil || len(table.Rules) != 1 || len(table.Rules[0].Matches) != 1 {
		t.Fatal("compiler did not emit the expected HTTP route table")
	}
	translated := table.Rules[0].Matches[0]
	if translated.PathType != string(gatewayv1.PathMatchExact) || translated.Path != "/items" || len(translated.Headers) != 1 || translated.Headers[0].Name != "x-request-id" || translated.Headers[0].Value != "fixed" {
		t.Fatal("exact path or case-insensitive header name was translated incorrectly")
	}
	backends := table.Rules[0].Action.Backends
	if table.Rules[0].Action.Kind != config.ActionForward || len(backends) != 1 || !backends[0].Valid || backends[0].Weight != weight {
		t.Fatal("weighted cross-namespace backend was not retained and resolved")
	}

	input.ReferenceGrants = nil
	denied := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	deniedStatus := denied.RouteStatuses[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
	if !conditionIs(deniedStatus.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue) ||
		!conditionIs(deniedStatus.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionFalse) {
		t.Fatal("missing cross-namespace Service grant did not preserve attachment and report unresolved refs")
	}
	deniedTable := findValue[config.HTTPRouteTable](denied.Resources, config.KindHTTPRoute)
	if deniedTable == nil || len(deniedTable.Rules[0].Action.Backends) != 1 || deniedTable.Rules[0].Action.Backends[0].Valid {
		t.Fatal("denied weighted backend reference was dropped or marked valid")
	}
}

func TestCompileHonorsAllowedRouteNamespaceSelectorAndKind(t *testing.T) {
	input := compilerTestInput()
	route := testRoute("apps", "namespace-selection")
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{
		Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/allowed")},
	}}}}
	input.Routes = []gatewayv1.HTTPRoute{route}
	from := gatewayv1.NamespacesFromSelector
	input.Gateway.Spec.Listeners[0].AllowedRoutes.Namespaces = &gatewayv1.RouteNamespaces{
		From:     &from,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"gateway-access": "allowed"}},
	}
	compiler := Compiler{ControllerName: "example.net/controller"}

	denied := compiler.Compile(input)
	deniedStatus := denied.RouteStatuses[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
	if len(deniedStatus.Parents) != 1 ||
		!conditionIs(deniedStatus.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionFalse) ||
		denied.GatewayStatus.Listeners[0].AttachedRoutes != 0 {
		t.Fatal("namespace selector did not deny an unlabeled Route")
	}

	namespace := input.Namespaces[types.NamespacedName{Name: route.Namespace}]
	namespace.Labels = map[string]string{"gateway-access": "allowed"}
	input.Namespaces[types.NamespacedName{Name: route.Namespace}] = namespace
	allowed := compiler.Compile(input)
	allowedStatus := allowed.RouteStatuses[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
	if len(allowedStatus.Parents) != 1 ||
		!conditionIs(allowedStatus.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue) ||
		allowed.GatewayStatus.Listeners[0].AttachedRoutes != 1 {
		t.Fatal("namespace selector did not allow a matching Route")
	}

	input.Gateway.Spec.Listeners[0].AllowedRoutes.Kinds = []gatewayv1.RouteGroupKind{{
		Group: pointer(gatewayv1.Group(gatewayv1.GroupName)), Kind: "GRPCRoute",
	}}
	wrongKind := compiler.Compile(input)
	wrongKindStatus := wrongKind.RouteStatuses[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
	if len(wrongKindStatus.Parents) != 1 ||
		!conditionIs(wrongKindStatus.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionFalse) ||
		wrongKind.GatewayStatus.Listeners[0].AttachedRoutes != 0 {
		t.Fatal("listener AllowedRoutes kinds did not reject an HTTPRoute")
	}
}

func TestCompileKeepsInvalidBackendWeight(t *testing.T) {
	input := compilerTestInput()
	route := testRoute("apps", "invalid-weight")
	port := gatewayv1.PortNumber(8080)
	weight := int32(-3)
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend", Port: &port},
		Weight:                 &weight,
	}}}}
	input.Routes = []gatewayv1.HTTPRoute{route}
	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	table := findValue[config.HTTPRouteTable](result.Resources, config.KindHTTPRoute)
	if table == nil || len(table.Rules) != 1 || len(table.Rules[0].Action.Backends) != 1 {
		t.Fatal("invalid weighted backend reference was omitted")
	}
	backend := table.Rules[0].Action.Backends[0]
	if backend.Weight != weight || backend.Valid || table.Rules[0].Action.Kind != config.ActionUnsupported {
		t.Fatal("invalid backend weight was changed or treated as routable")
	}
}

func TestCompileSecretHandleUsesMetadataAndRotatesRevision(t *testing.T) {
	input := compilerTestInput()
	secretName := gatewayv1.ObjectName("cert")
	secretNamespace := gatewayv1.Namespace("certs")
	certificateRef := gatewayv1.SecretObjectReference{Name: secretName, Namespace: &secretNamespace}
	input.Gateway.Spec.Listeners[0].Protocol = gatewayv1.HTTPSProtocolType
	input.Gateway.Spec.Listeners[0].TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{certificateRef}}
	key := types.NamespacedName{Namespace: "certs", Name: "cert"}
	certificate, privateKey := testTLSKeyPair(t)
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, UID: "secret-uid", ResourceVersion: "4"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: certificate, corev1.TLSPrivateKeyKey: privateKey},
	}
	input.Secrets = map[types.NamespacedName]TLSSecret{key: TLSSecretInfo(&secret)}
	compiler := Compiler{ControllerName: "example.net/controller"}

	input.ReferenceGrants = nil
	denied := compiler.Compile(input)
	if findResource(denied.Resources, config.KindSecretHandle) != nil || findResource(denied.Resources, config.KindListener) != nil {
		t.Fatal("cross-namespace certificate reference without a grant was emitted")
	}
	if !conditionIs(denied.GatewayStatus.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionFalse) {
		t.Fatal("cross-namespace certificate denial was not reported in listener status")
	}

	input.ReferenceGrants = []gatewayv1.ReferenceGrant{secretGrant("certs", "gateway", "cert")}

	first := compiler.Compile(input)
	firstListenerResource := findResource(first.Resources, config.KindListener)
	firstHandleResource := findResource(first.Resources, config.KindSecretHandle)
	if firstListenerResource == nil || firstHandleResource == nil {
		t.Fatal("authorized HTTPS listener did not emit a stable reference and secret handle")
	}
	firstListener := firstListenerResource.Value.(config.Listener)
	firstHandle := firstHandleResource.Value.(config.SecretHandle)
	if len(firstListener.Secrets) != 1 {
		t.Fatal("listener did not retain its stable Secret reference")
	}
	if firstHandle.ResourceVersion != "4" || firstHandle.UID != "secret-uid" {
		t.Fatal("SecretHandle did not contain the Secret's non-secret rotation metadata")
	}

	invalidSecret := secret.DeepCopy()
	invalidSecret.Type = corev1.SecretTypeOpaque
	input.Secrets[key] = TLSSecretInfo(invalidSecret)
	invalid := compiler.Compile(input)
	if findResource(invalid.Resources, config.KindSecretHandle) != nil || findResource(invalid.Resources, config.KindListener) != nil ||
		!conditionIs(invalid.GatewayStatus.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionFalse) {
		t.Fatal("invalid TLS Secret was published or reported as resolved")
	}

	secret.ResourceVersion = "5"
	input.Secrets[key] = TLSSecretInfo(&secret)
	second := compiler.Compile(input)
	secondListenerResource := findResource(second.Resources, config.KindListener)
	secondHandleResource := findResource(second.Resources, config.KindSecretHandle)
	if secondListenerResource == nil || secondHandleResource == nil || firstListenerResource.Revision != secondListenerResource.Revision || firstHandleResource.Key != secondHandleResource.Key || firstHandleResource.Revision == secondHandleResource.Revision {
		t.Fatal("Secret metadata rotation changed the listener or failed to rotate the handle revision")
	}
	encoded, err := json.Marshal(first.Resources)
	if err != nil {
		t.Fatal("compiler resources could not be serialized")
	}
	if strings.Contains(string(encoded), string(certificate)) || strings.Contains(string(encoded), string(privateKey)) {
		t.Fatal("Secret data entered an intermediate resource")
	}
}

func TestCompileOutputRevisionsIgnoreRouteInputPermutation(t *testing.T) {
	input := compilerTestInput()
	firstRoute := testRoute("apps", "alpha")
	firstRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Value: pointer("/alpha")}}}}}
	secondRoute := testRoute("apps", "beta")
	secondRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Value: pointer("/beta")}}}}}
	input.Routes = []gatewayv1.HTTPRoute{firstRoute, secondRoute}
	compiler := Compiler{ControllerName: "example.net/controller"}
	one := compiler.Compile(input)
	input.Routes = []gatewayv1.HTTPRoute{secondRoute, firstRoute}
	two := compiler.Compile(input)
	if !reflect.DeepEqual(resourceRevisions(one.Resources), resourceRevisions(two.Resources)) {
		t.Fatal("resource keys or revisions changed when Routes were permuted")
	}
}

func TestCompileCanonicalizesMatchAndBackendPermutation(t *testing.T) {
	input := compilerTestInput()
	for _, name := range []string{"alpha", "beta"} {
		service := testService("apps", name, name+"-uid")
		input.Services[types.NamespacedName{Namespace: service.Namespace, Name: service.Name}] = service
	}
	route := testRoute("apps", "canonical")
	pathPrefix := gatewayv1.PathMatchPathPrefix
	pathExact := gatewayv1.PathMatchExact
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
		Matches: []gatewayv1.HTTPRouteMatch{
			{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: pointer("/items")}},
			{Path: &gatewayv1.HTTPPathMatch{Type: &pathExact, Value: pointer("/health")}},
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "beta", Port: pointer(gatewayv1.PortNumber(8080))}},
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "alpha", Port: pointer(gatewayv1.PortNumber(8080))}},
		},
	}}
	input.Routes = []gatewayv1.HTTPRoute{route}
	compiler := Compiler{ControllerName: "example.net/controller"}
	one := compiler.Compile(input)
	input.Routes[0].Spec.Rules[0].Matches[0], input.Routes[0].Spec.Rules[0].Matches[1] = input.Routes[0].Spec.Rules[0].Matches[1], input.Routes[0].Spec.Rules[0].Matches[0]
	input.Routes[0].Spec.Rules[0].BackendRefs[0], input.Routes[0].Spec.Rules[0].BackendRefs[1] = input.Routes[0].Spec.Rules[0].BackendRefs[1], input.Routes[0].Spec.Rules[0].BackendRefs[0]
	two := compiler.Compile(input)
	if !reflect.DeepEqual(resourceRevisions(one.Resources), resourceRevisions(two.Resources)) {
		t.Fatal("resource revisions changed when unordered match and backend inputs were permuted")
	}
}

func TestCompileRanksEachORMatchIndependently(t *testing.T) {
	input := compilerTestInput()
	broadRoute := testRoute("apps", "z-broad")
	broadRoute.CreationTimestamp = metav1.NewTime(time.Unix(20, 0))
	broadRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{
		{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchPathPrefix), Value: pointer("/api")}},
		{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/api/v1")}},
	}}}

	olderRoute := testRoute("apps", "a-older")
	olderRoute.CreationTimestamp = metav1.NewTime(time.Unix(10, 0))
	olderRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{
		{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchPathPrefix), Value: pointer("/api")}},
	}}}
	input.Routes = []gatewayv1.HTTPRoute{broadRoute, olderRoute}

	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	table := findValue[config.HTTPRouteTable](result.Resources, config.KindHTTPRoute)
	if table == nil || len(table.Rules) != 3 {
		t.Fatal("compiler did not emit one table record per OR match")
	}
	first, second, third := table.Rules[0], table.Rules[1], table.Rules[2]
	if first.Source.Name != broadRoute.Name || first.Matches[0].PathType != string(gatewayv1.PathMatchExact) ||
		second.Source.Name != olderRoute.Name || third.Source.Name != broadRoute.Name ||
		second.Matches[0].Path != "/api" || third.Matches[0].Path != "/api" {
		t.Fatal("a broad alternative inherited a stronger OR match's priority")
	}
}

func TestCompileRanksEachHostnameIntersectionIndependently(t *testing.T) {
	input := compilerTestInput()
	wildcardRoute := testRoute("apps", "a-wildcard")
	wildcardRoute.CreationTimestamp = metav1.NewTime(time.Unix(10, 0))
	wildcardRoute.Spec.Hostnames = []gatewayv1.Hostname{"*.example.com", "foo.example.com"}
	wildcardRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{
		Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchPathPrefix), Value: pointer("/")},
	}}}}

	exactRoute := testRoute("apps", "z-exact-bar")
	exactRoute.CreationTimestamp = metav1.NewTime(time.Unix(20, 0))
	exactRoute.Spec.Hostnames = []gatewayv1.Hostname{"bar.example.com"}
	exactRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{
		Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchPathPrefix), Value: pointer("/")},
	}}}}
	input.Routes = []gatewayv1.HTTPRoute{wildcardRoute, exactRoute}

	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	table := findValue[config.HTTPRouteTable](result.Resources, config.KindHTTPRoute)
	if table == nil || len(table.Rules) != 3 {
		t.Fatal("compiler did not emit an independently ranked row for each hostname")
	}
	if table.Rules[0].Source.Name != wildcardRoute.Name || len(table.Rules[0].Hostnames) != 1 || table.Rules[0].Hostnames[0] != "foo.example.com" ||
		table.Rules[1].Source.Name != exactRoute.Name || len(table.Rules[1].Hostnames) != 1 || table.Rules[1].Hostnames[0] != "bar.example.com" ||
		table.Rules[2].Source.Name != wildcardRoute.Name || len(table.Rules[2].Hostnames) != 1 || table.Rules[2].Hostnames[0] != "*.example.com" {
		t.Fatal("wildcard row inherited the exact hostname's precedence")
	}
}

func TestCompileDoesNotPublishUnsupportedMatchesAsBroaderRules(t *testing.T) {
	input := compilerTestInput()
	regex := gatewayv1.PathMatchType("RegularExpression")
	method := gatewayv1.HTTPMethod("GET")
	unsupportedRoute := testRoute("apps", "unsupported-matches")
	orphanBackend := testService("apps", "orphan-backend", "orphan-backend-uid")
	input.Services[types.NamespacedName{Namespace: orphanBackend.Namespace, Name: orphanBackend.Name}] = orphanBackend
	backendPort := gatewayv1.PortNumber(8080)
	unsupportedRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{
		Matches: []gatewayv1.HTTPRouteMatch{
			{Path: &gatewayv1.HTTPPathMatch{Type: &regex, Value: pointer("/admin/.*")}},
			{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchPathPrefix), Value: pointer("/method")}, Method: &method},
			{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/query")}, QueryParams: []gatewayv1.HTTPQueryParamMatch{{Name: "mode", Value: "private"}}},
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "orphan-backend", Port: &backendPort}}},
	}}
	supportedRoute := testRoute("apps", "supported")
	supportedRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{
		Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/safe")},
	}}}}
	input.Routes = []gatewayv1.HTTPRoute{unsupportedRoute, supportedRoute}

	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	table := findValue[config.HTTPRouteTable](result.Resources, config.KindHTTPRoute)
	if table == nil || len(table.Rules) != 1 || table.Rules[0].Source.Name != supportedRoute.Name ||
		table.Rules[0].Matches[0].PathType != string(gatewayv1.PathMatchExact) || table.Rules[0].Matches[0].Path != "/safe" {
		t.Fatal("unsupported match features were published as broader route rules")
	}
	status := result.RouteStatuses[types.NamespacedName{Namespace: unsupportedRoute.Namespace, Name: unsupportedRoute.Name}]
	if len(status.Parents) != 1 || !conditionIs(status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionFalse) {
		t.Fatal("route with only unsupported match features was not rejected")
	}
	if result.GatewayStatus.Listeners[0].AttachedRoutes != 1 {
		t.Fatal("route rejected for unsupported matches contributed to AttachedRoutes")
	}
	if findResource(result.Resources, config.KindCluster) != nil || findResource(result.Resources, config.KindEndpointSet) != nil {
		t.Fatal("route without a supported match emitted orphan backend resources")
	}
}

func TestCompileResolvesBackendRefsWhenMatchesAreUnsupported(t *testing.T) {
	input := compilerTestInput()
	method := gatewayv1.HTTPMethod("GET")
	backendNamespace := gatewayv1.Namespace("backend")
	port := gatewayv1.PortNumber(8080)
	route := testRoute("apps", "unsupported-match-invalid-backend")
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
		Matches: []gatewayv1.HTTPRouteMatch{{Method: &method}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{BackendObjectReference: gatewayv1.BackendObjectReference{
			Name: "private", Namespace: &backendNamespace, Port: &port,
		}}},
	}}
	input.Routes = []gatewayv1.HTTPRoute{route}

	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	status := result.RouteStatuses[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
	if len(status.Parents) != 1 ||
		!conditionIs(status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionFalse) ||
		!conditionIs(status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionFalse) {
		t.Fatal("unsupported matches hid the unauthorized backend reference from route status")
	}
	if findResource(result.Resources, config.KindCluster) != nil || findResource(result.Resources, config.KindEndpointSet) != nil {
		t.Fatal("invalid backend reference emitted backend resources")
	}
}

func TestCompileResolvesBackendRefsWithUnsupportedBackendFilters(t *testing.T) {
	port := gatewayv1.PortNumber(8080)
	backendNamespace := gatewayv1.Namespace("backend")
	for _, test := range []struct {
		name               string
		backendName        gatewayv1.ObjectName
		backendNS          *gatewayv1.Namespace
		service            *corev1.Service
		wantResolvedRefs   metav1.ConditionStatus
		wantResolvedReason string
	}{
		{name: "missing Service", backendName: "missing", wantResolvedRefs: metav1.ConditionFalse, wantResolvedReason: string(gatewayv1.RouteReasonBackendNotFound)},
		{name: "ungranted cross-namespace Service", backendName: "private", backendNS: &backendNamespace, wantResolvedRefs: metav1.ConditionFalse, wantResolvedReason: string(gatewayv1.RouteReasonRefNotPermitted)},
		{name: "valid Service is still omitted", backendName: "valid", service: pointer(testService("apps", "valid", "valid-service-uid")), wantResolvedRefs: metav1.ConditionTrue},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := compilerTestInput()
			if test.service != nil {
				input.Services[types.NamespacedName{Namespace: test.service.Namespace, Name: test.service.Name}] = *test.service
			}
			route := testRoute("apps", "unsupported-backend-filter")
			route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/private")}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: test.backendName, Namespace: test.backendNS, Port: &port},
					Filters:                []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{}}},
				}},
			}}
			input.Routes = []gatewayv1.HTTPRoute{route}

			result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
			status := result.RouteStatuses[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
			if len(status.Parents) != 1 {
				t.Fatal("route parent status is missing")
			}
			resolvedRefsFound := false
			var resolvedRefsStatus metav1.ConditionStatus
			var resolvedRefsReason string
			for _, condition := range status.Parents[0].Conditions {
				if condition.Type == string(gatewayv1.RouteConditionResolvedRefs) {
					resolvedRefsFound = true
					resolvedRefsStatus = condition.Status
					resolvedRefsReason = condition.Reason
					break
				}
			}
			if !resolvedRefsFound || resolvedRefsStatus != test.wantResolvedRefs {
				t.Fatalf("ResolvedRefs status = %v, want %v", resolvedRefsStatus, test.wantResolvedRefs)
			}
			if test.wantResolvedReason != "" && resolvedRefsReason != test.wantResolvedReason {
				t.Fatalf("ResolvedRefs reason = %q, want %q", resolvedRefsReason, test.wantResolvedReason)
			}
			if findResource(result.Resources, config.KindCluster) != nil || findResource(result.Resources, config.KindEndpointSet) != nil {
				t.Fatal("unsupported backend filter emitted backend resources")
			}
			if table := findValue[config.HTTPRouteTable](result.Resources, config.KindHTTPRoute); table != nil && len(table.Rules) != 0 {
				t.Fatal("unsupported backend filter emitted a route-table row")
			}
		})
	}
}

func TestCompileOmitsUnsupportedFilterRowsAndPreservesPartialStatus(t *testing.T) {
	input := compilerTestInput()
	backend := testService("apps", "filter-backend", "filter-backend-uid")
	input.Services[types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}] = backend
	backendPort := gatewayv1.PortNumber(8080)
	partiallyInvalidRoute := testRoute("apps", "a-partial")
	partiallyInvalidRoute.CreationTimestamp = metav1.NewTime(time.Unix(10, 0))
	sharedMatch := gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/shared")}}
	partiallyInvalidRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{sharedMatch},
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
				ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{Add: []gatewayv1.HTTPHeader{{Name: "x-extra", Value: "yes"}}},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "filter-backend", Port: &backendPort}}},
		},
		{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/other")}}}},
	}
	supportedRoute := testRoute("apps", "z-supported")
	supportedRoute.CreationTimestamp = metav1.NewTime(time.Unix(20, 0))
	supportedRoute.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{sharedMatch}}}
	input.Routes = []gatewayv1.HTTPRoute{partiallyInvalidRoute, supportedRoute}

	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	table := findValue[config.HTTPRouteTable](result.Resources, config.KindHTTPRoute)
	if table == nil || len(table.Rules) != 2 || table.Rules[0].Source.Name != supportedRoute.Name ||
		table.Rules[0].Matches[0].Path != "/shared" || table.Rules[1].Source.Name != partiallyInvalidRoute.Name ||
		table.Rules[1].Matches[0].Path != "/other" {
		t.Fatal("unsupported filter rule was published or shadowed the supported route")
	}
	status := result.RouteStatuses[types.NamespacedName{Namespace: partiallyInvalidRoute.Namespace, Name: partiallyInvalidRoute.Name}]
	if len(status.Parents) != 1 ||
		!conditionIs(status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue) ||
		!conditionIs(status.Parents[0].Conditions, string(gatewayv1.RouteConditionPartiallyInvalid), metav1.ConditionTrue) {
		t.Fatal("partially supported route lost its Accepted or PartiallyInvalid status")
	}
	if findResource(result.Resources, config.KindCluster) != nil || findResource(result.Resources, config.KindEndpointSet) != nil {
		t.Fatal("unsupported filter rule emitted backend resources")
	}
}

func TestHTTPRouteBackendRefsForEndpointSetsOmitsUnpublishedRules(t *testing.T) {
	backend := gatewayv1.HTTPBackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend"}}
	valid := gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend}}
	if refs := HTTPRouteBackendRefsForEndpointSets(valid); len(refs) != 1 {
		t.Fatal("supported default-match rule lost its backend")
	}

	regex := gatewayv1.PathMatchType("RegularExpression")
	unsupportedMatch := valid
	unsupportedMatch.Matches = []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &regex, Value: pointer("/private/.*")}}}
	if refs := HTTPRouteBackendRefsForEndpointSets(unsupportedMatch); len(refs) != 0 {
		t.Fatal("unsupported match rule retained an endpoint dependency")
	}

	unsupportedFilter := valid
	unsupportedFilter.Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier, ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{}}}
	if refs := HTTPRouteBackendRefsForEndpointSets(unsupportedFilter); len(refs) != 0 {
		t.Fatal("unsupported filter rule retained an endpoint dependency")
	}

	backendFilter := valid
	backendFilter.BackendRefs = []gatewayv1.HTTPBackendRef{{BackendObjectReference: backend.BackendObjectReference, Filters: []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{}}}}}
	if refs := HTTPRouteBackendRefsForEndpointSets(backendFilter); len(refs) != 0 {
		t.Fatal("implementation-specific backend filter retained an endpoint dependency")
	}

	partiallySupported := valid
	partiallySupported.Matches = append(unsupportedMatch.Matches, gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Type: pointer(gatewayv1.PathMatchExact), Value: pointer("/public")}})
	if refs := HTTPRouteBackendRefsForEndpointSets(partiallySupported); len(refs) != 1 {
		t.Fatal("supported match alternative lost its endpoint dependency")
	}
}

func TestCompileHTTPSHostnameOverlapIsDistinctButReported(t *testing.T) {
	input := compilerTestInput()
	input.Gateway.Spec.Listeners[0].Protocol = gatewayv1.HTTPSProtocolType
	wildcard := gatewayv1.Hostname("*.example.com")
	input.Gateway.Spec.Listeners[0].Hostname = &wildcard
	input.Gateway.Spec.Listeners[0].TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "wildcard-cert"}}}
	input.Secrets[types.NamespacedName{Namespace: "gateway", Name: "wildcard-cert"}] = TLSSecret{UID: "wildcard-uid", ResourceVersion: "1", Valid: true}

	exact := gatewayv1.Hostname("api.example.com")
	input.Gateway.Spec.Listeners = append(input.Gateway.Spec.Listeners, gatewayv1.Listener{
		Name:          "exact",
		Port:          8080,
		Protocol:      gatewayv1.HTTPSProtocolType,
		Hostname:      &exact,
		AllowedRoutes: input.Gateway.Spec.Listeners[0].AllowedRoutes,
		TLS:           &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "exact-cert"}}},
	})
	input.Secrets[types.NamespacedName{Namespace: "gateway", Name: "exact-cert"}] = TLSSecret{UID: "exact-uid", ResourceVersion: "1", Valid: true}

	result := (Compiler{ControllerName: "example.net/controller"}).Compile(input)
	if len(result.GatewayStatus.Listeners) != 2 {
		t.Fatal("compiler did not report both HTTPS listeners")
	}
	for _, status := range result.GatewayStatus.Listeners {
		if !conditionIs(status.Conditions, string(gatewayv1.ListenerConditionAccepted), metav1.ConditionTrue) ||
			conditionIs(status.Conditions, string(gatewayv1.ListenerConditionConflicted), metav1.ConditionTrue) ||
			!conditionIs(status.Conditions, string(gatewayv1.ListenerConditionOverlappingTLSConfig), metav1.ConditionTrue) {
			t.Fatal("overlapping but distinct HTTPS listener was rejected or not reported")
		}
	}
	if len(findAllResources(result.Resources, config.KindListener)) != 2 {
		t.Fatal("overlapping HTTPS listeners did not both produce listener resources")
	}

	duplicateInput := input
	duplicateInput.Gateway = input.Gateway.DeepCopy()
	duplicateInput.Gateway.Spec.Listeners[1].Hostname = &wildcard
	duplicate := (Compiler{ControllerName: "example.net/controller"}).Compile(duplicateInput)
	for _, status := range duplicate.GatewayStatus.Listeners {
		if !conditionIs(status.Conditions, string(gatewayv1.ListenerConditionConflicted), metav1.ConditionTrue) ||
			!conditionIs(status.Conditions, string(gatewayv1.ListenerConditionAccepted), metav1.ConditionFalse) ||
			!conditionIs(status.Conditions, string(gatewayv1.ListenerConditionOverlappingTLSConfig), metav1.ConditionTrue) {
			t.Fatal("duplicate listener tuple was not rejected")
		}
	}
}

func backendTestInput() BackendInput {
	service := testService("apps", "backend", "backend-uid")
	service.Spec.Ports[0].Name = "http"
	return BackendInput{
		Target:         config.Target{Namespace: "gateway", Name: "edge", UID: "gateway-uid"},
		OwnerEpoch:     "controller-epoch",
		RouteNamespace: "apps",
		Reference:      &gatewayv1.BackendObjectReference{Name: "backend", Port: pointer(gatewayv1.PortNumber(8080))},
		Service:        &service,
	}
}

func compilerTestInput() Input {
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "praxis"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.net/controller"}}
	from := gatewayv1.NamespacesFromAll
	listener := gatewayv1.Listener{
		Name:          "web",
		Port:          8080,
		Protocol:      gatewayv1.HTTPProtocolType,
		AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &from}},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gateway", Name: "edge", UID: "gateway-uid", Generation: 3},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "praxis", Listeners: []gatewayv1.Listener{listener}},
	}
	return Input{
		Target:         config.Target{Namespace: gateway.Namespace, Name: gateway.Name, UID: string(gateway.UID)},
		OwnerEpoch:     "controller-epoch",
		GatewayClass:   class,
		Gateway:        gateway,
		Namespaces:     map[types.NamespacedName]corev1.Namespace{{Name: "apps"}: {ObjectMeta: metav1.ObjectMeta{Name: "apps"}}},
		Services:       make(map[types.NamespacedName]corev1.Service),
		EndpointSlices: make(map[types.NamespacedName][]discoveryv1.EndpointSlice),
		Secrets:        make(map[types.NamespacedName]TLSSecret),
	}
}

func testRoute(namespace, name string) gatewayv1.HTTPRoute {
	parentNS := gatewayv1.Namespace("gateway")
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid"), CreationTimestamp: metav1.NewTime(metav1.Now().Add(-1))},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge", Namespace: &parentNS, SectionName: pointer(gatewayv1.SectionName("web"))}}},
			Hostnames:       []gatewayv1.Hostname{"api.example.com"},
		},
	}
}

func testService(namespace, name, uid string) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid)},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP}}},
	}
}

func endpointSlice(namespace, serviceName, address string, port discoveryv1.EndpointPort, owner *metav1.OwnerReference) discoveryv1.EndpointSlice {
	return endpointSliceWithReady(namespace, serviceName, address, port, owner, nil)
}

func endpointSliceWithReady(namespace, serviceName, address string, port discoveryv1.EndpointPort, owner *metav1.OwnerReference, ready *bool) discoveryv1.EndpointSlice {
	owners := []metav1.OwnerReference(nil)
	if owner != nil {
		owners = []metav1.OwnerReference{*owner}
	}
	addressType := discoveryv1.AddressTypeIPv4
	if strings.Contains(address, ":") {
		addressType = discoveryv1.AddressTypeIPv6
	}
	return discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: namespace, Labels: map[string]string{discoveryServiceNameLabel: serviceName}, OwnerReferences: owners},
		AddressType: addressType,
		Ports:       []discoveryv1.EndpointPort{port},
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: ready}}},
	}
}

func discoveryPort(name string, protocol *corev1.Protocol, port *int32) discoveryv1.EndpointPort {
	return discoveryv1.EndpointPort{Name: &name, Protocol: protocol, Port: port}
}

func serviceGrant(targetNamespace, routeNamespace, serviceName string) gatewayv1.ReferenceGrant {
	from := gatewayv1.ReferenceGrantFrom{Group: gatewayv1.Group(gatewayv1.GroupName), Kind: "HTTPRoute", Namespace: gatewayv1.Namespace(routeNamespace)}
	to := gatewayv1.ReferenceGrantTo{Group: gatewayv1.Group(""), Kind: "Service", Name: pointer(gatewayv1.ObjectName(serviceName))}
	return gatewayv1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: targetNamespace, Name: "allow-web"}, Spec: gatewayv1.ReferenceGrantSpec{From: []gatewayv1.ReferenceGrantFrom{from}, To: []gatewayv1.ReferenceGrantTo{to}}}
}

func secretGrant(targetNamespace, gatewayNamespace, secretName string) gatewayv1.ReferenceGrant {
	from := gatewayv1.ReferenceGrantFrom{Group: gatewayv1.Group(gatewayv1.GroupName), Kind: "Gateway", Namespace: gatewayv1.Namespace(gatewayNamespace)}
	to := gatewayv1.ReferenceGrantTo{Group: gatewayv1.Group(""), Kind: "Secret", Name: pointer(gatewayv1.ObjectName(secretName))}
	return gatewayv1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: targetNamespace, Name: "allow-gateway"}, Spec: gatewayv1.ReferenceGrantSpec{From: []gatewayv1.ReferenceGrantFrom{from}, To: []gatewayv1.ReferenceGrantTo{to}}}
}

func conditionIs(conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}

func findValue[T config.Value](resources []config.Resource, kind config.Kind) *T {
	for i := range resources {
		if resources[i].Key.Kind != kind {
			continue
		}
		value, ok := resources[i].Value.(T)
		if ok {
			return &value
		}
	}
	return nil
}

func findResource(resources []config.Resource, kind config.Kind) *config.Resource {
	for i := range resources {
		if resources[i].Key.Kind == kind {
			return &resources[i]
		}
	}
	return nil
}

func findAllResources(resources []config.Resource, kind config.Kind) []config.Resource {
	result := make([]config.Resource, 0)
	for _, resource := range resources {
		if resource.Key.Kind == kind {
			result = append(result, resource)
		}
	}
	return result
}

func resourceRevisions(resources []config.Resource) map[config.Key]string {
	result := make(map[config.Key]string, len(resources))
	for _, resource := range resources {
		result[resource.Key] = resource.Revision
	}
	return result
}

const discoveryServiceNameLabel = "kubernetes.io/service-name"

func pointer[T any](value T) *T { return &value }
