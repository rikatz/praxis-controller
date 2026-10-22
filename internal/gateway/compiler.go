package gateway

import (
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/config"
)

type Compiler struct {
	ControllerName string
}

type Input struct {
	Target          config.Target
	OwnerEpoch      string
	GatewayClass    *gatewayv1.GatewayClass
	Gateway         *gatewayv1.Gateway
	Routes          []gatewayv1.HTTPRoute
	Namespaces      map[types.NamespacedName]corev1.Namespace
	Services        map[types.NamespacedName]corev1.Service
	EndpointSlices  map[types.NamespacedName][]discoveryv1.EndpointSlice
	Secrets         map[types.NamespacedName]TLSSecret
	ReferenceGrants []gatewayv1.ReferenceGrant
}

type TLSSecret struct {
	UID             string
	ResourceVersion string
	Valid           bool
}

type Diagnostic struct {
	Object    types.NamespacedName
	Condition string
	Reason    string
	Message   string
}

type Result struct {
	Resources         []config.Resource
	GatewayStatus     gatewayv1.GatewayStatus
	RouteStatuses     map[types.NamespacedName]gatewayv1.HTTPRouteStatus
	Diagnostics       []Diagnostic
	GatewayConditions []metav1.Condition
}

type BackendInput struct {
	Target         config.Target
	OwnerEpoch     string
	RouteNamespace string
	Reference      *gatewayv1.BackendObjectReference
	Service        *corev1.Service
	EndpointSlices []discoveryv1.EndpointSlice
	Permitted      bool
}

type BackendResult struct {
	Reference   config.BackendRef
	Cluster     *config.Resource
	EndpointSet *config.Resource
	Diagnostic  *Diagnostic
}
