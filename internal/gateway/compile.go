package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/config"
)

type listenerPlan struct {
	spec            gatewayv1.Listener
	protocol        config.ListenerProtocol
	supportsHTTP    bool
	accepted        bool
	acceptReason    string
	acceptMessage   string
	conflict        string
	tlsOverlap      bool
	resolved        bool
	resolvedReason  string
	resolvedMessage string
	secretRefs      []config.SecretRef
	secretHandles   []config.Resource
	routeTable      string
	rules           []rankedRule
	attached        map[types.NamespacedName]struct{}
}

type matchRank struct {
	headers    int
	method     bool
	queries    int
	pathExact  bool
	pathLength int
}

type translatedMatch struct {
	value       config.HTTPMatch
	unsupported bool
	rank        matchRank
}

type rankedRule struct {
	rule        config.HTTPRule
	hostExact   int
	hostLength  int
	pathExact   bool
	pathLength  int
	hasMethod   bool
	headerCount int
	queryCount  int
	created     time.Time
	namespace   string
	routeName   string
}

type routeCompileResult struct {
	rules            []rankedRule
	validRules       int
	unsupportedRules int
	resolvedReason   string
	resolvedMessage  string
}

type parentEvaluation struct {
	ref               gatewayv1.ParentReference
	attachedListeners []*listenerPlan
	accepted          bool
	acceptedReason    string
	acceptedMessage   string
	resolvedReason    string
	resolvedMessage   string
	validRules        int
	unsupportedRules  int
}

// Compile produces target-scoped desired resources and Gateway API status
// decisions from the supplied snapshot.
func (c Compiler) Compile(input Input) Result {
	result := Result{RouteStatuses: make(map[types.NamespacedName]gatewayv1.HTTPRouteStatus)}
	gateway := input.Gateway
	if gateway == nil || input.GatewayClass == nil || c.ControllerName == "" ||
		input.GatewayClass.Name != string(gateway.Spec.GatewayClassName) ||
		string(input.GatewayClass.Spec.ControllerName) != c.ControllerName {
		return result
	}
	if gateway.UID == "" || input.Target.UID != string(gateway.UID) || input.Target.Namespace != gateway.Namespace || input.Target.Name != gateway.Name {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Object:    types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name},
			Condition: string(gatewayv1.GatewayConditionProgrammed),
			Reason:    string(gatewayv1.GatewayReasonInvalid),
			Message:   "Gateway target identity does not match the Gateway UID",
		})
		return result
	}
	if input.OwnerEpoch == "" {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Object:    types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name},
			Condition: string(gatewayv1.GatewayConditionProgrammed),
			Reason:    string(gatewayv1.GatewayReasonInvalid),
			Message:   "Gateway owner epoch is required",
		})
		return result
	}

	ownerEpoch := input.OwnerEpoch
	resources := make(map[config.Key]config.Resource)
	diagnostics := make([]Diagnostic, 0)
	plans := make([]*listenerPlan, 0, len(gateway.Spec.Listeners))
	for _, spec := range gateway.Spec.Listeners {
		plans = append(plans, c.planListener(input, spec, &diagnostics))
	}
	markListenerConflicts(plans, gateway, &diagnostics)
	sort.Slice(plans, func(i, j int) bool { return string(plans[i].spec.Name) < string(plans[j].spec.Name) })

	routes := append([]gatewayv1.HTTPRoute(nil), input.Routes...)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Namespace != routes[j].Namespace {
			return routes[i].Namespace < routes[j].Namespace
		}
		return routes[i].Name < routes[j].Name
	})
	for i := range routes {
		c.compileRoute(input, &routes[i], plans, ownerEpoch, resources, &diagnostics, result.RouteStatuses)
	}

	listenerStatuses := make([]gatewayv1.ListenerStatus, 0, len(plans))
	acceptedListeners := 0
	invalidListeners := false
	for _, plan := range plans {
		if plan.accepted && plan.conflict == "" {
			acceptedListeners++
		}
		if !plan.accepted || plan.conflict != "" {
			invalidListeners = true
		}

		table := config.HTTPRouteTable{Rules: make([]config.HTTPRule, 0, len(plan.rules))}
		sort.Slice(plan.rules, func(i, j int) bool { return lessRankedRule(plan.rules[i], plan.rules[j]) })
		for i := range plan.rules {
			plan.rules[i].rule.Priority = i
			table.Rules = append(table.Rules, plan.rules[i].rule)
		}
		if plan.accepted && plan.supportsHTTP && plan.conflict == "" {
			tableResource, err := config.NewResource(
				config.Key{Domain: config.DomainPraxis, Target: input.Target, OwnerEpoch: ownerEpoch, Kind: config.KindHTTPRoute, Name: plan.routeTable},
				gatewayObjectRef(*gateway),
				table,
			)
			if err == nil {
				resources[tableResource.Key] = tableResource
			}
		}
		if plan.accepted && plan.conflict == "" {
			listener := config.Listener{
				Port:       int32(plan.spec.Port),
				Protocol:   plan.protocol,
				Hostname:   listenerHostname(plan.spec),
				RouteTable: plan.routeTable,
				Secrets:    append([]config.SecretRef(nil), plan.secretRefs...),
			}
			resource, err := config.NewResource(
				config.Key{Domain: config.DomainPraxis, Target: input.Target, OwnerEpoch: ownerEpoch, Kind: config.KindListener, Name: string(plan.spec.Name)},
				gatewayObjectRef(*gateway),
				listener,
			)
			if err == nil {
				resources[resource.Key] = resource
			}
			for _, handle := range plan.secretHandles {
				resources[handle.Key] = handle
			}
		}

		listenerStatuses = append(listenerStatuses, c.listenerStatus(gateway, plan, previousListenerStatus(gateway.Status.Listeners, plan.spec.Name)))
	}
	result.GatewayStatus = c.gatewayStatus(gateway, listenerStatuses, acceptedListeners, invalidListeners)
	result.GatewayConditions = append([]metav1.Condition(nil), result.GatewayStatus.Conditions...)
	for _, resource := range resources {
		result.Resources = append(result.Resources, resource)
	}
	sort.Slice(result.Resources, func(i, j int) bool { return result.Resources[i].Key.String() < result.Resources[j].Key.String() })
	result.Diagnostics = dedupeDiagnostics(diagnostics)
	return result
}

func (c Compiler) planListener(input Input, spec gatewayv1.Listener, diagnostics *[]Diagnostic) *listenerPlan {
	plan := &listenerPlan{
		spec:          spec,
		routeTable:    string(spec.Name),
		accepted:      true,
		acceptReason:  string(gatewayv1.ListenerReasonAccepted),
		acceptMessage: "Listener is valid",
		resolved:      true,
		attached:      make(map[types.NamespacedName]struct{}),
	}
	listenerObject := types.NamespacedName{Namespace: input.Gateway.Namespace, Name: input.Gateway.Name}
	switch string(spec.Protocol) {
	case string(gatewayv1.HTTPProtocolType):
		plan.protocol = config.ListenerHTTP
		plan.supportsHTTP = true
		if spec.TLS != nil {
			plan.reject(string(gatewayv1.ListenerReasonUnsupportedValue), "HTTP listeners cannot configure TLS")
			addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionAccepted), string(gatewayv1.ListenerReasonUnsupportedValue), "HTTP listeners cannot configure TLS")
		}
	case string(gatewayv1.HTTPSProtocolType):
		plan.protocol = config.ListenerHTTPS
		plan.supportsHTTP = true
		c.resolveListenerTLS(input, plan, diagnostics)
	default:
		plan.reject(string(gatewayv1.ListenerReasonUnsupportedProtocol), "listener protocol is not supported by the HTTP compiler")
		addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionAccepted), string(gatewayv1.ListenerReasonUnsupportedProtocol), "listener protocol is not supported by the HTTP compiler")
	}
	if int32(spec.Port) < 1 || int32(spec.Port) > 65535 {
		plan.reject(string(gatewayv1.ListenerReasonUnsupportedValue), "listener port must be between 1 and 65535")
		addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionAccepted), string(gatewayv1.ListenerReasonUnsupportedValue), fmt.Sprintf("listener %q has an invalid port", spec.Name))
	}

	_, invalidKinds := supportedHTTPKinds(spec)
	if invalidKinds {
		plan.setResolved(string(gatewayv1.ListenerReasonInvalidRouteKinds), "listener specifies unsupported Route kinds")
		addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionResolvedRefs), string(gatewayv1.ListenerReasonInvalidRouteKinds), fmt.Sprintf("listener %q specifies unsupported Route kinds", spec.Name))
	}
	return plan
}

func (c Compiler) resolveListenerTLS(input Input, plan *listenerPlan, diagnostics *[]Diagnostic) {
	listenerObject := types.NamespacedName{Namespace: input.Gateway.Namespace, Name: input.Gateway.Name}
	if plan.spec.TLS == nil || len(plan.spec.TLS.CertificateRefs) == 0 {
		plan.reject(string(gatewayv1.ListenerReasonInvalid), "HTTPS listener requires a TLS Secret reference")
		plan.setResolved(string(gatewayv1.ListenerReasonInvalidCertificateRef), "HTTPS listener has no certificate reference")
		addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionResolvedRefs), string(gatewayv1.ListenerReasonInvalidCertificateRef), fmt.Sprintf("listener %q requires a certificate Secret", plan.spec.Name))
		return
	}
	mode := gatewayv1.TLSModeTerminate
	if plan.spec.TLS.Mode != nil {
		mode = *plan.spec.TLS.Mode
	}
	if mode != gatewayv1.TLSModeTerminate {
		plan.reject(string(gatewayv1.ListenerReasonUnsupportedValue), "HTTPS listener uses TLS settings outside the HTTP Core profile")
		addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionAccepted), string(gatewayv1.ListenerReasonUnsupportedValue), fmt.Sprintf("listener %q uses TLS settings outside the HTTP Core profile", plan.spec.Name))
		return
	}
	unsupportedSettings := len(plan.spec.TLS.Options) != 0 || len(plan.spec.TLS.CertificateRefs) != 1
	if unsupportedSettings {
		plan.reject(string(gatewayv1.ListenerReasonUnsupportedValue), "HTTPS listener uses TLS settings outside the HTTP Core profile")
		addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionAccepted), string(gatewayv1.ListenerReasonUnsupportedValue), fmt.Sprintf("listener %q uses TLS settings outside the HTTP Core profile", plan.spec.Name))
	}

	resolved := make([]config.SecretRef, 0, len(plan.spec.TLS.CertificateRefs))
	handles := make([]config.Resource, 0, len(plan.spec.TLS.CertificateRefs))
	for _, ref := range plan.spec.TLS.CertificateRefs {
		group := ""
		if ref.Group != nil {
			group = string(*ref.Group)
		}
		kind := "Secret"
		if ref.Kind != nil {
			kind = string(*ref.Kind)
		}
		namespace := input.Gateway.Namespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}
		name := string(ref.Name)
		if group != "" || kind != "Secret" {
			plan.reject(string(gatewayv1.ListenerReasonInvalid), "HTTPS certificate reference must be a core Secret")
			plan.setResolved(string(gatewayv1.ListenerReasonInvalidCertificateRef), "HTTPS certificate reference must be a core Secret")
			addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionResolvedRefs), string(gatewayv1.ListenerReasonInvalidCertificateRef), fmt.Sprintf("listener %q certificate reference must be a core Secret", plan.spec.Name))
			continue
		}
		if namespace != input.Gateway.Namespace && !referenceGranted(input.ReferenceGrants, namespace, string(gatewayv1.GroupName), "Gateway", input.Gateway.Namespace, "", "Secret", name) {
			plan.reject(string(gatewayv1.ListenerReasonInvalid), "cross-namespace certificate Secret reference is not permitted")
			plan.setResolved(string(gatewayv1.ListenerReasonRefNotPermitted), "cross-namespace certificate Secret reference is not permitted")
			addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionResolvedRefs), string(gatewayv1.ListenerReasonRefNotPermitted), fmt.Sprintf("listener %q certificate Secret reference is not permitted", plan.spec.Name))
			continue
		}
		secret, ok := input.Secrets[types.NamespacedName{Namespace: namespace, Name: name}]
		if !ok || secret.UID == "" || secret.ResourceVersion == "" || !secret.Valid {
			plan.reject(string(gatewayv1.ListenerReasonInvalid), "HTTPS certificate Secret is missing or invalid")
			plan.setResolved(string(gatewayv1.ListenerReasonInvalidCertificateRef), "HTTPS certificate Secret is missing or invalid")
			addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionResolvedRefs), string(gatewayv1.ListenerReasonInvalidCertificateRef), fmt.Sprintf("listener %q certificate Secret is missing or invalid", plan.spec.Name))
			continue
		}

		resolved = append(resolved, config.SecretRef{Namespace: namespace, Name: name})
		if !unsupportedSettings {
			handle, err := config.NewResource(
				config.Key{Domain: config.DomainPraxis, Target: input.Target, OwnerEpoch: input.OwnerEpoch, Kind: config.KindSecretHandle, Name: secretResourceName(namespace, name)},
				config.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: namespace, Name: name, UID: secret.UID},
				config.SecretHandle{Namespace: namespace, Name: name, UID: secret.UID, ResourceVersion: secret.ResourceVersion},
			)
			if err != nil {
				plan.reject(string(gatewayv1.ListenerReasonInvalid), "HTTPS certificate Secret handle could not be compiled")
				plan.setResolved(string(gatewayv1.ListenerReasonInvalidCertificateRef), "HTTPS certificate Secret handle could not be compiled")
				addDiagnostic(diagnostics, listenerObject, string(gatewayv1.ListenerConditionResolvedRefs), string(gatewayv1.ListenerReasonInvalidCertificateRef), fmt.Sprintf("listener %q certificate Secret handle could not be compiled", plan.spec.Name))
				continue
			}
			handles = append(handles, handle)
		}
	}
	if !unsupportedSettings && plan.resolved && len(resolved) == 1 && len(handles) == 1 {
		plan.secretRefs = resolved
		plan.secretHandles = handles
	}
}

func (p *listenerPlan) reject(reason, message string) {
	if p.accepted || p.acceptReason == "" {
		p.accepted = false
		p.acceptReason = reason
		p.acceptMessage = message
	}
}

func (p *listenerPlan) setResolved(reason, message string) {
	if p.resolvedReason == "" || reason == string(gatewayv1.ListenerReasonRefNotPermitted) {
		p.resolved = false
		p.resolvedReason = reason
		p.resolvedMessage = message
	}
}

func supportedHTTPKinds(listener gatewayv1.Listener) ([]gatewayv1.RouteGroupKind, bool) {
	if !isHTTPProtocol(listener.Protocol) {
		return nil, false
	}
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		return []gatewayv1.RouteGroupKind{httpRouteGroupKind()}, false
	}
	var supported []gatewayv1.RouteGroupKind
	invalid := false
	for _, kind := range listener.AllowedRoutes.Kinds {
		group := string(gatewayv1.GroupName)
		if kind.Group != nil {
			group = string(*kind.Group)
		}
		if group == string(gatewayv1.GroupName) && string(kind.Kind) == "HTTPRoute" {
			supported = append(supported, httpRouteGroupKind())
		} else {
			invalid = true
		}
	}
	if len(supported) > 0 {
		return []gatewayv1.RouteGroupKind{httpRouteGroupKind()}, invalid
	}
	return nil, invalid
}

func isHTTPProtocol(protocol gatewayv1.ProtocolType) bool {
	return protocol == gatewayv1.HTTPProtocolType || protocol == gatewayv1.HTTPSProtocolType
}

func httpRouteGroupKind() gatewayv1.RouteGroupKind {
	group := gatewayv1.Group(gatewayv1.GroupName)
	return gatewayv1.RouteGroupKind{Group: &group, Kind: gatewayv1.Kind("HTTPRoute")}
}

func (c Compiler) compileRoute(input Input, route *gatewayv1.HTTPRoute, plans []*listenerPlan, ownerEpoch string, resources map[config.Key]config.Resource, diagnostics *[]Diagnostic, output map[types.NamespacedName]gatewayv1.HTTPRouteStatus) {
	routeKey := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	parents := make([]parentEvaluation, 0)
	seen := make(map[string]struct{})
	routeResults := make(map[string]routeCompileResult)
	for _, parentRef := range route.Spec.ParentRefs {
		if !parentTargetsGateway(parentRef, route.Namespace, *input.Gateway) {
			continue
		}
		key := parentReferenceKey(parentRef, route.Namespace)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		evaluation := parentEvaluation{ref: parentRef}
		selected := make([]*listenerPlan, 0)
		for _, plan := range plans {
			if parentSelectsListener(parentRef, plan.spec) {
				selected = append(selected, plan)
			}
		}
		if len(selected) == 0 {
			evaluation.acceptedReason = string(gatewayv1.RouteReasonNoMatchingParent)
			evaluation.acceptedMessage = "no Gateway listener matches the parent reference"
			parents = append(parents, evaluation)
			continue
		}

		allowed := make([]*listenerPlan, 0, len(selected))
		for _, plan := range selected {
			if plan.accepted && plan.conflict == "" && plan.supportsHTTP && routeNamespaceAllowed(input, route.Namespace, input.Gateway.Namespace, plan.spec) && routeKindAllowed(plan.spec) {
				allowed = append(allowed, plan)
			}
		}
		if len(allowed) == 0 {
			evaluation.acceptedReason = string(gatewayv1.RouteReasonNotAllowedByListeners)
			evaluation.acceptedMessage = "no selected listener allows this HTTPRoute"
			parents = append(parents, evaluation)
			continue
		}

		for _, plan := range allowed {
			hostnames, intersects := intersectRouteHostnames(route.Spec.Hostnames, plan.spec.Hostname)
			if !intersects {
				continue
			}
			evaluation.attachedListeners = append(evaluation.attachedListeners, plan)
			compiled, exists := routeResults[string(plan.spec.Name)]
			if !exists {
				compiled = c.compileRouteRules(input, route, plan, hostnames, ownerEpoch, resources, diagnostics)
				routeResults[string(plan.spec.Name)] = compiled
				plan.rules = append(plan.rules, compiled.rules...)
			}
			evaluation.validRules += compiled.validRules
			evaluation.unsupportedRules += compiled.unsupportedRules
			if evaluation.resolvedReason == "" && compiled.resolvedReason != "" {
				evaluation.resolvedReason = compiled.resolvedReason
				evaluation.resolvedMessage = compiled.resolvedMessage
			}
		}
		if len(evaluation.attachedListeners) == 0 {
			evaluation.acceptedReason = string(gatewayv1.RouteReasonNoMatchingListenerHostname)
			evaluation.acceptedMessage = "no selected listener hostname intersects the HTTPRoute hostnames"
		} else if evaluation.validRules > 0 {
			evaluation.accepted = true
			evaluation.acceptedReason = string(gatewayv1.RouteReasonAccepted)
			evaluation.acceptedMessage = "HTTPRoute is accepted by at least one Gateway listener"
		} else {
			evaluation.acceptedReason = string(gatewayv1.RouteReasonUnsupportedValue)
			evaluation.acceptedMessage = "HTTPRoute has no rule supported by the HTTP Core compiler"
		}
		parents = append(parents, evaluation)
	}

	oldTargetStatuses := make(map[string]gatewayv1.RouteParentStatus)
	preserved := make([]gatewayv1.RouteParentStatus, 0, len(route.Status.Parents)+len(parents))
	for _, old := range route.Status.Parents {
		if string(old.ControllerName) == c.ControllerName && parentTargetsGateway(old.ParentRef, route.Namespace, *input.Gateway) {
			oldTargetStatuses[parentReferenceKey(old.ParentRef, route.Namespace)] = old
			continue
		}
		preserved = append(preserved, old)
	}
	for _, evaluation := range parents {
		old := oldTargetStatuses[parentReferenceKey(evaluation.ref, route.Namespace)]
		conditions := routeParentConditions(route.Generation, old.Conditions, evaluation)
		preserved = append(preserved, gatewayv1.RouteParentStatus{
			ParentRef:      evaluation.ref,
			ControllerName: gatewayv1.GatewayController(c.ControllerName),
			Conditions:     conditions,
		})
		if evaluation.accepted {
			for _, plan := range evaluation.attachedListeners {
				plan.attached[routeKey] = struct{}{}
			}
		}
	}
	if len(parents) > 0 || len(oldTargetStatuses) > 0 {
		output[routeKey] = gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: preserved}}
	}
}

func (c Compiler) compileRouteRules(input Input, route *gatewayv1.HTTPRoute, plan *listenerPlan, hostnames []string, ownerEpoch string, resources map[config.Key]config.Resource, diagnostics *[]Diagnostic) routeCompileResult {
	result := routeCompileResult{rules: make([]rankedRule, 0)}
	rules := route.Spec.Rules
	if len(rules) == 0 {
		rules = []gatewayv1.HTTPRouteRule{{}}
	}
	source := config.ObjectRef{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: route.Namespace, Name: route.Name}
	for ruleIndex, rule := range rules {
		matches := translateMatches(rule.Matches)
		supportedMatches := make([]translatedMatch, 0, len(matches))
		for _, match := range matches {
			if !match.unsupported {
				supportedMatches = append(supportedMatches, match)
				continue
			}
			result.unsupportedRules++
			addDiagnostic(diagnostics, types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, string(gatewayv1.RouteConditionAccepted), string(gatewayv1.RouteReasonUnsupportedValue), fmt.Sprintf("HTTPRoute rule %d uses an unsupported HTTP feature", ruleIndex))
		}
		ruleResources := make(map[config.Key]config.Resource)
		action, actionUnsupported, ruleUnsupported, unresolvedReason, unresolvedMessage := c.translateAction(input, route, rule, ownerEpoch, ruleResources, diagnostics)
		if result.resolvedReason == "" && unresolvedReason != "" {
			result.resolvedReason = unresolvedReason
			result.resolvedMessage = unresolvedMessage
		}
		if len(supportedMatches) == 0 {
			continue
		}
		if ruleUnsupported {
			result.unsupportedRules++
			addDiagnostic(diagnostics, types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, string(gatewayv1.RouteConditionAccepted), string(gatewayv1.RouteReasonUnsupportedValue), fmt.Sprintf("HTTPRoute rule %d uses an unsupported HTTP feature", ruleIndex))
			continue
		}
		for key, resource := range ruleResources {
			resources[key] = resource
		}
		for _, match := range supportedMatches {
			unsupported := actionUnsupported
			compiledAction := action
			if unsupported {
				compiledAction.Kind = config.ActionUnsupported
				compiledAction.UnsupportedReason = "HTTP rule uses a feature outside the HTTP Core profile"
				result.unsupportedRules++
				addDiagnostic(diagnostics, types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, string(gatewayv1.RouteConditionAccepted), string(gatewayv1.RouteReasonUnsupportedValue), fmt.Sprintf("HTTPRoute rule %d uses an unsupported HTTP feature", ruleIndex))
			} else {
				result.validRules++
			}
			ruleHostnames := hostnames
			if len(ruleHostnames) == 0 {
				ruleHostnames = []string{""}
			}
			for _, hostname := range ruleHostnames {
				compiledHostnames := []string(nil)
				candidate := rankedRule{
					rule: config.HTTPRule{
						Source:    source,
						RuleIndex: ruleIndex,
						Matches:   []config.HTTPMatch{match.value},
						Action:    compiledAction,
					},
					headerCount: match.rank.headers,
					queryCount:  match.rank.queries,
					hasMethod:   match.rank.method,
					pathExact:   match.rank.pathExact,
					pathLength:  match.rank.pathLength,
					created:     route.CreationTimestamp.Time,
					namespace:   route.Namespace,
					routeName:   route.Name,
				}
				if hostname != "" {
					compiledHostnames = []string{hostname}
					candidate.hostExact, candidate.hostLength = hostnameSpecificity(hostname)
				}
				candidate.rule.Hostnames = compiledHostnames
				result.rules = append(result.rules, candidate)
			}
		}
	}
	return result
}

// HTTPRouteBackendRefsForEndpointSets returns only backend refs from rules that
// can publish HTTP Core rows, keeping EndpointSlice updates out of rejected rules.
func HTTPRouteBackendRefsForEndpointSets(rule gatewayv1.HTTPRouteRule) []gatewayv1.HTTPBackendRef {
	hasSupportedMatch := false
	for _, match := range translateMatches(rule.Matches) {
		if !match.unsupported {
			hasSupportedMatch = true
			break
		}
	}
	if !hasSupportedMatch {
		return nil
	}

	seenRedirect, seenHeaders := false, false
	for _, filter := range rule.Filters {
		switch filter.Type {
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			if filter.RequestHeaderModifier == nil || seenHeaders {
				return nil
			}
			seenHeaders = true
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			if filter.RequestRedirect == nil || seenRedirect {
				return nil
			}
			if _, ok := translateRedirect(*filter.RequestRedirect); !ok {
				return nil
			}
			seenRedirect = true
		default:
			return nil
		}
	}
	if seenRedirect && len(rule.BackendRefs) > 0 {
		return nil
	}

	backends := make([]gatewayv1.HTTPBackendRef, 0, len(rule.BackendRefs))
	for _, backend := range rule.BackendRefs {
		if len(backend.Filters) > 0 {
			return nil
		}
		if backend.Weight != nil && *backend.Weight < 0 {
			continue
		}
		backends = append(backends, backend)
	}
	return backends
}

func (c Compiler) translateAction(input Input, route *gatewayv1.HTTPRoute, rule gatewayv1.HTTPRouteRule, ownerEpoch string, resources map[config.Key]config.Resource, diagnostics *[]Diagnostic) (config.HTTPAction, bool, bool, string, string) {
	action := config.HTTPAction{}
	unsupported := false
	ruleUnsupported := false
	var redirect *config.Redirect
	var requestHeaders *config.HeaderModifier
	seenRedirect := false
	seenHeaders := false
	for _, filter := range rule.Filters {
		switch filter.Type {
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			if filter.RequestHeaderModifier == nil || seenHeaders {
				unsupported = true
				ruleUnsupported = true
				continue
			}
			seenHeaders = true
			requestHeaders = translateHeaderModifier(*filter.RequestHeaderModifier)
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			if filter.RequestRedirect == nil || seenRedirect {
				unsupported = true
				ruleUnsupported = true
				continue
			}
			seenRedirect = true
			translated, ok := translateRedirect(*filter.RequestRedirect)
			if !ok {
				unsupported = true
				ruleUnsupported = true
			} else {
				redirect = translated
			}
		default:
			unsupported = true
			ruleUnsupported = true
		}
	}
	action.RequestHeaders = requestHeaders

	backends := make([]config.BackendRef, 0, len(rule.BackendRefs))
	resolvedReason, resolvedMessage := "", ""
	for _, backend := range rule.BackendRefs {
		weight := int32(1)
		if backend.Weight != nil {
			weight = *backend.Weight
		}
		if weight < 0 {
			backends = append(backends, config.BackendRef{Weight: weight, Valid: false, Reason: string(gatewayv1.RouteReasonUnsupportedValue)})
			unsupported = true
			continue
		}
		if len(backend.Filters) != 0 {
			unsupported = true
			ruleUnsupported = true
		}
		reference := &backend.BackendObjectReference
		namespace := route.Namespace
		if reference.Namespace != nil {
			namespace = string(*reference.Namespace)
		}
		service := input.Services[types.NamespacedName{Namespace: namespace, Name: string(reference.Name)}]
		servicePointer := &service
		if _, exists := input.Services[types.NamespacedName{Namespace: namespace, Name: string(reference.Name)}]; !exists {
			servicePointer = nil
		}
		backendResult := ResolveBackend(BackendInput{
			Target:         input.Target,
			OwnerEpoch:     ownerEpoch,
			RouteNamespace: route.Namespace,
			Reference:      reference,
			Service:        servicePointer,
			EndpointSlices: input.EndpointSlices[types.NamespacedName{Namespace: namespace, Name: string(reference.Name)}],
			Permitted:      referenceGranted(input.ReferenceGrants, namespace, string(gatewayv1.GroupName), "HTTPRoute", route.Namespace, "", "Service", string(reference.Name)),
		})
		backendResult.Reference.Weight = weight
		backends = append(backends, backendResult.Reference)
		if backendResult.Cluster != nil {
			resources[backendResult.Cluster.Key] = *backendResult.Cluster
		}
		if backendResult.EndpointSet != nil {
			resources[backendResult.EndpointSet.Key] = *backendResult.EndpointSet
		}
		if backendResult.Diagnostic != nil {
			backendResult.Diagnostic.Object = types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
			*diagnostics = append(*diagnostics, *backendResult.Diagnostic)
			if resolvedReason == "" {
				resolvedReason = backendResult.Diagnostic.Reason
				resolvedMessage = backendResult.Diagnostic.Message
			}
		}
	}
	sort.Slice(backends, func(i, j int) bool {
		if backends[i].Cluster != backends[j].Cluster {
			return backends[i].Cluster < backends[j].Cluster
		}
		if backends[i].Valid != backends[j].Valid {
			return !backends[i].Valid
		}
		if backends[i].Weight != backends[j].Weight {
			return backends[i].Weight < backends[j].Weight
		}
		return backends[i].Reason < backends[j].Reason
	})

	if seenRedirect && len(rule.BackendRefs) != 0 {
		unsupported = true
		ruleUnsupported = true
	}
	switch {
	case unsupported:
		action.Kind = config.ActionUnsupported
		action.Backends = backends
	case redirect != nil:
		action.Kind = config.ActionRedirect
		action.Redirect = redirect
	case len(backends) > 0:
		action.Kind = config.ActionForward
		action.Backends = backends
	default:
		action.Kind = config.ActionNoBackend
	}
	return action, unsupported, ruleUnsupported, resolvedReason, resolvedMessage
}

func translateMatches(matches []gatewayv1.HTTPRouteMatch) []translatedMatch {
	if len(matches) == 0 {
		return []translatedMatch{{value: config.HTTPMatch{PathType: string(gatewayv1.PathMatchPathPrefix), Path: "/"}, rank: matchRank{pathLength: 1}}}
	}
	translated := make([]translatedMatch, 0, len(matches))
	for _, match := range matches {
		item := translatedMatch{}
		pathType := gatewayv1.PathMatchPathPrefix
		path := "/"
		if match.Path != nil {
			if match.Path.Type != nil {
				pathType = *match.Path.Type
			}
			if match.Path.Value != nil {
				path = *match.Path.Value
			}
		}
		if pathType != gatewayv1.PathMatchExact && pathType != gatewayv1.PathMatchPathPrefix {
			item.unsupported = true
			pathType, path = gatewayv1.PathMatchPathPrefix, "/"
		}
		if match.Method != nil {
			item.unsupported = true
			item.rank.method = true
		}
		if len(match.QueryParams) > 0 {
			item.unsupported = true
			item.rank.queries = len(match.QueryParams)
		}
		headers := make([]config.HeaderMatch, 0, len(match.Headers))
		for _, header := range match.Headers {
			name := strings.ToLower(string(header.Name))
			matchType := gatewayv1.HeaderMatchExact
			if header.Type != nil {
				matchType = *header.Type
			}
			if matchType != gatewayv1.HeaderMatchExact {
				item.unsupported = true
			}
			headers = append(headers, config.HeaderMatch{Name: name, Value: header.Value})
		}
		sort.Slice(headers, func(i, j int) bool {
			if headers[i].Name != headers[j].Name {
				return headers[i].Name < headers[j].Name
			}
			return headers[i].Value < headers[j].Value
		})
		item.rank.headers = len(headers)
		if pathType == gatewayv1.PathMatchExact {
			item.rank.pathExact = true
		}
		item.rank.pathLength = len(path)
		item.value = config.HTTPMatch{PathType: string(pathType), Path: path, Headers: headers}
		translated = append(translated, item)
	}
	sort.Slice(translated, func(i, j int) bool {
		return lessHTTPMatch(translated[i].value, translated[j].value)
	})
	return translated
}

func lessHTTPMatch(a, b config.HTTPMatch) bool {
	if a.PathType != b.PathType {
		return a.PathType < b.PathType
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	for i := 0; i < min(len(a.Headers), len(b.Headers)); i++ {
		if a.Headers[i].Name != b.Headers[i].Name {
			return a.Headers[i].Name < b.Headers[i].Name
		}
		if a.Headers[i].Value != b.Headers[i].Value {
			return a.Headers[i].Value < b.Headers[i].Value
		}
	}
	return len(a.Headers) < len(b.Headers)
}

func translateHeaderModifier(filter gatewayv1.HTTPHeaderFilter) *config.HeaderModifier {
	modifier := &config.HeaderModifier{Add: make([]config.HeaderValue, 0, len(filter.Add)), Set: make([]config.HeaderValue, 0, len(filter.Set)), Remove: make([]string, 0, len(filter.Remove))}
	for _, header := range filter.Add {
		modifier.Add = append(modifier.Add, config.HeaderValue{Name: strings.ToLower(string(header.Name)), Value: header.Value})
	}
	for _, header := range filter.Set {
		modifier.Set = append(modifier.Set, config.HeaderValue{Name: strings.ToLower(string(header.Name)), Value: header.Value})
	}
	for _, name := range filter.Remove {
		modifier.Remove = append(modifier.Remove, strings.ToLower(name))
	}
	sort.Slice(modifier.Add, func(i, j int) bool {
		if modifier.Add[i].Name != modifier.Add[j].Name {
			return modifier.Add[i].Name < modifier.Add[j].Name
		}
		return modifier.Add[i].Value < modifier.Add[j].Value
	})
	sort.Slice(modifier.Set, func(i, j int) bool {
		if modifier.Set[i].Name != modifier.Set[j].Name {
			return modifier.Set[i].Name < modifier.Set[j].Name
		}
		return modifier.Set[i].Value < modifier.Set[j].Value
	})
	sort.Strings(modifier.Remove)
	modifier.Remove = compactStrings(modifier.Remove)
	if len(modifier.Add)+len(modifier.Set)+len(modifier.Remove) == 0 {
		return nil
	}
	return modifier
}

func translateRedirect(filter gatewayv1.HTTPRequestRedirectFilter) (*config.Redirect, bool) {
	if filter.Scheme != nil || filter.Path != nil || filter.Port != nil {
		return nil, false
	}
	status := int32(302)
	if filter.StatusCode != nil {
		status = int32(*filter.StatusCode)
	}
	if status != 301 && status != 302 {
		return nil, false
	}
	redirect := &config.Redirect{StatusCode: status}
	if filter.Hostname != nil {
		redirect.Hostname = string(*filter.Hostname)
	}
	return redirect, true
}

func routeNamespaceAllowed(input Input, routeNamespace, gatewayNamespace string, listener gatewayv1.Listener) bool {
	if listener.AllowedRoutes == nil || listener.AllowedRoutes.Namespaces == nil || listener.AllowedRoutes.Namespaces.From == nil {
		return routeNamespace == gatewayNamespace
	}
	namespaces := listener.AllowedRoutes.Namespaces
	switch *namespaces.From {
	case gatewayv1.NamespacesFromAll:
		return true
	case gatewayv1.NamespacesFromSame:
		return routeNamespace == gatewayNamespace
	case gatewayv1.NamespacesFromSelector:
		if namespaces.Selector == nil {
			return false
		}
		namespace, ok := input.Namespaces[types.NamespacedName{Name: routeNamespace}]
		if !ok {
			return false
		}
		selector, err := metav1.LabelSelectorAsSelector(namespaces.Selector)
		return err == nil && selector.Matches(labels.Set(namespace.Labels))
	default:
		return false
	}
}

func routeKindAllowed(listener gatewayv1.Listener) bool {
	kinds, _ := supportedHTTPKinds(listener)
	return len(kinds) != 0
}

func parentSelectsListener(parent gatewayv1.ParentReference, listener gatewayv1.Listener) bool {
	if parent.SectionName != nil && *parent.SectionName != listener.Name {
		return false
	}
	return parent.Port == nil || int32(*parent.Port) == int32(listener.Port)
}

func parentTargetsGateway(parent gatewayv1.ParentReference, routeNamespace string, gateway gatewayv1.Gateway) bool {
	group := string(gatewayv1.GroupName)
	if parent.Group != nil {
		group = string(*parent.Group)
	}
	kind := "Gateway"
	if parent.Kind != nil {
		kind = string(*parent.Kind)
	}
	namespace := routeNamespace
	if parent.Namespace != nil {
		namespace = string(*parent.Namespace)
	}
	return group == string(gatewayv1.GroupName) && kind == "Gateway" && namespace == gateway.Namespace && string(parent.Name) == gateway.Name
}

func parentReferenceKey(parent gatewayv1.ParentReference, routeNamespace string) string {
	group := string(gatewayv1.GroupName)
	if parent.Group != nil {
		group = string(*parent.Group)
	}
	kind := "Gateway"
	if parent.Kind != nil {
		kind = string(*parent.Kind)
	}
	namespace := routeNamespace
	if parent.Namespace != nil {
		namespace = string(*parent.Namespace)
	}
	section, port := "", ""
	if parent.SectionName != nil {
		section = string(*parent.SectionName)
	}
	if parent.Port != nil {
		port = fmt.Sprint(*parent.Port)
	}
	return strings.Join([]string{group, kind, namespace, string(parent.Name), section, port}, "|")
}

func referenceGranted(grants []gatewayv1.ReferenceGrant, targetNamespace, fromGroup, fromKind, fromNamespace, toGroup, toKind, toName string) bool {
	for _, grant := range grants {
		if grant.Namespace != targetNamespace {
			continue
		}
		fromAllowed, toAllowed := false, false
		for _, from := range grant.Spec.From {
			if string(from.Group) == fromGroup && string(from.Kind) == fromKind && string(from.Namespace) == fromNamespace {
				fromAllowed = true
				break
			}
		}
		if !fromAllowed {
			continue
		}
		for _, to := range grant.Spec.To {
			if string(to.Group) != toGroup || string(to.Kind) != toKind {
				continue
			}
			if to.Name == nil || string(*to.Name) == toName {
				toAllowed = true
				break
			}
		}
		if toAllowed {
			return true
		}
	}
	return false
}

func intersectRouteHostnames(routeHostnames []gatewayv1.Hostname, listenerHostname *gatewayv1.Hostname) ([]string, bool) {
	listener := ""
	if listenerHostname != nil {
		listener = strings.ToLower(string(*listenerHostname))
	}
	if len(routeHostnames) == 0 {
		if listener == "" {
			return nil, true
		}
		return []string{listener}, true
	}
	result := make([]string, 0, len(routeHostnames))
	seen := make(map[string]struct{}, len(routeHostnames))
	for _, value := range routeHostnames {
		route := strings.ToLower(string(value))
		intersection, ok := intersectHostname(listener, route)
		if !ok {
			continue
		}
		if _, exists := seen[intersection]; exists {
			continue
		}
		seen[intersection] = struct{}{}
		result = append(result, intersection)
	}
	sort.Strings(result)
	return result, len(result) > 0
}

func intersectHostname(a, b string) (string, bool) {
	if a == "" {
		return b, true
	}
	if b == "" {
		return a, true
	}
	if a == b {
		return a, true
	}
	if strings.HasPrefix(a, "*.") && !strings.HasPrefix(b, "*.") && wildcardMatches(a, b) {
		return b, true
	}
	if strings.HasPrefix(b, "*.") && !strings.HasPrefix(a, "*.") && wildcardMatches(b, a) {
		return a, true
	}
	if strings.HasPrefix(a, "*.") && strings.HasPrefix(b, "*.") {
		suffixA, suffixB := strings.TrimPrefix(a, "*."), strings.TrimPrefix(b, "*.")
		if strings.HasSuffix(suffixA, "."+suffixB) {
			return a, true
		}
		if strings.HasSuffix(suffixB, "."+suffixA) {
			return b, true
		}
	}
	return "", false
}

func wildcardMatches(pattern, hostname string) bool {
	suffix := strings.TrimPrefix(pattern, "*.")
	return len(hostname) > len(suffix)+1 && strings.HasSuffix(hostname, "."+suffix)
}

func markListenerConflicts(plans []*listenerPlan, gateway *gatewayv1.Gateway, diagnostics *[]Diagnostic) {
	for i := 0; i < len(plans); i++ {
		for j := i + 1; j < len(plans); j++ {
			a, b := plans[i], plans[j]
			if !a.supportsHTTP || !b.supportsHTTP || a.spec.Port != b.spec.Port {
				continue
			}
			if a.spec.Protocol == b.spec.Protocol {
				hostA, hostB := listenerHostname(a.spec), listenerHostname(b.spec)
				if a.protocol == config.ListenerHTTPS && hostnamesOverlap(hostA, hostB) {
					a.tlsOverlap, b.tlsOverlap = true, true
				}
				if hostA == hostB {
					markConflict(a, b, string(gatewayv1.ListenerReasonHostnameConflict), gateway, diagnostics)
					continue
				}
				continue
			}
		}
	}
}

func markConflict(a, b *listenerPlan, reason string, gateway *gatewayv1.Gateway, diagnostics *[]Diagnostic) {
	a.conflict, b.conflict = reason, reason
	a.reject(string(gatewayv1.ListenerReasonPortUnavailable), "listener conflicts with another listener on the same port")
	b.reject(string(gatewayv1.ListenerReasonPortUnavailable), "listener conflicts with another listener on the same port")
	message := fmt.Sprintf("listeners %q and %q conflict on port %d", a.spec.Name, b.spec.Name, a.spec.Port)
	addDiagnostic(diagnostics, types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, string(gatewayv1.ListenerConditionConflicted), reason, message)
}

func hostnamesOverlap(a, b string) bool {
	_, ok := intersectHostname(a, b)
	return ok
}

func (c Compiler) listenerStatus(gateway *gatewayv1.Gateway, plan *listenerPlan, previous *gatewayv1.ListenerStatus) gatewayv1.ListenerStatus {
	oldConditions := []metav1.Condition(nil)
	if previous != nil {
		oldConditions = previous.Conditions
	}
	acceptedStatus, acceptedReason, acceptedMessage := metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), "Listener is valid"
	if !plan.accepted || plan.conflict != "" {
		acceptedStatus = metav1.ConditionFalse
		acceptedReason, acceptedMessage = plan.acceptReason, plan.acceptMessage
		if plan.conflict != "" {
			acceptedReason, acceptedMessage = string(gatewayv1.ListenerReasonPortUnavailable), "listener conflicts with another listener on the same port and hostname"
		}
	}
	resolvedStatus, resolvedReason, resolvedMessage := metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), "Listener references are resolved"
	if !plan.resolved {
		resolvedStatus, resolvedReason, resolvedMessage = metav1.ConditionFalse, plan.resolvedReason, plan.resolvedMessage
	}
	programmedStatus, programmedReason, programmedMessage := pendingProgrammed(gateway.Generation, findCondition(oldConditions, string(gatewayv1.ListenerConditionProgrammed)))
	if acceptedStatus == metav1.ConditionFalse {
		programmedStatus, programmedReason, programmedMessage = metav1.ConditionFalse, string(gatewayv1.ListenerReasonInvalid), "Listener is not accepted"
	}
	conditions := mergeConditions(oldConditions, []metav1.Condition{
		newCondition(string(gatewayv1.ListenerConditionAccepted), acceptedStatus, acceptedReason, acceptedMessage, gateway.Generation, oldConditions),
		newCondition(string(gatewayv1.ListenerConditionProgrammed), programmedStatus, programmedReason, programmedMessage, gateway.Generation, oldConditions),
		newCondition(string(gatewayv1.ListenerConditionResolvedRefs), resolvedStatus, resolvedReason, resolvedMessage, gateway.Generation, oldConditions),
	}, map[string]bool{
		string(gatewayv1.ListenerConditionAccepted):             true,
		string(gatewayv1.ListenerConditionProgrammed):           true,
		string(gatewayv1.ListenerConditionResolvedRefs):         true,
		string(gatewayv1.ListenerConditionConflicted):           true,
		string(gatewayv1.ListenerConditionOverlappingTLSConfig): true,
	})
	conflictStatus, conflictReason, conflictMessage := metav1.ConditionFalse, string(gatewayv1.ListenerReasonNoConflicts), "Listener has no conflicts"
	if plan.conflict != "" {
		conflictStatus, conflictReason = metav1.ConditionTrue, plan.conflict
		conflictMessage = "listener conflicts with another listener on the same port and hostname"
	}
	conditions = mergeConditions(conditions, []metav1.Condition{
		newCondition(string(gatewayv1.ListenerConditionConflicted), conflictStatus, conflictReason, conflictMessage, gateway.Generation, oldConditions),
	}, map[string]bool{string(gatewayv1.ListenerConditionConflicted): true})
	if plan.tlsOverlap {
		conditions = mergeConditions(conditions, []metav1.Condition{
			newCondition(string(gatewayv1.ListenerConditionOverlappingTLSConfig), metav1.ConditionTrue, string(gatewayv1.ListenerReasonOverlappingHostnames), "listener hostname overlaps another HTTPS listener on the same port", gateway.Generation, oldConditions),
		}, map[string]bool{string(gatewayv1.ListenerConditionOverlappingTLSConfig): true})
	}
	kinds, _ := supportedHTTPKinds(plan.spec)
	return gatewayv1.ListenerStatus{
		Name:           plan.spec.Name,
		SupportedKinds: kinds,
		AttachedRoutes: int32(len(plan.attached)),
		Conditions:     conditions,
	}
}

func (c Compiler) gatewayStatus(gateway *gatewayv1.Gateway, listeners []gatewayv1.ListenerStatus, acceptedListeners int, invalidListeners bool) gatewayv1.GatewayStatus {
	old := gateway.Status.Conditions
	acceptedStatus, acceptedReason, acceptedMessage := metav1.ConditionTrue, string(gatewayv1.GatewayReasonAccepted), "Gateway has at least one accepted HTTP listener"
	if acceptedListeners == 0 {
		acceptedStatus, acceptedReason, acceptedMessage = metav1.ConditionFalse, string(gatewayv1.GatewayReasonListenersNotValid), "Gateway has no accepted HTTP listeners"
	} else if invalidListeners {
		acceptedReason = string(gatewayv1.GatewayReasonListenersNotValid)
		acceptedMessage = "Gateway has one or more invalid listeners"
	}
	programmedStatus, programmedReason, programmedMessage := pendingProgrammed(gateway.Generation, findCondition(old, string(gatewayv1.GatewayConditionProgrammed)))
	if acceptedStatus == metav1.ConditionFalse {
		programmedStatus, programmedReason, programmedMessage = metav1.ConditionFalse, string(gatewayv1.GatewayReasonInvalid), "Gateway has no accepted listeners"
	}
	conditions := mergeConditions(old, []metav1.Condition{
		newCondition(string(gatewayv1.GatewayConditionAccepted), acceptedStatus, acceptedReason, acceptedMessage, gateway.Generation, old),
		newCondition(string(gatewayv1.GatewayConditionProgrammed), programmedStatus, programmedReason, programmedMessage, gateway.Generation, old),
	}, map[string]bool{string(gatewayv1.GatewayConditionAccepted): true, string(gatewayv1.GatewayConditionProgrammed): true})
	status := gateway.Status
	status.Conditions = conditions
	status.Listeners = listeners
	return status
}

func routeParentConditions(generation int64, old []metav1.Condition, evaluation parentEvaluation) []metav1.Condition {
	acceptedStatus := metav1.ConditionFalse
	acceptedReason, acceptedMessage := evaluation.acceptedReason, evaluation.acceptedMessage
	if evaluation.accepted {
		acceptedStatus, acceptedReason, acceptedMessage = metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted by at least one Gateway listener"
	}
	desired := []metav1.Condition{newCondition(string(gatewayv1.RouteConditionAccepted), acceptedStatus, acceptedReason, acceptedMessage, generation, old)}
	if len(evaluation.attachedListeners) > 0 {
		resolvedStatus, reason, message := metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), "HTTPRoute references are resolved"
		if evaluation.resolvedReason != "" {
			resolvedStatus, reason, message = metav1.ConditionFalse, evaluation.resolvedReason, evaluation.resolvedMessage
		}
		desired = append(desired, newCondition(string(gatewayv1.RouteConditionResolvedRefs), resolvedStatus, reason, message, generation, old))
		if evaluation.validRules > 0 && evaluation.unsupportedRules > 0 {
			desired = append(desired, newCondition(string(gatewayv1.RouteConditionPartiallyInvalid), metav1.ConditionTrue, string(gatewayv1.RouteReasonUnsupportedValue), "Some HTTPRoute rules use unsupported HTTP features", generation, old))
		}
	}
	return mergeConditions(old, desired, map[string]bool{
		string(gatewayv1.RouteConditionAccepted):         true,
		string(gatewayv1.RouteConditionResolvedRefs):     true,
		string(gatewayv1.RouteConditionPartiallyInvalid): true,
	})
}

func previousListenerStatus(statuses []gatewayv1.ListenerStatus, name gatewayv1.SectionName) *gatewayv1.ListenerStatus {
	for i := range statuses {
		if statuses[i].Name == name {
			return &statuses[i]
		}
	}
	return nil
}

func newCondition(conditionType string, status metav1.ConditionStatus, reason, message string, generation int64, previous []metav1.Condition) metav1.Condition {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
	if old := findCondition(previous, conditionType); old != nil && old.Status == status {
		condition.LastTransitionTime = old.LastTransitionTime
	}
	return condition
}

func pendingProgrammed(generation int64, previous *metav1.Condition) (metav1.ConditionStatus, string, string) {
	if previous != nil && previous.ObservedGeneration == generation {
		return previous.Status, previous.Reason, previous.Message
	}
	return metav1.ConditionUnknown, string(gatewayv1.GatewayReasonPending), "Desired configuration has not been observed as active"
}

func mergeConditions(previous, desired []metav1.Condition, owned map[string]bool) []metav1.Condition {
	result := make([]metav1.Condition, 0, len(previous)+len(desired))
	for _, condition := range previous {
		if !owned[condition.Type] {
			result = append(result, condition)
		}
	}
	for _, condition := range desired {
		if old := findCondition(previous, condition.Type); old != nil {
			condition.LastTransitionTime = old.LastTransitionTime
			if old.Status != condition.Status {
				condition.LastTransitionTime = metav1.NewTime(time.Now())
			}
		}
		result = append(result, condition)
	}
	return result
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func gatewayObjectRef(gateway gatewayv1.Gateway) config.ObjectRef {
	return config.ObjectRef{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway", Namespace: gateway.Namespace, Name: gateway.Name, UID: string(gateway.UID), Generation: gateway.Generation}
}

func listenerHostname(listener gatewayv1.Listener) string {
	if listener.Hostname == nil {
		return ""
	}
	return strings.ToLower(string(*listener.Hostname))
}

func hostnameSpecificity(hostname string) (int, int) {
	if strings.HasPrefix(hostname, "*.") {
		return len(hostname) - 2, len(hostname)
	}
	return len(hostname), len(hostname)
}

func lessRankedRule(a, b rankedRule) bool {
	if a.hostExact != b.hostExact {
		return a.hostExact > b.hostExact
	}
	if a.hostLength != b.hostLength {
		return a.hostLength > b.hostLength
	}
	if a.pathExact != b.pathExact {
		return a.pathExact
	}
	if a.pathLength != b.pathLength {
		return a.pathLength > b.pathLength
	}
	if a.hasMethod != b.hasMethod {
		return a.hasMethod
	}
	if a.headerCount != b.headerCount {
		return a.headerCount > b.headerCount
	}
	if a.queryCount != b.queryCount {
		return a.queryCount > b.queryCount
	}
	if !a.created.Equal(b.created) {
		if a.created.IsZero() {
			return false
		}
		if b.created.IsZero() {
			return true
		}
		return a.created.Before(b.created)
	}
	if a.namespace != b.namespace {
		return a.namespace < b.namespace
	}
	if a.routeName != b.routeName {
		return a.routeName < b.routeName
	}
	if a.rule.RuleIndex != b.rule.RuleIndex {
		return a.rule.RuleIndex < b.rule.RuleIndex
	}
	if len(a.rule.Matches) == 0 || len(b.rule.Matches) == 0 {
		return len(a.rule.Matches) < len(b.rule.Matches)
	}
	return lessHTTPMatch(a.rule.Matches[0], b.rule.Matches[0])
}

func addDiagnostic(diagnostics *[]Diagnostic, object types.NamespacedName, condition, reason, message string) {
	*diagnostics = append(*diagnostics, Diagnostic{Object: object, Condition: condition, Reason: reason, Message: message})
}

func dedupeDiagnostics(input []Diagnostic) []Diagnostic {
	sort.Slice(input, func(i, j int) bool {
		a, b := input[i], input[j]
		if a.Object.Namespace != b.Object.Namespace {
			return a.Object.Namespace < b.Object.Namespace
		}
		if a.Object.Name != b.Object.Name {
			return a.Object.Name < b.Object.Name
		}
		if a.Condition != b.Condition {
			return a.Condition < b.Condition
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Message < b.Message
	})
	result := input[:0]
	for _, diagnostic := range input {
		if len(result) == 0 || result[len(result)-1] != diagnostic {
			result = append(result, diagnostic)
		}
	}
	return result
}

func compactStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func secretResourceName(namespace, name string) string {
	identity := namespace + "\x00" + name
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("secret-%s-%s-%s", namespace, name, hex.EncodeToString(sum[:16]))
}
