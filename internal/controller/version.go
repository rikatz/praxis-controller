package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

var requiredGatewayAPICRDs = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
	"referencegrants.gateway.networking.k8s.io",
}

func gatewayAPICRDBundleSupported(ctx context.Context, reader client.Reader) (bool, string, error) {
	if reader == nil {
		return false, "Gateway API CRD metadata reader is unavailable", fmt.Errorf("Gateway API CRD metadata reader is required")
	}
	detected := make([]string, 0, len(requiredGatewayAPICRDs))
	supported := true
	for _, name := range requiredGatewayAPICRDs {
		metadata := &metav1.PartialObjectMetadata{}
		metadata.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "apiextensions.k8s.io",
			Version: "v1",
			Kind:    "CustomResourceDefinition",
		})
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, metadata); err != nil {
			if apierrors.IsNotFound(err) {
				detected = append(detected, name+"=missing")
				supported = false
				continue
			}
			return false, "", fmt.Errorf("read Gateway API CRD metadata %q: %w", name, err)
		}
		version := metadata.Annotations[gatewayconsts.BundleVersionAnnotation]
		if version == "" {
			detected = append(detected, name+"=unannotated")
			supported = false
			continue
		}
		detected = append(detected, name+"="+version)
		if version != gatewayconsts.BundleVersion {
			supported = false
		}
	}
	sort.Strings(detected)
	message := fmt.Sprintf("Gateway API CRD bundle detected [%s]; supported bundle is %s", strings.Join(detected, ", "), gatewayconsts.BundleVersion)
	return supported, message, nil
}
