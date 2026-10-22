// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses;httproutes;referencegrants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status;gateways/status;httproutes/status,verbs=patch
// Secrets use a metadata-only informer, but core RBAC cannot grant metadata-only reads.
// The issued get/list/watch permissions also allow a holder of this credential to read Secret.Data.
// +kubebuilder:rbac:groups="",resources=namespaces;services;secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:urls=/metrics,verbs=get,roleName=metrics-reader

// Package controller reconciles Gateway API resources into Praxis configuration.
package controller
