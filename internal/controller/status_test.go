package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	"github.com/praxis-proxy/controller/internal/gateway"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestMergeGatewayStatusProgramsOnlyAcceptedUnconflictedListeners(t *testing.T) {
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Generation: 7}}
	gateway.Status.Conditions = []metav1.Condition{{Type: "OtherControllerCondition", Status: metav1.ConditionTrue, Reason: "Kept", Message: "preserve"}}
	accepted := gatewayv1.ListenerStatus{Name: "accepted", Conditions: []metav1.Condition{
		{Type: string(gatewayv1.ListenerConditionAccepted), Status: metav1.ConditionTrue, ObservedGeneration: 7, Reason: "Accepted"},
		{Type: string(gatewayv1.ListenerConditionConflicted), Status: metav1.ConditionFalse, ObservedGeneration: 7, Reason: "NoConflicts"},
		{Type: string(gatewayv1.ListenerConditionProgrammed), Status: metav1.ConditionFalse, ObservedGeneration: 7, Reason: "Pending"},
		{Type: "ListenerOtherCondition", Status: metav1.ConditionTrue, Reason: "Kept"},
	}}
	rejected := gatewayv1.ListenerStatus{Name: "rejected", Conditions: []metav1.Condition{
		{Type: string(gatewayv1.ListenerConditionAccepted), Status: metav1.ConditionFalse, ObservedGeneration: 7, Reason: "UnsupportedValue"},
		{Type: string(gatewayv1.ListenerConditionConflicted), Status: metav1.ConditionTrue, ObservedGeneration: 7, Reason: "HostnameConflict"},
		{Type: string(gatewayv1.ListenerConditionProgrammed), Status: metav1.ConditionFalse, ObservedGeneration: 7, Reason: "Invalid"},
	}}
	gateway.Spec.Listeners = []gatewayv1.Listener{{Name: accepted.Name}, {Name: rejected.Name}}
	status := mergeGatewayStatus(gateway, gatewayv1.GatewayStatus{Listeners: []gatewayv1.ListenerStatus{accepted, rejected}}, true, true)

	if !conditionIs(status.Conditions, "OtherControllerCondition", metav1.ConditionTrue, 0) {
		t.Fatal("Gateway conditions owned by another controller were not preserved")
	}
	if condition := findListenerCondition(status.Listeners, accepted.Name, string(gatewayv1.ListenerConditionProgrammed)); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("accepted listener Programmed condition = %#v, want True", condition)
	}
	if condition := findListenerCondition(status.Listeners, rejected.Name, string(gatewayv1.ListenerConditionProgrammed)); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("rejected listener Programmed condition = %#v, want False", condition)
	}
	if condition := findListenerCondition(status.Listeners, accepted.Name, "ListenerOtherCondition"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("unmanaged listener condition = %#v, want preserved True", condition)
	}
	if !conditionIs(status.Conditions, string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionTrue, 7) {
		t.Fatal("Gateway Programmed should aggregate active accepted listeners")
	}
}

func TestMergeGatewayStatusClearsStaleAddressesAndListeners(t *testing.T) {
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "192.0.2.10"}}
	gateway.Status.Listeners = []gatewayv1.ListenerStatus{{Name: "old", AttachedRoutes: 4}}

	status := mergeGatewayStatus(gateway, unsupportedClassStatus(gateway), false, false)
	if len(status.Addresses) != 0 {
		t.Fatalf("unsupported Gateway retained addresses: %#v", status.Addresses)
	}
	if len(status.Listeners) != 0 {
		t.Fatalf("unsupported Gateway retained listener status: %#v", status.Listeners)
	}
}

func TestGatewayStatusAddressesInferAddressTypeAndDeduplicate(t *testing.T) {
	addresses := gatewayStatusAddresses([]string{"192.0.2.10", "edge.example.test", "192.0.2.10"})
	if len(addresses) != 2 {
		t.Fatalf("mapped address count = %d, want 2", len(addresses))
	}
	if addresses[0].Value != "192.0.2.10" || addresses[0].Type == nil || *addresses[0].Type != gatewayv1.IPAddressType {
		t.Fatalf("mapped IP address = %#v", addresses[0])
	}
	if addresses[1].Value != "edge.example.test" || addresses[1].Type == nil || *addresses[1].Type != gatewayv1.HostnameAddressType {
		t.Fatalf("mapped hostname address = %#v", addresses[1])
	}
}

func TestEndpointSliceMembershipPredicateObservesOwnerReferenceChanges(t *testing.T) {
	oldSlice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Namespace: "apps",
		Labels:    map[string]string{EndpointSliceServiceLabel: "backend"},
	}}
	newSlice := oldSlice.DeepCopy()
	newSlice.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: "backend", UID: "new-service-uid"}}
	if !endpointSliceMembershipPredicate().Update(event.UpdateEvent{ObjectOld: oldSlice, ObjectNew: newSlice}) {
		t.Fatal("EndpointSlice owner reference change did not trigger backend reconciliation")
	}
}

func TestBackendReconcilerRequeuesWhenMatchingObservationIsDisconnected(t *testing.T) {
	target := config.Target{Namespace: "apps", Name: "edge", UID: "gateway-uid"}
	service := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "backend", UID: "service-uid"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP}}},
	}
	resource, err := gateway.ResolveEndpointSet(target, "test-owner-epoch", service, service.Spec.Ports[0], nil)
	if err != nil {
		t.Fatalf("resolve endpoint set: %v", err)
	}
	configurator := newSimulatedConfigurator()
	configurator.observed[target.ID()] = delivery.Observation{
		Target:    target,
		Connected: false,
		Simulated: true,
		Resources: []delivery.ResourceResult{{
			Key: resource.Key, DesiredRevision: resource.Revision, ActiveRevision: resource.Revision, State: delivery.ApplyActive,
		}},
	}
	reconciler := BackendReconciler{Configurator: configurator}
	result, err := reconciler.applyEndpointSets(context.Background(), target, "test-owner-epoch", []config.Resource{resource})
	if err != nil {
		t.Fatalf("apply endpoint sets: %v", err)
	}
	if !result.pending {
		t.Fatal("disconnected matching observation did not request a retry")
	}
	if configurator.appliedCount() != 0 {
		t.Fatal("disconnected no-op observation caused an unnecessary Apply")
	}
}

func TestSimulatedReceiptMustCoverEveryResource(t *testing.T) {
	target := config.Target{Namespace: "ns", Name: "gw", UID: "gateway-uid"}
	first := testListenerResource(t, target, "listener-a")
	second := testListenerResource(t, target, "listener-b")
	receipt := delivery.Receipt{Target: target, Simulated: true, Resources: []delivery.ResourceResult{
		{Key: first.Key, DesiredRevision: first.Revision, ActiveRevision: first.Revision, State: delivery.ApplyActive},
	}}
	if simulatedReceiptReady(&receipt, []config.Resource{first, second}, target) {
		t.Fatal("receipt with one missing resource was accepted")
	}
	receipt.Resources = append(receipt.Resources, delivery.ResourceResult{
		Key: second.Key, DesiredRevision: second.Revision, ActiveRevision: second.Revision, State: delivery.ApplyActive,
	})
	if !simulatedReceiptReady(&receipt, []config.Resource{first, second}, target) {
		t.Fatal("complete matching simulated receipt was rejected")
	}
}

func TestAllResourcesActiveRequiresMatchingObservationAndReceipt(t *testing.T) {
	target := config.Target{Namespace: "ns", Name: "gw", UID: "gateway-uid"}
	resource := testListenerResource(t, target, "listener")
	result := delivery.ResourceResult{Key: resource.Key, DesiredRevision: resource.Revision, ActiveRevision: resource.Revision, State: delivery.ApplyActive}
	observation := delivery.Observation{Target: target, Connected: true, Resources: []delivery.ResourceResult{result}}
	if !allResourcesActive(target, []config.Resource{resource}, nil, observation) {
		t.Fatal("matching active observation was rejected")
	}
	observation.Target.UID = "replacement"
	if allResourcesActive(target, []config.Resource{resource}, nil, observation) {
		t.Fatal("observation for a replaced Gateway UID was accepted")
	}
	receipt := delivery.Receipt{Target: target, Resources: []delivery.ResourceResult{result}}
	if !allResourcesActive(target, []config.Resource{resource}, &receipt, delivery.Observation{}) {
		t.Fatal("matching explicit receipt was rejected")
	}
	receipt.Target.UID = "replacement"
	if allResourcesActive(target, []config.Resource{resource}, &receipt, delivery.Observation{}) {
		t.Fatal("receipt for a replaced Gateway UID was accepted")
	}
}

func TestServiceTCPPortByNumber(t *testing.T) {
	ports := []corev1.ServicePort{
		{Name: "udp", Port: 8080, Protocol: corev1.ProtocolUDP},
		{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP},
	}
	selected, ok := serviceTCPPortByNumber(ports, 8080)
	if !ok || selected.Name != "http" {
		t.Fatalf("selected port = %#v, %t; want TCP port", selected, ok)
	}
	if _, ok := serviceTCPPortByNumber([]corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolUDP}}, 8080); ok {
		t.Fatal("UDP-only port was accepted for HTTP backend")
	}
	if _, ok := serviceTCPPortByNumber([]corev1.ServicePort{{Port: 8080}, {Port: 8080, Protocol: corev1.ProtocolTCP}}, 8080); ok {
		t.Fatal("ambiguous TCP port was accepted")
	}
}

func TestReferenceAllowedRequiresGrantForCrossNamespaceReference(t *testing.T) {
	group := gatewayv1.Group(gatewayv1.GroupName)
	kind := gatewayv1.Kind("Service")
	name := gatewayv1.ObjectName("backend")
	grant := gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "target"},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: group, Kind: "HTTPRoute", Namespace: "source"}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: kind, Name: &name}},
		},
	}
	if !referenceAllowed([]gatewayv1.ReferenceGrant{grant}, string(gatewayv1.GroupName), "HTTPRoute", "source", "", "Service", "target", "backend") {
		t.Fatal("matching ReferenceGrant was rejected")
	}
	if referenceAllowed([]gatewayv1.ReferenceGrant{grant}, string(gatewayv1.GroupName), "HTTPRoute", "other", "", "Service", "target", "backend") {
		t.Fatal("ReferenceGrant from another namespace was accepted")
	}
}

func findListenerCondition(listeners []gatewayv1.ListenerStatus, name gatewayv1.SectionName, conditionType string) *metav1.Condition {
	for i := range listeners {
		if listeners[i].Name != name {
			continue
		}
		return findCondition(listeners[i].Conditions, conditionType)
	}
	return nil
}

func testListenerResource(t *testing.T, target config.Target, name string) config.Resource {
	t.Helper()
	resource, err := config.NewResource(
		config.Key{Domain: config.DomainPraxis, Target: target, OwnerEpoch: "test-owner-epoch", Kind: config.KindListener, Name: name},
		config.ObjectRef{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway", Namespace: target.Namespace, Name: target.Name, UID: target.UID},
		config.Listener{Port: 8080, Protocol: config.ListenerHTTP},
	)
	if err != nil {
		t.Fatalf("create listener resource: %v", err)
	}
	return resource
}

func TestDeliveredRevisionDoesNotSatisfyProgrammed(t *testing.T) {
	target := config.Target{Namespace: "edge", Name: "public", UID: "gateway-uid"}
	resource := testListenerResource(t, target, "web")
	observation := delivery.Observation{Target: target, Connected: true, Resources: []delivery.ResourceResult{{
		Key: resource.Key, DesiredRevision: resource.Revision, DeliveredRevision: resource.Revision, State: delivery.ApplyDelivered,
	}}}
	if allResourcesActive(target, []config.Resource{resource}, nil, observation) {
		t.Fatal("delivered revision must not satisfy Programmed")
	}
}
