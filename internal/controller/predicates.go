package controller

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func gatewaySpecPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObject, oldOK := e.ObjectOld.(*gatewayv1.Gateway)
			newObject, newOK := e.ObjectNew.(*gatewayv1.Gateway)
			return oldOK && newOK && (!reflect.DeepEqual(oldObject.Spec, newObject.Spec) || !reflect.DeepEqual(oldObject.DeletionTimestamp, newObject.DeletionTimestamp))
		},
	}
}

func gatewayClassSpecPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObject, oldOK := e.ObjectOld.(*gatewayv1.GatewayClass)
			newObject, newOK := e.ObjectNew.(*gatewayv1.GatewayClass)
			return oldOK && newOK && !reflect.DeepEqual(oldObject.Spec, newObject.Spec)
		},
	}
}

func httpRouteSpecPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObject, oldOK := e.ObjectOld.(*gatewayv1.HTTPRoute)
			newObject, newOK := e.ObjectNew.(*gatewayv1.HTTPRoute)
			return oldOK && newOK && !reflect.DeepEqual(oldObject.Spec, newObject.Spec)
		},
	}
}

func serviceSpecPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObject, oldOK := e.ObjectOld.(*corev1.Service)
			newObject, newOK := e.ObjectNew.(*corev1.Service)
			return oldOK && newOK && !reflect.DeepEqual(oldObject.Spec, newObject.Spec)
		},
	}
}

func namespaceLabelsPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObject, oldOK := e.ObjectOld.(*corev1.Namespace)
			newObject, newOK := e.ObjectNew.(*corev1.Namespace)
			return oldOK && newOK && (!reflect.DeepEqual(oldObject.Labels, newObject.Labels) || !reflect.DeepEqual(oldObject.DeletionTimestamp, newObject.DeletionTimestamp))
		},
	}
}

func referenceGrantSpecPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObject, oldOK := e.ObjectOld.(*gatewayv1.ReferenceGrant)
			newObject, newOK := e.ObjectNew.(*gatewayv1.ReferenceGrant)
			return oldOK && newOK && !reflect.DeepEqual(oldObject.Spec, newObject.Spec)
		},
	}
}

func secretMetadataPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld != nil && e.ObjectNew != nil && e.ObjectOld.GetResourceVersion() != e.ObjectNew.GetResourceVersion()
		},
	}
}

func endpointSliceMembershipPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObject, oldOK := e.ObjectOld.(*discoveryv1.EndpointSlice)
			newObject, newOK := e.ObjectNew.(*discoveryv1.EndpointSlice)
			if !oldOK || !newOK {
				return false
			}
			return oldObject.Labels[EndpointSliceServiceLabel] != newObject.Labels[EndpointSliceServiceLabel] ||
				oldObject.AddressType != newObject.AddressType ||
				!reflect.DeepEqual(oldObject.OwnerReferences, newObject.OwnerReferences) ||
				!reflect.DeepEqual(oldObject.Ports, newObject.Ports) ||
				!reflect.DeepEqual(oldObject.Endpoints, newObject.Endpoints)
		},
	}
}
