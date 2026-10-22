package controller

import (
	"context"
	"errors"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	"github.com/praxis-proxy/controller/internal/gateway"
)

const (
	GatewayFinalizer = "gateway.networking.k8s.io/praxis-proxy-cleanup"
	deliveryRecheck  = 5 * time.Second
)

type Reconciler struct {
	client.Client
	APIReader      client.Reader
	Configurator   delivery.Configurator
	Provisioner    delivery.Provisioner
	ControllerName string
	OwnerEpoch     string
}

type ClassReconciler struct {
	client.Client
	APIReader      client.Reader
	ControllerName string
}

func SetupWithManager(mgr manager.Manager, configurator delivery.Configurator, provisioner delivery.Provisioner, controllerName, ownerEpoch string) error {
	if configurator == nil || provisioner == nil || controllerName == "" || ownerEpoch == "" {
		return errors.New("configurator, provisioner, controller name and owner epoch are required")
	}
	if err := RegisterIndexes(context.Background(), mgr, controllerName); err != nil {
		return err
	}

	r := &Reconciler{
		Client:         mgr.GetClient(),
		APIReader:      mgr.GetAPIReader(),
		Configurator:   configurator,
		Provisioner:    provisioner,
		ControllerName: controllerName,
		OwnerEpoch:     ownerEpoch,
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GatewayClass{}, builder.WithPredicates(gatewayClassSpecPredicate())).
		Named("gatewayclass").
		Complete(&ClassReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), ControllerName: controllerName}); err != nil {
		return err
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}, builder.WithPredicates(gatewaySpecPredicate())).
		Watches(&gatewayv1.GatewayClass{}, handler.EnqueueRequestsFromMapFunc(r.mapGatewayClass), builder.WithPredicates(gatewayClassSpecPredicate())).
		Watches(&gatewayv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(r.mapHTTPRoute), builder.WithPredicates(httpRouteSpecPredicate())).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespace), builder.WithPredicates(namespaceLabelsPredicate())).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.mapService), builder.WithPredicates(serviceSpecPredicate())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecret), builder.WithPredicates(secretMetadataPredicate()), builder.OnlyMetadata).
		Watches(&gatewayv1.ReferenceGrant{}, handler.EnqueueRequestsFromMapFunc(r.mapReferenceGrant), builder.WithPredicates(referenceGrantSpecPredicate())).
		Named("gateway").
		Complete(r); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(mapEndpointSliceToService), builder.WithPredicates(endpointSliceMembershipPredicate())).
		Named("backend-membership").
		Complete(&BackendReconciler{Client: mgr.GetClient(), Configurator: configurator, ControllerName: controllerName, OwnerEpoch: ownerEpoch})
}

func (r *ClassReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	class := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if string(class.Spec.ControllerName) != r.ControllerName {
		return reconcile.Result{}, nil
	}
	versionSupported, versionMessage, err := gatewayAPICRDBundleSupported(ctx, r.APIReader)
	if err != nil {
		return reconcile.Result{}, err
	}
	original := class.DeepCopy()
	accepted := metav1.ConditionTrue
	reason := string(gatewayv1.GatewayClassReasonAccepted)
	message := "GatewayClass is managed by this controller"
	if class.Spec.ParametersRef != nil {
		accepted = metav1.ConditionFalse
		reason = string(gatewayv1.GatewayClassReasonInvalidParameters)
		message = "GatewayClass parametersRef is not supported"
	} else if !versionSupported {
		accepted = metav1.ConditionFalse
		reason = "UnsupportedVersions"
		message = versionMessage
	}
	apiMeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:             accepted,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: class.Generation,
	})
	versionStatus := metav1.ConditionTrue
	versionReason := string(gatewayv1.GatewayClassReasonSupportedVersion)
	if !versionSupported {
		versionStatus = metav1.ConditionFalse
		versionReason = string(gatewayv1.GatewayClassReasonUnsupportedVersion)
	}
	apiMeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		Status:             versionStatus,
		Reason:             versionReason,
		Message:            versionMessage,
		ObservedGeneration: class.Generation,
	})
	if apiequality.Semantic.DeepEqual(original.Status, class.Status) {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, r.Status().Patch(ctx, class, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	gatewayObject := &gatewayv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gatewayObject); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !gatewayObject.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(gatewayObject, GatewayFinalizer) {
			return reconcile.Result{}, nil
		}
		if err := r.cleanupTarget(ctx, gatewayObject); err != nil {
			return reconcile.Result{}, err
		}
		if err := r.clearGatewayRouteStatuses(ctx, gatewayObject); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, r.removeFinalizer(ctx, gatewayObject)
	}

	class := &gatewayv1.GatewayClass{}
	classKey := client.ObjectKey{Name: string(gatewayObject.Spec.GatewayClassName)}
	if err := r.Get(ctx, classKey, class); err != nil {
		if apierrors.IsNotFound(err) && controllerutil.ContainsFinalizer(gatewayObject, GatewayFinalizer) {
			if cleanupErr := r.cleanupTarget(ctx, gatewayObject); cleanupErr != nil {
				return reconcile.Result{}, cleanupErr
			}
			if clearErr := r.clearGatewayRouteStatuses(ctx, gatewayObject); clearErr != nil {
				return reconcile.Result{}, clearErr
			}
			return reconcile.Result{}, r.removeFinalizer(ctx, gatewayObject)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if string(class.Spec.ControllerName) != r.ControllerName {
		if controllerutil.ContainsFinalizer(gatewayObject, GatewayFinalizer) {
			if err := r.cleanupTarget(ctx, gatewayObject); err != nil {
				return reconcile.Result{}, err
			}
			if err := r.clearGatewayRouteStatuses(ctx, gatewayObject); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, r.removeFinalizer(ctx, gatewayObject)
		}
		return reconcile.Result{}, nil
	}
	if class.Spec.ParametersRef != nil {
		if controllerutil.ContainsFinalizer(gatewayObject, GatewayFinalizer) {
			if err := r.cleanupTarget(ctx, gatewayObject); err != nil {
				return reconcile.Result{}, err
			}
			if err := r.clearGatewayRouteStatuses(ctx, gatewayObject); err != nil {
				return reconcile.Result{}, err
			}
		}
		if err := r.writeGatewayStatus(ctx, gatewayObject, unsupportedClassStatus(gatewayObject), false, false); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}
	acceptedClass := conditionCurrentTrue(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted), class.Generation)
	supportedVersion := conditionCurrentTrue(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion), class.Generation)
	if !acceptedClass || !supportedVersion {
		acceptedCondition := apiMeta.FindStatusCondition(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		versionCondition := apiMeta.FindStatusCondition(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
		unsupportedVersion := versionCondition != nil && versionCondition.ObservedGeneration == class.Generation && versionCondition.Status == metav1.ConditionFalse ||
			acceptedCondition != nil && acceptedCondition.ObservedGeneration == class.Generation && acceptedCondition.Status == metav1.ConditionFalse && acceptedCondition.Reason == "UnsupportedVersions"
		if unsupportedVersion {
			if controllerutil.ContainsFinalizer(gatewayObject, GatewayFinalizer) {
				if err := r.cleanupTarget(ctx, gatewayObject); err != nil {
					return reconcile.Result{}, err
				}
				if err := r.clearGatewayRouteStatuses(ctx, gatewayObject); err != nil {
					return reconcile.Result{}, err
				}
			}
			message := "Gateway API CRD bundle is not supported"
			if versionCondition != nil && versionCondition.ObservedGeneration == class.Generation {
				message = versionCondition.Message
			}
			if err := r.writeGatewayStatus(ctx, gatewayObject, unsupportedVersionGatewayStatus(gatewayObject, message), false, false); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{RequeueAfter: deliveryRecheck}, nil
	}
	if err := r.ensureFinalizer(ctx, gatewayObject); err != nil {
		return reconcile.Result{}, err
	}

	allRoutes, routes, err := r.listRoutesForGateway(ctx, gatewayObject)
	if err != nil {
		return reconcile.Result{}, err
	}
	input, err := r.compilerInput(ctx, gatewayObject, class, routes)
	if err != nil {
		return reconcile.Result{}, err
	}
	compiled := (gateway.Compiler{ControllerName: r.ControllerName}).Compile(input)

	target := targetForGateway(gatewayObject)
	targetState, ensureErr := r.Provisioner.Ensure(ctx, targetIntent(gatewayObject))
	var deliveryErr error
	var receipt *delivery.Receipt
	var observation delivery.Observation
	if ensureErr != nil {
		deliveryErr = errors.New("ensure Gateway target failed")
	} else if targetState.Target != target {
		deliveryErr = errors.New("Gateway target response did not match the requested target")
	} else if targetState.Ready {
		forceReceipt := targetState.Simulated && !conditionCurrentMockTrue(gatewayObject.Status.Conditions, gatewayObject.Generation)
		observation, deliveryErr = r.applyDesired(ctx, target, r.OwnerEpoch, compiled.Resources, forceReceipt, &receipt)
	}
	if ensureErr == nil && targetState.Target == target {
		compiled.GatewayStatus.Addresses = gatewayStatusAddresses(targetState.Addresses)
	} else {
		compiled.GatewayStatus.Addresses = append([]gatewayv1.GatewayStatusAddress(nil), gatewayObject.Status.Addresses...)
	}

	active := targetState.Ready && allResourcesActive(target, compiled.Resources, receipt, observation)
	wasActive := conditionCurrentTrue(gatewayObject.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed), gatewayObject.Generation)
	wasMockActive := conditionCurrentMockTrue(gatewayObject.Status.Conditions, gatewayObject.Generation)
	simulated := targetState.Simulated || observation.Simulated
	if receipt != nil {
		simulated = simulated || receipt.Simulated
	}
	if targetState.Simulated {
		active = active && simulatedReceiptReady(receipt, compiled.Resources, target)
	}
	if targetState.Simulated && wasMockActive && observation.Simulated && observation.Connected && allResourcesActive(target, compiled.Resources, nil, observation) {
		active = true
	}

	if deliveryErr != nil {
		active = active && (receipt != nil || (wasActive && observation.Connected))
	}

	if err := r.writeGatewayStatus(ctx, gatewayObject, compiled.GatewayStatus, active, simulated); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.writeRouteStatuses(ctx, allRoutes, gatewayObject, compiled.RouteStatuses); err != nil {
		return reconcile.Result{}, err
	}
	if deliveryErr != nil {
		return reconcile.Result{}, deliveryErr
	}
	if !targetState.Ready || !active {
		return reconcile.Result{RequeueAfter: deliveryRecheck}, nil
	}
	if ctrl.LoggerFrom(ctx).V(1).Enabled() {
		ctrl.LoggerFrom(ctx).V(1).Info("Gateway configuration is active", "Gateway", req.NamespacedName, "simulated", simulated)
	}
	return reconcile.Result{}, nil
}

func (r *Reconciler) applyDesired(ctx context.Context, target config.Target, ownerEpoch string, desired []config.Resource, forceReceipt bool, receiptOut **delivery.Receipt) (delivery.Observation, error) {
	if err := r.Configurator.Claim(ctx, target, ownerEpoch); err != nil {
		return delivery.Observation{}, errors.New("claim Gateway configuration target failed")
	}
	observation, err := r.Configurator.Observe(ctx, target)
	if err != nil {
		return observation, errors.New("observe Gateway configuration failed")
	}
	changes, err := delivery.Diff(target, ownerEpoch, config.DomainPraxis, desired, observation)
	if err != nil {
		return observation, errors.New("calculate Gateway configuration changes failed")
	}
	if len(changes.Upserts) == 0 && len(changes.Deletes) == 0 && forceReceipt && len(desired) > 0 {
		changes = config.ChangeSet{Target: target, OwnerEpoch: ownerEpoch, Upserts: append([]config.Resource(nil), desired...)}
		sort.Slice(changes.Upserts, func(i, j int) bool { return changes.Upserts[i].Key.String() < changes.Upserts[j].Key.String() })
		changes.ID, _ = changes.StableID()
		if err := changes.Validate(); err != nil {
			return observation, errors.New("build Gateway active receipt request failed")
		}
	}
	if len(changes.Upserts) == 0 && len(changes.Deletes) == 0 {
		return observation, nil
	}
	receipt, applyErr := r.Configurator.Apply(ctx, changes)
	if applyErr != nil {
		updated, observeErr := r.Configurator.Observe(ctx, target)
		if observeErr == nil {
			observation = updated
		}
		return observation, errors.New("apply Gateway configuration failed")
	}
	if delivery.ValidateReceipt(changes, receipt) != nil {
		return observation, errors.New("Gateway configuration receipt did not match the requested changes")
	}
	*receiptOut = &receipt
	updated, err := r.Configurator.Observe(ctx, target)
	if err != nil {
		return observation, errors.New("observe applied Gateway configuration failed")
	}
	return updated, nil
}

func (r *Reconciler) compilerInput(ctx context.Context, gatewayObject *gatewayv1.Gateway, class *gatewayv1.GatewayClass, routes []gatewayv1.HTTPRoute) (gateway.Input, error) {
	input := gateway.Input{
		Target:         targetForGateway(gatewayObject),
		OwnerEpoch:     r.OwnerEpoch,
		GatewayClass:   class,
		Gateway:        gatewayObject,
		Routes:         routes,
		Namespaces:     make(map[types.NamespacedName]corev1.Namespace),
		Services:       make(map[types.NamespacedName]corev1.Service),
		EndpointSlices: make(map[types.NamespacedName][]discoveryv1.EndpointSlice),
		Secrets:        make(map[types.NamespacedName]gateway.TLSSecret),
	}
	grants := make(map[types.NamespacedName]gatewayv1.ReferenceGrant)
	addGrants := func(group, kind, namespace string) error {
		items, err := listReferenceGrantsForSource(ctx, r.Client, group, kind, namespace)
		if err != nil {
			return err
		}
		for _, item := range items {
			grants[client.ObjectKeyFromObject(&item)] = item
		}
		return nil
	}
	if err := addGrants(string(gatewayv1.GroupName), "Gateway", gatewayObject.Namespace); err != nil {
		return gateway.Input{}, err
	}
	for _, route := range routes {
		if err := addGrants(string(gatewayv1.GroupName), "HTTPRoute", route.Namespace); err != nil {
			return gateway.Input{}, err
		}
		if err := addNamespaceToInput(ctx, r.Client, input.Namespaces, route.Namespace); err != nil {
			return gateway.Input{}, err
		}
	}
	if err := addNamespaceToInput(ctx, r.Client, input.Namespaces, gatewayObject.Namespace); err != nil {
		return gateway.Input{}, err
	}
	input.ReferenceGrants = sortedGrantValues(grants)

	servicesSeen := make(map[types.NamespacedName]struct{})
	for _, route := range routes {
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
				if !referenceAllowed(input.ReferenceGrants, string(gatewayv1.GroupName), "HTTPRoute", route.Namespace, "", "Service", namespace, string(ref.Name)) {
					continue
				}
				key := types.NamespacedName{Namespace: namespace, Name: string(ref.Name)}
				if _, seen := servicesSeen[key]; seen {
					continue
				}
				servicesSeen[key] = struct{}{}
				service := &corev1.Service{}
				if err := r.Get(ctx, key, service); err != nil {
					if apierrors.IsNotFound(err) {
						continue
					}
					return gateway.Input{}, err
				}
				input.Services[key] = *service
				var slices discoveryv1.EndpointSliceList
				if err := r.List(ctx, &slices, client.InNamespace(namespace), client.MatchingFields{EndpointSliceServiceIndex: service.Name}); err != nil {
					return gateway.Input{}, err
				}
				input.EndpointSlices[key] = slices.Items
			}
		}
	}

	for _, listener := range gatewayObject.Spec.Listeners {
		if listener.TLS == nil {
			continue
		}
		for _, ref := range listener.TLS.CertificateRefs {
			if !isSecretReference(ref.Group, ref.Kind) {
				continue
			}
			namespace := gatewayObject.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}
			if !referenceAllowed(input.ReferenceGrants, string(gatewayv1.GroupName), "Gateway", gatewayObject.Namespace, "", "Secret", namespace, string(ref.Name)) {
				continue
			}
			key := types.NamespacedName{Namespace: namespace, Name: string(ref.Name)}
			secret := &corev1.Secret{}
			if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: string(ref.Name)}, secret); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return gateway.Input{}, err
			}
			input.Secrets[key] = gateway.TLSSecretInfo(secret)
			// The Secret informer is metadata-only; discard the direct-read bytes after validation.
			for _, value := range secret.Data {
				clear(value)
			}
		}
	}
	return input, nil
}

func addNamespaceToInput(ctx context.Context, c client.Reader, namespaces map[types.NamespacedName]corev1.Namespace, name string) error {
	key := types.NamespacedName{Name: name}
	if _, exists := namespaces[key]; exists {
		return nil
	}
	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	namespaces[key] = namespace
	return nil
}

func sortedGrantValues(grants map[types.NamespacedName]gatewayv1.ReferenceGrant) []gatewayv1.ReferenceGrant {
	keys := make([]types.NamespacedName, 0, len(grants))
	for key := range grants {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})
	items := make([]gatewayv1.ReferenceGrant, 0, len(keys))
	for _, key := range keys {
		items = append(items, grants[key])
	}
	return items
}

func referenceAllowed(grants []gatewayv1.ReferenceGrant, sourceGroup, sourceKind, sourceNamespace, targetGroup, targetKind, targetNamespace, targetName string) bool {
	if sourceNamespace == targetNamespace {
		return true
	}
	for _, grant := range grants {
		if grant.Namespace != targetNamespace {
			continue
		}
		fromAllowed := false
		for _, from := range grant.Spec.From {
			if string(from.Group) == sourceGroup && string(from.Kind) == sourceKind && string(from.Namespace) == sourceNamespace {
				fromAllowed = true
				break
			}
		}
		if !fromAllowed {
			continue
		}
		for _, to := range grant.Spec.To {
			if string(to.Group) != targetGroup || string(to.Kind) != targetKind {
				continue
			}
			if to.Name == nil || string(*to.Name) == targetName {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) listRoutesForGateway(ctx context.Context, gatewayObject *gatewayv1.Gateway) ([]gatewayv1.HTTPRoute, []gatewayv1.HTTPRoute, error) {
	key := objectKey(gatewayObject.Namespace, gatewayObject.Name)
	current := make(map[types.NamespacedName]gatewayv1.HTTPRoute)
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{HTTPRouteParentIndex: key}); err != nil {
		return nil, nil, err
	}
	for _, route := range routes.Items {
		current[client.ObjectKeyFromObject(&route)] = route
	}
	all := make(map[types.NamespacedName]gatewayv1.HTTPRoute, len(current))
	for routeKey, route := range current {
		all[routeKey] = route
	}
	var stale gatewayv1.HTTPRouteList
	if err := r.List(ctx, &stale, client.MatchingFields{HTTPRouteStatusIndex: key}); err != nil {
		return nil, nil, err
	}
	for _, route := range stale.Items {
		all[client.ObjectKeyFromObject(&route)] = route
	}
	allItems := routeMapValues(all)
	currentItems := routeMapValues(current)
	return allItems, currentItems, nil
}

func routeMapValues(routes map[types.NamespacedName]gatewayv1.HTTPRoute) []gatewayv1.HTTPRoute {
	keys := make([]types.NamespacedName, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})
	items := make([]gatewayv1.HTTPRoute, 0, len(keys))
	for _, key := range keys {
		items = append(items, routes[key])
	}
	return items
}

func (r *Reconciler) writeGatewayStatus(ctx context.Context, gatewayObject *gatewayv1.Gateway, desired gatewayv1.GatewayStatus, programmed, simulated bool) error {
	original := gatewayObject.DeepCopy()
	gatewayObject.Status = mergeGatewayStatus(gatewayObject, desired, programmed, simulated)
	if apiequality.Semantic.DeepEqual(original.Status, gatewayObject.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, gatewayObject, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return nil
}

func (r *Reconciler) writeRouteStatuses(ctx context.Context, routes []gatewayv1.HTTPRoute, gatewayObject *gatewayv1.Gateway, desired map[types.NamespacedName]gatewayv1.HTTPRouteStatus) error {
	for i := range routes {
		route := routes[i].DeepCopy()
		key := client.ObjectKeyFromObject(route)
		compiled, found := desired[key]
		var compiledPtr *gatewayv1.HTTPRouteStatus
		if found {
			compiledPtr = &compiled
		}
		original := route.DeepCopy()
		route.Status.Parents = mergeRouteParents(route, gatewayObject, r.ControllerName, compiledPtr)
		if apiequality.Semantic.DeepEqual(original.Status, route.Status) {
			continue
		}
		if err := r.Status().Patch(ctx, route, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) clearGatewayRouteStatuses(ctx context.Context, gatewayObject *gatewayv1.Gateway) error {
	routes, _, err := r.listRoutesForGateway(ctx, gatewayObject)
	if err != nil {
		return err
	}
	for i := range routes {
		route := routes[i].DeepCopy()
		original := route.DeepCopy()
		remaining := make([]gatewayv1.RouteParentStatus, 0, len(route.Status.Parents))
		for _, parent := range route.Status.Parents {
			if string(parent.ControllerName) == r.ControllerName && parentRefTargetsGateway(parent.ParentRef, route.Namespace, gatewayObject) {
				continue
			}
			remaining = append(remaining, parent)
		}
		route.Status.Parents = remaining
		if apiequality.Semantic.DeepEqual(original.Status, route.Status) {
			continue
		}
		if err := r.Status().Patch(ctx, route, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ensureFinalizer(ctx context.Context, gatewayObject *gatewayv1.Gateway) error {
	if controllerutil.ContainsFinalizer(gatewayObject, GatewayFinalizer) {
		return nil
	}
	original := gatewayObject.DeepCopy()
	controllerutil.AddFinalizer(gatewayObject, GatewayFinalizer)
	if err := r.Patch(ctx, gatewayObject, client.MergeFrom(original)); err != nil {
		return err
	}
	return nil
}

func (r *Reconciler) removeFinalizer(ctx context.Context, gatewayObject *gatewayv1.Gateway) error {
	if !controllerutil.ContainsFinalizer(gatewayObject, GatewayFinalizer) {
		return nil
	}
	original := gatewayObject.DeepCopy()
	controllerutil.RemoveFinalizer(gatewayObject, GatewayFinalizer)
	return r.Patch(ctx, gatewayObject, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) cleanupTarget(ctx context.Context, gatewayObject *gatewayv1.Gateway) error {
	if err := r.withdrawTarget(ctx, gatewayObject); err != nil {
		return err
	}
	if err := r.Provisioner.Release(ctx, targetForGateway(gatewayObject)); err != nil {
		return errors.New("release Gateway target failed")
	}
	return nil
}

func (r *Reconciler) withdrawTarget(ctx context.Context, gatewayObject *gatewayv1.Gateway) error {
	target := targetForGateway(gatewayObject)
	if err := r.Configurator.Claim(ctx, target, r.OwnerEpoch); err != nil {
		return errors.New("claim Gateway configuration target failed")
	}
	observation, err := r.Configurator.Observe(ctx, target)
	if err != nil || !observation.Connected || observation.Target != target {
		return errors.New("cannot confirm Gateway target cleanup state")
	}
	keys := observedPraxisKeys(target, observation)
	var receipt *delivery.Receipt
	if len(keys) > 0 {
		changes := config.ChangeSet{Target: target, OwnerEpoch: r.OwnerEpoch, Deletes: keys}
		changes.ID, _ = changes.StableID()
		if err := changes.Validate(); err != nil {
			return errors.New("build Gateway cleanup changes failed")
		}
		result, err := r.Configurator.Apply(ctx, changes)
		if err != nil {
			return errors.New("delete Gateway configuration failed")
		}
		if delivery.ValidateReceipt(changes, result) != nil {
			return errors.New("Gateway cleanup receipt did not match the requested changes")
		}
		receipt = &result
	}
	if len(keys) > 0 && !deletesAcknowledged(keys, receipt) {
		updated, err := r.Configurator.Observe(ctx, target)
		if err != nil || !updated.Connected || updated.Target != target || len(observedPraxisKeys(target, updated)) != 0 {
			return errors.New("Gateway cleanup is not yet acknowledged")
		}
	}
	return nil
}

func observedPraxisKeys(target config.Target, observation delivery.Observation) []config.Key {
	seen := make(map[config.Key]struct{})
	for _, result := range append(append([]delivery.ResourceResult(nil), observation.Resources...), observation.Rejected...) {
		key := result.Key
		if key.Domain == config.DomainPraxis && key.Target == target {
			seen[key] = struct{}{}
		}
	}
	keys := make([]config.Key, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

func deletesAcknowledged(keys []config.Key, receipt *delivery.Receipt) bool {
	if receipt == nil {
		return false
	}
	deleted := make(map[config.Key]bool, len(receipt.Resources))
	for _, result := range receipt.Resources {
		if result.State == delivery.ApplyDeleted {
			deleted[result.Key] = true
		}
	}
	for _, key := range keys {
		if !deleted[key] {
			return false
		}
	}
	return true
}

func targetForGateway(gatewayObject *gatewayv1.Gateway) config.Target {
	return config.Target{Namespace: gatewayObject.Namespace, Name: gatewayObject.Name, UID: string(gatewayObject.UID)}
}

func targetIntent(gatewayObject *gatewayv1.Gateway) delivery.TargetIntent {
	intent := delivery.TargetIntent{Gateway: config.ObjectRef{
		APIVersion: gatewayv1.GroupVersion.String(),
		Kind:       "Gateway",
		Namespace:  gatewayObject.Namespace,
		Name:       gatewayObject.Name,
		UID:        string(gatewayObject.UID),
		Generation: gatewayObject.Generation,
	}}
	for _, listener := range gatewayObject.Spec.Listeners {
		intent.Listeners = append(intent.Listeners, delivery.ListenerIntent{
			Name:     string(listener.Name),
			Port:     int32(listener.Port),
			Protocol: string(listener.Protocol),
		})
	}
	return intent
}

func conditionCurrentTrue(conditions []metav1.Condition, conditionType string, generation int64) bool {
	condition := apiMeta.FindStatusCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
}

func conditionCurrentMockTrue(conditions []metav1.Condition, generation int64) bool {
	condition := apiMeta.FindStatusCondition(conditions, string(gatewayv1.GatewayConditionProgrammed))
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation && condition.Message == "Mock configuration is active"
}

func simulatedReceiptReady(receipt *delivery.Receipt, resources []config.Resource, target config.Target) bool {
	if receipt == nil || !receipt.Simulated || receipt.Target != target || len(resources) == 0 {
		return false
	}
	for _, resource := range resources {
		if revision, ok := receipt.ActiveRevision(resource.Key); !ok || revision != resource.Revision {
			return false
		}
	}
	return true
}

func requestForGateway(namespace, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

func (r *Reconciler) mapGatewayClass(ctx context.Context, obj client.Object) []reconcile.Request {
	class, ok := obj.(*gatewayv1.GatewayClass)
	if !ok {
		return nil
	}
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{GatewayClassIndex: class.Name}); err != nil {
		r.logMappingError(ctx, "GatewayClass")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(gateways.Items))
	for _, gatewayObject := range gateways.Items {
		requests = append(requests, requestForGateway(gatewayObject.Namespace, gatewayObject.Name))
	}
	return requests
}

func (r *Reconciler) mapHTTPRoute(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	return requestsForParents(route.Spec.ParentRefs, route.Namespace)
}

func (r *Reconciler) mapNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	namespace, ok := obj.(*corev1.Namespace)
	if !ok {
		return nil
	}
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.InNamespace(namespace.Name)); err != nil {
		r.logMappingError(ctx, "Namespace")
		return nil
	}
	var requests []reconcile.Request
	for _, route := range routes.Items {
		requests = append(requests, requestsForParents(route.Spec.ParentRefs, route.Namespace)...)
	}
	return uniqueRequests(requests)
}

func (r *Reconciler) mapService(ctx context.Context, obj client.Object) []reconcile.Request {
	service, ok := obj.(*corev1.Service)
	if !ok {
		return nil
	}
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{HTTPRouteBackendIndex: objectKey(service.Namespace, service.Name)}); err != nil {
		r.logMappingError(ctx, "Service")
		return nil
	}
	var requests []reconcile.Request
	for _, route := range routes.Items {
		requests = append(requests, requestsForParents(route.Spec.ParentRefs, route.Namespace)...)
	}
	return uniqueRequests(requests)
}

func (r *Reconciler) mapSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*metav1.PartialObjectMetadata)
	if !ok {
		return nil
	}
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{GatewaySecretIndex: objectKey(secret.Namespace, secret.Name)}); err != nil {
		r.logMappingError(ctx, "Secret")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(gateways.Items))
	for _, gatewayObject := range gateways.Items {
		requests = append(requests, requestForGateway(gatewayObject.Namespace, gatewayObject.Name))
	}
	return requests
}

func (r *Reconciler) mapReferenceGrant(ctx context.Context, obj client.Object) []reconcile.Request {
	grant, ok := obj.(*gatewayv1.ReferenceGrant)
	if !ok {
		return nil
	}
	var requests []reconcile.Request
	for _, from := range grant.Spec.From {
		sourceNamespace := string(from.Namespace)
		switch {
		case string(from.Group) == string(gatewayv1.GroupName) && string(from.Kind) == "HTTPRoute":
			var routes gatewayv1.HTTPRouteList
			if err := r.List(ctx, &routes, client.InNamespace(sourceNamespace)); err != nil {
				r.logMappingError(ctx, "ReferenceGrant")
				continue
			}
			for _, route := range routes.Items {
				if routeReferencesGrantTarget(&route, grant) {
					requests = append(requests, requestsForParents(route.Spec.ParentRefs, route.Namespace)...)
				}
			}
		case string(from.Group) == string(gatewayv1.GroupName) && string(from.Kind) == "Gateway":
			var gateways gatewayv1.GatewayList
			if err := r.List(ctx, &gateways, client.InNamespace(sourceNamespace)); err != nil {
				r.logMappingError(ctx, "ReferenceGrant")
				continue
			}
			for _, gatewayObject := range gateways.Items {
				if gatewayReferencesGrantTarget(&gatewayObject, grant) {
					requests = append(requests, requestForGateway(gatewayObject.Namespace, gatewayObject.Name))
				}
			}
		}
	}
	return uniqueRequests(requests)
}

func (r *Reconciler) logMappingError(ctx context.Context, kind string) {
	ctrl.LoggerFrom(ctx).Error(errors.New("dependency mapping list failed"), "Could not map dependency change", "kind", kind)
}

func requestsForParents(parents []gatewayv1.ParentReference, routeNamespace string) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(parents))
	for _, parent := range parents {
		if !isGatewayParent(parent) {
			continue
		}
		namespace := routeNamespace
		if parent.Namespace != nil {
			namespace = string(*parent.Namespace)
		}
		requests = append(requests, requestForGateway(namespace, string(parent.Name)))
	}
	return uniqueRequests(requests)
}

func uniqueRequests(requests []reconcile.Request) []reconcile.Request {
	seen := make(map[types.NamespacedName]struct{}, len(requests))
	out := make([]reconcile.Request, 0, len(requests))
	for _, request := range requests {
		if _, ok := seen[request.NamespacedName]; ok {
			continue
		}
		seen[request.NamespacedName] = struct{}{}
		out = append(out, request)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func routeReferencesGrantTarget(route *gatewayv1.HTTPRoute, grant *gatewayv1.ReferenceGrant) bool {
	for _, rule := range route.Spec.Rules {
		for _, backend := range rule.BackendRefs {
			ref := backend.BackendRef
			namespace := route.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}
			if namespace != grant.Namespace || !isServiceReference(ref.Group, ref.Kind) {
				continue
			}
			for _, to := range grant.Spec.To {
				if string(to.Group) != "" || string(to.Kind) != "Service" || to.Name != nil && string(*to.Name) != string(ref.Name) {
					continue
				}
				return true
			}
		}
	}
	return false
}

func gatewayReferencesGrantTarget(gatewayObject *gatewayv1.Gateway, grant *gatewayv1.ReferenceGrant) bool {
	for _, listener := range gatewayObject.Spec.Listeners {
		if listener.TLS == nil {
			continue
		}
		for _, ref := range listener.TLS.CertificateRefs {
			namespace := gatewayObject.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}
			if namespace != grant.Namespace || !isSecretReference(ref.Group, ref.Kind) {
				continue
			}
			for _, to := range grant.Spec.To {
				if string(to.Group) != "" || string(to.Kind) != "Secret" || to.Name != nil && string(*to.Name) != string(ref.Name) {
					continue
				}
				return true
			}
		}
	}
	return false
}

func mapEndpointSliceToService(_ context.Context, obj client.Object) []reconcile.Request {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}
	serviceName := slice.Labels[EndpointSliceServiceLabel]
	if serviceName == "" {
		return nil
	}
	return []reconcile.Request{requestForGateway(slice.Namespace, serviceName)}
}
