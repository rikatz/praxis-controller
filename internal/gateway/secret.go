package gateway

import (
	"crypto/tls"

	corev1 "k8s.io/api/core/v1"
)

// TLSSecretInfo returns only the Secret identity, rotation token and validation result.
func TLSSecretInfo(secret *corev1.Secret) TLSSecret {
	if secret == nil {
		return TLSSecret{}
	}

	info := TLSSecret{
		UID:             string(secret.UID),
		ResourceVersion: secret.ResourceVersion,
	}
	if secret.Type != corev1.SecretTypeTLS {
		return info
	}
	certificate, certificateExists := secret.Data[corev1.TLSCertKey]
	privateKey, privateKeyExists := secret.Data[corev1.TLSPrivateKeyKey]
	if !certificateExists || !privateKeyExists || len(certificate) == 0 || len(privateKey) == 0 {
		return info
	}
	_, err := tls.X509KeyPair(certificate, privateKey)
	info.Valid = err == nil
	return info
}
