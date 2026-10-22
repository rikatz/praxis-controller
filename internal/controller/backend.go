package controller

import (
	"context"
	"errors"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	"github.com/praxis-proxy/controller/internal/gateway"
)

type BackendReconciler struct {
	client.Client
	Configurator   delivery.Configurator
	ControllerName string
	OwnerEpoch     string
}

func (r *BackendReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	service := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, service); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{HTTPRouteBackendIndex: objectKey(service.Namespace, service.Name)}); err != nil {
		return reconcile.Result{}, err
	}
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices, client.InNamespace(service.Namespace), client.MatchingFields{EndpointSliceServiceIndex: service.Name}); err != nil {
		return reconcile.Result{}, err
	}

	grantsBySource := make(map[string][]gatewayv1.ReferenceGrant)
	gateways := make(map[types.NamespacedName]*gatewayv1.Gateway)
	for i := range routes.Items {
		route := &routes.Items[i]
		grants, exists := grantsBySource[route.Namespace]
		if !exists {
			var err error
			grants, err = listReferenceGrantsForSource(ctx, r.Client, string(gatewayv1.GroupName), "HTTPRoute", route.Namespace)
			if err != nil {
				return reconcile.Result{}, err
			}
			grantsBySource[route.Namespace] = grants
		}
		for _, parent := range route.Status.Parents {
			if string(parent.ControllerName) != r.ControllerName || !conditionCurrentTrue(parent.Conditions, string(gatewayv1.RouteConditionAccepted), route.Generation) {
				continue
			}
			namespace := route.Namespace
			if parent.ParentRef.Namespace != nil {
				namespace = string(*parent.ParentRef.Namespace)
			}
			gatewayKey := types.NamespacedName{Namespace: namespace, Name: string(parent.ParentRef.Name)}
			if _, exists := gateways[gatewayKey]; exists {
				continue
			}
			gatewayObject := &gatewayv1.Gateway{}
			if err := r.Get(ctx, gatewayKey, gatewayObject); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return reconcile.Result{}, err
			}
			if !conditionCurrentTrue(gatewayObject.Status.Conditions, string(gatewayv1.GatewayConditionAccepted), gatewayObject.Generation) {
				continue
			}
			class := &gatewayv1.GatewayClass{}
			if err := r.Get(ctx, client.ObjectKey{Name: string(gatewayObject.Spec.GatewayClassName)}, class); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return reconcile.Result{}, err
			}
			if string(class.Spec.ControllerName) != r.ControllerName || class.Spec.ParametersRef != nil ||
				!conditionCurrentTrue(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted), class.Generation) ||
				!conditionCurrentTrue(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion), class.Generation) {
				continue
			}
			gateways[gatewayKey] = gatewayObject
		}
	}

	gatewayKeys := make([]types.NamespacedName, 0, len(gateways))
	for key := range gateways {
		gatewayKeys = append(gatewayKeys, key)
	}
	sort.Slice(gatewayKeys, func(i, j int) bool {
		if gatewayKeys[i].Namespace != gatewayKeys[j].Namespace {
			return gatewayKeys[i].Namespace < gatewayKeys[j].Namespace
		}
		return gatewayKeys[i].Name < gatewayKeys[j].Name
	})
	for _, gatewayKey := range gatewayKeys {
		gatewayObject := gateways[gatewayKey]
		desired, err := r.endpointSetsForGateway(ctx, gatewayObject, routes.Items, *service, slices.Items, grantsBySource)
		if err != nil {
			return reconcile.Result{}, err
		}
		if len(desired) == 0 {
			continue
		}
		result, err := r.applyEndpointSets(ctx, targetForGateway(gatewayObject), r.OwnerEpoch, desired)
		if err != nil {
			return reconcile.Result{}, err
		}
		if result.pending {
			return reconcile.Result{RequeueAfter: deliveryRecheck}, nil
		}
		if result.changed {
			ctrl.LoggerFrom(ctx).V(1).Info("Updated backend endpoint membership", "Gateway", gatewayKey, "simulated", result.simulated)
		}
	}
	return reconcile.Result{}, nil
}

func (r *BackendReconciler) endpointSetsForGateway(ctx context.Context, gatewayObject *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute, service corev1.Service, slices []discoveryv1.EndpointSlice, grantsBySource map[string][]gatewayv1.ReferenceGrant) ([]config.Resource, error) {
	resources := make(map[config.Key]config.Resource)
	for i := range routes {
		route := &routes[i]
		attached := false
		for _, parent := range route.Status.Parents {
			if string(parent.ControllerName) == r.ControllerName && parentRefTargetsGateway(parent.ParentRef, route.Namespace, gatewayObject) && conditionCurrentTrue(parent.Conditions, string(gatewayv1.RouteConditionAccepted), route.Generation) {
				attached = true
				break
			}
		}
		if !attached {
			continue
		}
		grants := grantsBySource[route.Namespace]
		for _, rule := range route.Spec.Rules {
			for _, backend := range gateway.HTTPRouteBackendRefsForEndpointSets(rule) {
				ref := backend.BackendRef
				if !isServiceReference(ref.Group, ref.Kind) {
					continue
				}
				namespace := route.Namespace
				if ref.Namespace != nil {
					namespace = string(*ref.Namespace)
				}
				if namespace != service.Namespace || string(ref.Name) != service.Name || !referenceAllowed(grants, string(gatewayv1.GroupName), "HTTPRoute", route.Namespace, "", "Service", namespace, string(ref.Name)) || ref.Port == nil {
					continue
				}
				servicePort, found := serviceTCPPortByNumber(service.Spec.Ports, int32(*ref.Port))
				if !found {
					continue
				}
				resource, err := gateway.ResolveEndpointSet(targetForGateway(gatewayObject), r.OwnerEpoch, service, servicePort, slices)
				if err != nil {
					return nil, errors.New("resolve backend endpoint membership failed")
				}
				resources[resource.Key] = resource
			}
		}
	}
	keys := make([]config.Key, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	result := make([]config.Resource, 0, len(keys))
	for _, key := range keys {
		result = append(result, resources[key])
	}
	return result, nil
}

func (r *BackendReconciler) applyEndpointSets(ctx context.Context, target config.Target, ownerEpoch string, desired []config.Resource) (endpointApplyResult, error) {
	observation, err := r.Configurator.Observe(ctx, target)
	if err != nil {
		return endpointApplyResult{}, errors.New("observe backend endpoint configuration failed")
	}
	filtered := observation
	filtered.Resources = nil
	wanted := make(map[config.Key]struct{}, len(desired))
	for _, resource := range desired {
		if resource.Key.Kind != config.KindEndpointSet {
			return endpointApplyResult{}, errors.New("backend resolver returned a non-endpoint resource")
		}
		wanted[resource.Key] = struct{}{}
	}
	for _, result := range observation.Resources {
		if _, ok := wanted[result.Key]; ok {
			filtered.Resources = append(filtered.Resources, result)
		}
	}
	changes, err := delivery.Diff(target, ownerEpoch, config.DomainPraxis, desired, filtered)
	if err != nil {
		return endpointApplyResult{}, errors.New("calculate backend endpoint changes failed")
	}
	if len(changes.Upserts) == 0 && len(changes.Deletes) == 0 {
		return endpointApplyResult{pending: !observation.Connected, simulated: observation.Simulated}, nil
	}
	receipt, applyErr := r.Configurator.Apply(ctx, changes)
	if applyErr != nil {
		updated, observeErr := r.Configurator.Observe(ctx, target)
		if observeErr == nil {
			observation = updated
		}
		return endpointApplyResult{pending: true, simulated: observation.Simulated}, nil
	}
	if delivery.ValidateReceipt(changes, receipt) != nil {
		return endpointApplyResult{}, errors.New("backend endpoint receipt did not match the requested changes")
	}
	updated, err := r.Configurator.Observe(ctx, target)
	if err != nil {
		return endpointApplyResult{}, errors.New("observe applied backend endpoint configuration failed")
	}
	active := allResourcesActive(target, desired, &receipt, updated)
	return endpointApplyResult{
		pending:   !active,
		changed:   true,
		simulated: receipt.Simulated || updated.Simulated,
	}, nil
}

type endpointApplyResult struct {
	pending   bool
	changed   bool
	simulated bool
}

func serviceTCPPortByNumber(ports []corev1.ServicePort, port int32) (corev1.ServicePort, bool) {
	var selected corev1.ServicePort
	found := false
	for _, candidate := range ports {
		if candidate.Port != port {
			continue
		}
		protocol := candidate.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		if protocol != corev1.ProtocolTCP {
			continue
		}
		if found {
			return corev1.ServicePort{}, false
		}
		selected = candidate
		found = true
	}
	if !found {
		return corev1.ServicePort{}, false
	}
	return selected, true
}
