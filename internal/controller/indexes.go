package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	GatewayClassIndex         = "praxis.gateway/class"
	HTTPRouteParentIndex      = "praxis.gateway/parent"
	HTTPRouteStatusIndex      = "praxis.gateway/status-parent"
	HTTPRouteBackendIndex     = "praxis.gateway/service-backend"
	GatewaySecretIndex        = "praxis.gateway/secret"
	EndpointSliceServiceIndex = "praxis.gateway/service"
	ReferenceGrantFromIndex   = "praxis.gateway/from"
	EndpointSliceServiceLabel = "kubernetes.io/service-name"
)

func RegisterIndexes(ctx context.Context, mgr manager.Manager, controllerName string) error {
	indexes := []struct {
		object client.Object
		name   string
		func_  client.IndexerFunc
	}{
		{&gatewayv1.Gateway{}, GatewayClassIndex, GatewayClassValues},
		{&gatewayv1.HTTPRoute{}, HTTPRouteParentIndex, HTTPRouteParentValues},
		{&gatewayv1.HTTPRoute{}, HTTPRouteStatusIndex, HTTPRouteStatusValues(controllerName)},
		{&gatewayv1.HTTPRoute{}, HTTPRouteBackendIndex, HTTPRouteBackendValues},
		{&gatewayv1.Gateway{}, GatewaySecretIndex, GatewaySecretValues},
		{&discoveryv1.EndpointSlice{}, EndpointSliceServiceIndex, EndpointSliceServiceValues},
		{&gatewayv1.ReferenceGrant{}, ReferenceGrantFromIndex, ReferenceGrantFromValues},
	}
	for _, index := range indexes {
		if err := mgr.GetFieldIndexer().IndexField(ctx, index.object, index.name, index.func_); err != nil {
			return fmt.Errorf("register %s index for %T: %w", index.name, index.object, err)
		}
	}
	return nil
}

func GatewayClassValues(obj client.Object) []string {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok || gateway.Spec.GatewayClassName == "" {
		return nil
	}
	return []string{string(gateway.Spec.GatewayClassName)}
}

func HTTPRouteParentValues(obj client.Object) []string {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(route.Spec.ParentRefs))
	for _, parent := range route.Spec.ParentRefs {
		if !isGatewayParent(parent) {
			continue
		}
		values = append(values, parentGatewayKey(parent, route.Namespace))
	}
	return unique(values)
}

func HTTPRouteStatusValues(controllerName string) client.IndexerFunc {
	return func(obj client.Object) []string {
		route, ok := obj.(*gatewayv1.HTTPRoute)
		if !ok {
			return nil
		}
		values := make([]string, 0, len(route.Status.Parents))
		for _, parent := range route.Status.Parents {
			if string(parent.ControllerName) != controllerName || !isGatewayParent(parent.ParentRef) {
				continue
			}
			values = append(values, parentGatewayKey(parent.ParentRef, route.Namespace))
		}
		return unique(values)
	}
}

func HTTPRouteBackendValues(obj client.Object) []string {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	var values []string
	for _, rule := range route.Spec.Rules {
		for _, backend := range rule.BackendRefs {
			ref := backend.BackendRef
			if !isServiceReference(ref.Group, ref.Kind) {
				continue
			}
			namespace := route.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}
			values = append(values, objectKey(namespace, string(ref.Name)))
		}
	}
	return unique(values)
}

func GatewaySecretValues(obj client.Object) []string {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}
	var values []string
	for _, listener := range gateway.Spec.Listeners {
		if listener.TLS == nil {
			continue
		}
		for _, ref := range listener.TLS.CertificateRefs {
			if !isSecretReference(ref.Group, ref.Kind) {
				continue
			}
			namespace := gateway.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}
			values = append(values, objectKey(namespace, string(ref.Name)))
		}
	}
	return unique(values)
}

func EndpointSliceServiceValues(obj client.Object) []string {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}
	name := slice.Labels[EndpointSliceServiceLabel]
	if name == "" {
		return nil
	}
	return []string{name}
}

func ReferenceGrantFromValues(obj client.Object) []string {
	grant, ok := obj.(*gatewayv1.ReferenceGrant)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(grant.Spec.From))
	for _, from := range grant.Spec.From {
		values = append(values, referenceGrantFromKey(string(from.Group), string(from.Kind), string(from.Namespace)))
	}
	return unique(values)
}

func listReferenceGrantsForSource(ctx context.Context, c client.Reader, group, kind, namespace string) ([]gatewayv1.ReferenceGrant, error) {
	var grants gatewayv1.ReferenceGrantList
	err := c.List(ctx, &grants, client.MatchingFields{ReferenceGrantFromIndex: referenceGrantFromKey(group, kind, namespace)})
	return grants.Items, err
}

func referenceGrantFromKey(group, kind, namespace string) string {
	return strings.Join([]string{group, kind, namespace}, "/")
}

func parentGatewayKey(parent gatewayv1.ParentReference, routeNamespace string) string {
	namespace := routeNamespace
	if parent.Namespace != nil {
		namespace = string(*parent.Namespace)
	}
	return objectKey(namespace, string(parent.Name))
}

func objectKey(namespace, name string) string {
	return namespace + "/" + name
}

func isGatewayParent(parent gatewayv1.ParentReference) bool {
	group := string(gatewayv1.GroupName)
	if parent.Group != nil {
		group = string(*parent.Group)
	}
	kind := "Gateway"
	if parent.Kind != nil {
		kind = string(*parent.Kind)
	}
	return group == string(gatewayv1.GroupName) && kind == "Gateway"
}

func isServiceReference(group *gatewayv1.Group, kind *gatewayv1.Kind) bool {
	groupName := ""
	if group != nil {
		groupName = string(*group)
	}
	kindName := "Service"
	if kind != nil {
		kindName = string(*kind)
	}
	return groupName == "" && kindName == "Service"
}

func isSecretReference(group *gatewayv1.Group, kind *gatewayv1.Kind) bool {
	groupName := ""
	if group != nil {
		groupName = string(*group)
	}
	kindName := "Secret"
	if kind != nil {
		kindName = string(*kind)
	}
	return groupName == "" && kindName == "Secret"
}

func unique(values []string) []string {
	if len(values) < 2 {
		return values
	}
	sort.Strings(values)
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func metadataSecretGVK(obj *metav1.PartialObjectMetadata) {
	obj.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
}
