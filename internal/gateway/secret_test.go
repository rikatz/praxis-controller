package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTLSSecretInfoValidatesAndDiscardsSecretData(t *testing.T) {
	certificate, key := testTLSKeyPair(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{UID: "secret-uid", ResourceVersion: "7"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certificate,
			corev1.TLSPrivateKeyKey: key,
		},
	}

	info := TLSSecretInfo(secret)
	if !info.Valid || info.UID != "secret-uid" || info.ResourceVersion != "7" {
		t.Fatalf("valid TLS Secret was not reduced to valid metadata: %+v", info)
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(key)) || strings.Contains(string(encoded), string(certificate)) {
		t.Fatal("TLS Secret data was retained in the compiler input")
	}

	wrongType := secret.DeepCopy()
	wrongType.Type = corev1.SecretTypeOpaque
	if TLSSecretInfo(wrongType).Valid {
		t.Fatal("non-TLS Secret was accepted")
	}

	malformed := secret.DeepCopy()
	malformed.Data[corev1.TLSCertKey] = []byte("canary-malformed-certificate")
	if TLSSecretInfo(malformed).Valid {
		t.Fatal("malformed certificate was accepted")
	}

	otherCertificate, _ := testTLSKeyPair(t)
	mismatched := secret.DeepCopy()
	mismatched.Data[corev1.TLSCertKey] = otherCertificate
	if TLSSecretInfo(mismatched).Valid {
		t.Fatal("certificate and private key mismatch was accepted")
	}
}

func testTLSKeyPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
