package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/config"
)

func ResolveBackend(input BackendInput) BackendResult {
	ref := config.BackendRef{Weight: 1}
	fail := func(reason, message string) BackendResult {
		ref.Valid = false
		ref.Reason = reason
		return BackendResult{
			Reference: ref,
			Diagnostic: &Diagnostic{
				Object:    backendObjectName(input),
				Condition: string(gatewayv1.RouteConditionResolvedRefs),
				Reason:    reason,
				Message:   message,
			},
		}
	}
	if input.Reference == nil {
		return fail(string(gatewayv1.RouteReasonInvalidKind), "backend reference is missing")
	}

	backend := input.Reference
	group := ""
	if backend.Group != nil {
		group = string(*backend.Group)
	}
	kind := "Service"
	if backend.Kind != nil {
		kind = string(*backend.Kind)
	}
	if group != "" || kind != "Service" {
		return fail(string(gatewayv1.RouteReasonInvalidKind), "HTTPRoute backends must reference core Services")
	}
	if backend.Port == nil {
		return fail(string(gatewayv1.RouteReasonBackendNotFound), "Service backend port is required")
	}

	namespace := input.RouteNamespace
	if backend.Namespace != nil {
		namespace = string(*backend.Namespace)
	}
	name := string(backend.Name)
	if namespace != input.RouteNamespace && !input.Permitted {
		return fail(string(gatewayv1.RouteReasonRefNotPermitted), "cross-namespace Service reference is not permitted")
	}
	if input.Service == nil || input.Service.Namespace != namespace || input.Service.Name != name {
		return fail(string(gatewayv1.RouteReasonBackendNotFound), "referenced Service does not exist")
	}

	var selected *corev1.ServicePort
	hasNonTCPPort := false
	for i := range input.Service.Spec.Ports {
		port := &input.Service.Spec.Ports[i]
		if port.Port != int32(*backend.Port) {
			continue
		}
		if effectiveProtocol(port.Protocol) != corev1.ProtocolTCP {
			hasNonTCPPort = true
			continue
		}
		if selected != nil {
			return fail(string(gatewayv1.RouteReasonBackendNotFound), "Service backend port is ambiguous")
		}
		selected = port
	}
	if selected == nil {
		if hasNonTCPPort {
			return fail(string(gatewayv1.RouteReasonUnsupportedProtocol), "HTTPRoute backend Service port is not TCP")
		}
		return fail(string(gatewayv1.RouteReasonBackendNotFound), "Service TCP port does not exist")
	}

	endpoints, err := ResolveEndpointSet(input.Target, input.OwnerEpoch, *input.Service, *selected, input.EndpointSlices)
	if err != nil {
		return fail(string(gatewayv1.RouteReasonBackendNotFound), "Service backend could not be compiled")
	}
	clusterName := backendResourceName(*input.Service, *selected)
	cluster, err := config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: input.Target, OwnerEpoch: input.OwnerEpoch, Kind: config.KindCluster, Name: clusterName},
		serviceObjectRef(*input.Service),
		config.Cluster{
			ServiceNamespace: input.Service.Namespace,
			ServiceName:      input.Service.Name,
			ServiceUID:       string(input.Service.UID),
			Port:             selected.Port,
			Protocol:         string(corev1.ProtocolTCP),
			EndpointSet:      endpoints.Key.Name,
		},
	)
	if err != nil {
		return fail(string(gatewayv1.RouteReasonBackendNotFound), "Service backend could not be compiled")
	}

	ref.Cluster = clusterName
	ref.Valid = true
	ref.Reason = ""
	return BackendResult{Reference: ref, Cluster: &cluster, EndpointSet: &endpoints}
}

func ResolveEndpointSet(target config.Target, ownerEpoch string, service corev1.Service, servicePort corev1.ServicePort, slices []discoveryv1.EndpointSlice) (config.Resource, error) {
	protocol := effectiveProtocol(servicePort.Protocol)
	if target.Namespace == "" || target.Name == "" || target.UID == "" || ownerEpoch == "" {
		return config.Resource{}, fmt.Errorf("target and owner epoch are required")
	}
	if service.Namespace == "" || service.Name == "" || service.UID == "" {
		return config.Resource{}, fmt.Errorf("Service namespace, name and UID are required")
	}
	if servicePort.Port < 1 || servicePort.Port > 65535 {
		return config.Resource{}, fmt.Errorf("Service port must be between 1 and 65535")
	}

	value := config.EndpointSet{
		ServiceNamespace: service.Namespace,
		ServiceName:      service.Name,
		ServiceUID:       string(service.UID),
		Port:             servicePort.Port,
		Protocol:         string(protocol),
		Endpoints:        collectEndpoints(service, servicePort, slices),
	}
	name := backendResourceName(service, servicePort)
	return config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: ownerEpoch, Kind: config.KindEndpointSet, Name: name},
		serviceObjectRef(service),
		value,
	)
}

func backendResourceName(service corev1.Service, port corev1.ServicePort) string {
	protocol := effectiveProtocol(port.Protocol)
	identity := strings.Join([]string{string(service.UID), strconv.Itoa(int(port.Port)), string(protocol)}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("service-%d-%s-%s", port.Port, strings.ToLower(string(protocol)), hex.EncodeToString(sum[:16]))
}

func serviceObjectRef(service corev1.Service) config.ObjectRef {
	return config.ObjectRef{
		APIVersion: "v1",
		Kind:       "Service",
		Namespace:  service.Namespace,
		Name:       service.Name,
		UID:        string(service.UID),
		Generation: service.Generation,
	}
}

func effectiveProtocol(protocol corev1.Protocol) corev1.Protocol {
	if protocol == "" {
		return corev1.ProtocolTCP
	}
	return protocol
}

func effectiveEndpointProtocol(protocol *corev1.Protocol) corev1.Protocol {
	if protocol == nil {
		return corev1.ProtocolTCP
	}
	return effectiveProtocol(*protocol)
}

func collectEndpoints(service corev1.Service, servicePort corev1.ServicePort, slices []discoveryv1.EndpointSlice) []config.Endpoint {
	protocol := effectiveProtocol(servicePort.Protocol)
	type endpointKey struct {
		address string
		port    int32
	}
	byAddress := make(map[endpointKey]config.Endpoint)
	for _, slice := range slices {
		if slice.Namespace != service.Namespace || slice.Labels[discoveryv1.LabelServiceName] != service.Name || staleServiceOwner(slice, service) {
			continue
		}
		port, ok := endpointPort(slice, servicePort, protocol)
		if !ok {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			ready := conditionValue(endpoint.Conditions.Ready, true)
			if service.Spec.PublishNotReadyAddresses {
				ready = true
			}
			serving := conditionValue(endpoint.Conditions.Serving, ready)
			terminating := conditionValue(endpoint.Conditions.Terminating, false)
			for _, address := range endpoint.Addresses {
				canonical, ok := canonicalAddress(address, slice.AddressType)
				if !ok {
					continue
				}
				key := endpointKey{address: canonical, port: *port}
				candidate := config.Endpoint{Address: canonical, Port: *port, Ready: ready, Serving: serving, Terminating: terminating}
				if current, exists := byAddress[key]; exists {
					// Conflicting duplicate slice entries fail closed for readiness.
					candidate.Ready = current.Ready && candidate.Ready
					candidate.Serving = current.Serving && candidate.Serving
					candidate.Terminating = current.Terminating || candidate.Terminating
				}
				byAddress[key] = candidate
			}
		}
	}

	endpoints := make([]config.Endpoint, 0, len(byAddress))
	for _, endpoint := range byAddress {
		endpoints = append(endpoints, endpoint)
	}
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Address != endpoints[j].Address {
			return endpoints[i].Address < endpoints[j].Address
		}
		if endpoints[i].Port != endpoints[j].Port {
			return endpoints[i].Port < endpoints[j].Port
		}
		return false
	})
	return endpoints
}

func endpointPort(slice discoveryv1.EndpointSlice, servicePort corev1.ServicePort, protocol corev1.Protocol) (*int32, bool) {
	var match *int32
	for _, port := range slice.Ports {
		name := ""
		if port.Name != nil {
			name = *port.Name
		}
		if name != servicePort.Name || effectiveEndpointProtocol(port.Protocol) != protocol || port.Port == nil || *port.Port < 1 || *port.Port > 65535 {
			continue
		}
		if match != nil && *match != *port.Port {
			return nil, false
		}
		match = port.Port
	}
	return match, match != nil
}

func staleServiceOwner(slice discoveryv1.EndpointSlice, service corev1.Service) bool {
	for _, owner := range slice.OwnerReferences {
		if owner.Kind != "Service" {
			continue
		}
		if owner.APIVersion != "v1" || owner.Name != service.Name || owner.UID == "" || owner.UID != service.UID {
			return true
		}
	}
	return false
}

func canonicalAddress(address string, addressType discoveryv1.AddressType) (string, bool) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", false
	}
	switch addressType {
	case discoveryv1.AddressTypeIPv4:
		if ip.To4() == nil {
			return "", false
		}
		return ip.To4().String(), true
	case discoveryv1.AddressTypeIPv6:
		if ip.To4() != nil || ip.To16() == nil {
			return "", false
		}
		return ip.String(), true
	default:
		return "", false
	}
}

func conditionValue(value *bool, defaultValue bool) bool {
	if value == nil {
		return defaultValue
	}
	return *value
}

func backendObjectName(input BackendInput) types.NamespacedName {
	if input.Reference == nil {
		return types.NamespacedName{Namespace: input.RouteNamespace}
	}
	namespace := input.RouteNamespace
	if input.Reference.Namespace != nil {
		namespace = string(*input.Reference.Namespace)
	}
	return types.NamespacedName{Namespace: namespace, Name: string(input.Reference.Name)}
}
