package controller

import (
	"net"
	"sort"
	"strings"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
)

func mergeGatewayStatus(gateway *gatewayv1.Gateway, desired gatewayv1.GatewayStatus, programmed, simulated bool) gatewayv1.GatewayStatus {
	status := *gateway.Status.DeepCopy()
	status.Addresses = append([]gatewayv1.GatewayStatusAddress(nil), desired.Addresses...)
	for _, condition := range desired.Conditions {
		if condition.Type == string(gatewayv1.GatewayConditionProgrammed) {
			continue
		}
		condition.ObservedGeneration = gateway.Generation
		apiMeta.SetStatusCondition(&status.Conditions, condition)
	}
	setProgrammed(&status.Conditions, gateway.Generation, programmed, simulated)

	old := make(map[gatewayv1.SectionName]gatewayv1.ListenerStatus, len(status.Listeners))
	for _, listener := range status.Listeners {
		old[listener.Name] = listener
	}
	listeners := make([]gatewayv1.ListenerStatus, 0, len(desired.Listeners))
	seen := make(map[gatewayv1.SectionName]struct{}, len(desired.Listeners))
	for _, desiredListener := range desired.Listeners {
		listener := desiredListener
		if previous, ok := old[listener.Name]; ok {
			listener.Conditions = mergeConditions(previous.Conditions, listener.Conditions, gateway.Generation, "")
		}
		if conditionCurrentTrue(listener.Conditions, string(gatewayv1.ListenerConditionAccepted), gateway.Generation) && !conditionCurrentTrue(listener.Conditions, string(gatewayv1.ListenerConditionConflicted), gateway.Generation) {
			setProgrammed(&listener.Conditions, gateway.Generation, programmed, simulated)
		}
		listeners = append(listeners, listener)
		seen[listener.Name] = struct{}{}
	}
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].Name < listeners[j].Name })
	status.Listeners = listeners
	return status
}

func mergeConditions(current, desired []metav1.Condition, generation int64, skipType string) []metav1.Condition {
	conditions := append([]metav1.Condition(nil), current...)
	for _, condition := range desired {
		if condition.Type == skipType {
			continue
		}
		condition.ObservedGeneration = generation
		apiMeta.SetStatusCondition(&conditions, condition)
	}
	return conditions
}

func setProgrammed(conditions *[]metav1.Condition, generation int64, programmed, simulated bool) {
	condition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		ObservedGeneration: generation,
	}
	if programmed {
		condition.Status = metav1.ConditionTrue
		condition.Reason = string(gatewayv1.GatewayReasonProgrammed)
		condition.Message = "Configuration is active"
		if simulated {
			condition.Message = "Mock configuration is active"
		}
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = string(gatewayv1.GatewayReasonPending)
		condition.Message = "Configuration is not active"
	}
	apiMeta.SetStatusCondition(conditions, condition)
}

func unsupportedClassStatus(gateway *gatewayv1.Gateway) gatewayv1.GatewayStatus {
	status := gatewayv1.GatewayStatus{}
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionFalse,
		Reason:             string(gatewayv1.GatewayReasonInvalidParameters),
		Message:            "GatewayClass parametersRef is not supported",
		ObservedGeneration: gateway.Generation,
	})
	return status
}

func unsupportedVersionGatewayStatus(gateway *gatewayv1.Gateway, message string) gatewayv1.GatewayStatus {
	status := gatewayv1.GatewayStatus{}
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionFalse,
		Reason:             "UnsupportedVersion",
		Message:            message,
		ObservedGeneration: gateway.Generation,
	})
	return status
}

func gatewayStatusAddresses(addresses []string) []gatewayv1.GatewayStatusAddress {
	result := make([]gatewayv1.GatewayStatusAddress, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		type_ := gatewayv1.HostnameAddressType
		if net.ParseIP(address) != nil {
			type_ = gatewayv1.IPAddressType
		}
		key := string(type_) + "/" + address
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		addressType := type_
		result = append(result, gatewayv1.GatewayStatusAddress{Type: &addressType, Value: address})
	}
	return result
}

func mergeRouteParents(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, controllerName string, desired *gatewayv1.HTTPRouteStatus) []gatewayv1.RouteParentStatus {
	var parents []gatewayv1.RouteParentStatus
	replace := desired != nil
	for _, parent := range route.Status.Parents {
		owned := string(parent.ControllerName) == controllerName && parentRefTargetsGateway(parent.ParentRef, route.Namespace, gateway)
		if owned && (!hasGatewayParent(route, gateway) || replace) {
			continue
		}
		parents = append(parents, parent)
	}
	if desired != nil {
		for _, parent := range desired.Parents {
			if string(parent.ControllerName) != controllerName || !parentRefTargetsGateway(parent.ParentRef, route.Namespace, gateway) {
				continue
			}
			for i := range parent.Conditions {
				parent.Conditions[i].ObservedGeneration = route.Generation
			}
			parents = append(parents, parent)
		}
	}
	return parents
}

func hasGatewayParent(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) bool {
	for _, parent := range route.Spec.ParentRefs {
		if parentRefTargetsGateway(parent, route.Namespace, gateway) {
			return true
		}
	}
	return false
}

func parentRefTargetsGateway(parent gatewayv1.ParentReference, routeNamespace string, gateway *gatewayv1.Gateway) bool {
	return isGatewayParent(parent) && parentGatewayKey(parent, routeNamespace) == objectKey(gateway.Namespace, gateway.Name)
}

func allResourcesActive(target config.Target, resources []config.Resource, receipt *delivery.Receipt, observation delivery.Observation) bool {
	if len(resources) == 0 {
		return false
	}
	if receipt != nil && receipt.Target != target {
		return false
	}
	if observation.Connected && observation.Target != target {
		return false
	}
	for _, resource := range resources {
		if receipt != nil {
			if revision, ok := receipt.ActiveRevision(resource.Key); ok && revision == resource.Revision {
				continue
			}
		}
		if observation.Connected {
			if revision, ok := observation.ActiveRevision(resource.Key); ok && revision == resource.Revision {
				continue
			}
		}
		return false
	}
	return true
}
