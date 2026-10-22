package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/praxis-proxy/controller/internal/config"
	"github.com/praxis-proxy/controller/internal/delivery"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	envtestKubernetesVersion = "1.37.0"
	gatewayAPIModuleVersion  = "v1.6.3"
	envtestControllerName    = "gateway.example.test/controller"
)

var (
	suiteContext    context.Context
	suiteCancel     context.CancelFunc
	testEnv         *envtest.Environment
	testManager     ctrl.Manager
	testClient      client.Client
	managerDone     chan struct{}
	envtestStarted  bool
	testDelivery    *simulatedConfigurator
	testProvisioner *simulatedProvisioner
)

func TestControllerEnvtest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway API controller envtest")
}

var _ = BeforeSuite(func() {
	SetDefaultEventuallyTimeout(25 * time.Second)
	SetDefaultEventuallyPollingInterval(100 * time.Millisecond)
	ctrl.SetLogger(logr.Discard())

	crdDir := gatewayAPICRDDirectory()
	testEnv = &envtest.Environment{
		DownloadBinaryAssets:        true,
		DownloadBinaryAssetsVersion: envtestKubernetesVersion,
		BinaryAssetsDirectory:       filepath.Join(os.TempDir(), "praxis-controller-envtest"),
		CRDDirectoryPaths:           []string{crdDir},
		ErrorIfCRDPathMissing:       true,
	}
	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	envtestStarted = true

	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(discoveryv1.AddToScheme(scheme)).To(Succeed())
	Expect(gatewayv1.Install(scheme)).To(Succeed())

	testDelivery = newSimulatedConfigurator()
	testProvisioner = &simulatedProvisioner{}
	testManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(SetupWithManager(testManager, testDelivery, testProvisioner, envtestControllerName, "envtest-owner-epoch")).To(Succeed())

	suiteContext, suiteCancel = context.WithCancel(context.Background())
	managerDone = make(chan struct{})
	go func() {
		defer close(managerDone)
		Expect(testManager.Start(suiteContext)).To(Succeed())
	}()
	Expect(testManager.GetCache().WaitForCacheSync(suiteContext)).To(BeTrue())
	testClient = testManager.GetClient()
})

var _ = AfterSuite(func() {
	if suiteCancel != nil {
		suiteCancel()
	}
	if managerDone != nil {
		Eventually(managerDone, 30*time.Second).Should(BeClosed())
		Expect(controllerPanicCount()).To(BeZero())
	}
	if envtestStarted {
		Expect(testEnv.Stop()).To(Succeed())
	}
})

var _ = Describe("Gateway reconciliation", func() {
	It("accepts a class and reports Programmed only from an active simulated receipt", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "controller-envtest-"}}
		Expect(testClient.Create(suiteContext, namespace)).To(Succeed())

		class := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-class"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(envtestControllerName)},
		}
		Expect(testClient.Create(suiteContext, class)).To(Succeed())
		Eventually(func() error {
			current := &gatewayv1.GatewayClass{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(class), current); err != nil {
				return err
			}
			condition := findCondition(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
			if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != current.Generation {
				return fmt.Errorf("GatewayClass is not currently accepted")
			}
			if !conditionIs(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion), metav1.ConditionTrue, current.Generation) {
				return fmt.Errorf("GatewayClass does not report the supported Gateway API bundle")
			}
			return nil
		}).Should(Succeed())

		gatewayObject := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: namespace.Name},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: gatewayv1.ObjectName(class.Name),
				Listeners: []gatewayv1.Listener{{
					Name:     "web",
					Port:     gatewayv1.PortNumber(8080),
					Protocol: gatewayv1.HTTPProtocolType,
				}},
			},
		}
		Expect(testClient.Create(suiteContext, gatewayObject)).To(Succeed())

		Eventually(func() error {
			current := &gatewayv1.Gateway{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(gatewayObject), current); err != nil {
				return err
			}
			if !containsString(current.Finalizers, GatewayFinalizer) {
				return fmt.Errorf("Gateway finalizer is missing")
			}
			if !conditionIs(current.Status.Conditions, string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue, current.Generation) {
				return fmt.Errorf("Gateway is not currently accepted")
			}
			programmed := findCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
			if programmed == nil || programmed.Status != metav1.ConditionTrue || programmed.ObservedGeneration != current.Generation || programmed.Message != "Mock configuration is active" {
				return fmt.Errorf("Gateway is not programmed from an active simulated receipt")
			}
			if len(current.Status.Addresses) != 0 {
				return fmt.Errorf("simulated target unexpectedly published addresses")
			}
			return nil
		}).Should(Succeed())
		Expect(testDelivery.appliedCount()).To(BeNumerically(">", 0))
	})

	It("rejects an unsupported GatewayClass parametersRef without programming its Gateway", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "controller-invalid-class-"}}
		Expect(testClient.Create(suiteContext, namespace)).To(Succeed())
		before := testProvisioner.ensureCount()
		class := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-invalid-params"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: envtestControllerName,
				ParametersRef:  &gatewayv1.ParametersReference{Group: "example.test", Kind: "GatewayConfig", Name: "unused"},
			},
		}
		Expect(testClient.Create(suiteContext, class)).To(Succeed())
		Eventually(func() error {
			current := &gatewayv1.GatewayClass{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(class), current); err != nil {
				return err
			}
			condition := findCondition(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != string(gatewayv1.GatewayClassReasonInvalidParameters) {
				return fmt.Errorf("GatewayClass parametersRef was not rejected")
			}
			if !conditionIs(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion), metav1.ConditionTrue, current.Generation) {
				return fmt.Errorf("GatewayClass should report the installed supported bundle")
			}
			return nil
		}).Should(Succeed())

		gatewayObject := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "unused-target", Namespace: namespace.Name},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: gatewayv1.ObjectName(class.Name),
				Listeners:        []gatewayv1.Listener{{Name: "web", Port: 8080, Protocol: gatewayv1.HTTPProtocolType}},
			},
		}
		Expect(testClient.Create(suiteContext, gatewayObject)).To(Succeed())
		Eventually(func() error {
			current := &gatewayv1.Gateway{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(gatewayObject), current); err != nil {
				return err
			}
			if !conditionIs(current.Status.Conditions, string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, current.Generation) {
				return fmt.Errorf("Gateway should be unaccepted while its class parameters are unsupported")
			}
			if !conditionIs(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, current.Generation) {
				return fmt.Errorf("Gateway should not be programmed")
			}
			if containsString(current.Finalizers, GatewayFinalizer) {
				return fmt.Errorf("unsupported Gateway unexpectedly acquired a target cleanup finalizer")
			}
			return nil
		}).Should(Succeed())
		Expect(testProvisioner.ensureCount()).To(Equal(before))
	})

	It("updates only EndpointSet resources for slice churn and withdraws stale Gateway status", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "controller-endpoints-"}}
		Expect(testClient.Create(suiteContext, namespace)).To(Succeed())

		class := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-endpoints-class"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(envtestControllerName)},
		}
		Expect(testClient.Create(suiteContext, class)).To(Succeed())
		Eventually(func() bool {
			current := &gatewayv1.GatewayClass{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(class), current); err != nil {
				return false
			}
			return conditionIs(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted), metav1.ConditionTrue, current.Generation) &&
				conditionIs(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion), metav1.ConditionTrue, current.Generation)
		}).Should(BeTrue())

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
				Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP,
			}}},
		}
		Expect(testClient.Create(suiteContext, service)).To(Succeed())
		invalidCert := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid-cert", Namespace: namespace.Name},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				corev1.TLSCertKey:       []byte("canary-invalid-certificate"),
				corev1.TLSPrivateKeyKey: []byte("canary-invalid-private-key"),
			},
		}
		Expect(testClient.Create(suiteContext, invalidCert)).To(Succeed())

		portName := "http"
		portProtocol := corev1.ProtocolTCP
		endpointPort := int32(8080)
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backend-slice",
				Namespace: namespace.Name,
				Labels:    map[string]string{EndpointSliceServiceLabel: service.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Service", Name: service.Name, UID: service.UID,
				}},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Ports: []discoveryv1.EndpointPort{{
				Name: &portName, Protocol: &portProtocol, Port: &endpointPort,
			}},
		}
		Expect(testClient.Create(suiteContext, slice)).To(Succeed())

		gatewayObject := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: namespace.Name},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: gatewayv1.ObjectName(class.Name),
				Listeners: []gatewayv1.Listener{
					{Name: "web", Port: 8080, Protocol: gatewayv1.HTTPProtocolType},
					{
						Name: "secure", Port: 8443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "invalid-cert"}}},
					},
				},
			},
		}
		testProvisioner.setAddresses([]string{"192.0.2.55"})
		Expect(testClient.Create(suiteContext, gatewayObject)).To(Succeed())
		servicePort := gatewayv1.PortNumber(8080)
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: namespace.Name},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(gatewayObject.Name)}}},
				Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(service.Name), Port: &servicePort},
				}}}},
			},
		}
		Expect(testClient.Create(suiteContext, route)).To(Succeed())

		var target config.Target
		Eventually(func() error {
			currentGateway := &gatewayv1.Gateway{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(gatewayObject), currentGateway); err != nil {
				return err
			}
			currentRoute := &gatewayv1.HTTPRoute{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(route), currentRoute); err != nil {
				return err
			}
			if !conditionIs(currentGateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionTrue, currentGateway.Generation) {
				return fmt.Errorf("Gateway is not programmed")
			}
			secureListener := findListenerStatus(currentGateway.Status.Listeners, "secure")
			if secureListener == nil || !conditionIs(secureListener.Conditions, string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionFalse, currentGateway.Generation) {
				return fmt.Errorf("malformed TLS Secret did not produce an unresolved listener")
			}
			statusJSON, err := json.Marshal(currentGateway.Status)
			if err != nil || strings.Contains(string(statusJSON), "canary-invalid-certificate") {
				return fmt.Errorf("TLS Secret data appeared in Gateway status")
			}
			for _, change := range testDelivery.changesSince(0) {
				changeJSON, err := json.Marshal(change)
				if err != nil || strings.Contains(string(changeJSON), "canary-invalid-certificate") {
					return fmt.Errorf("TLS Secret data appeared in a mock configuration change")
				}
			}
			if len(currentGateway.Status.Addresses) != 1 || currentGateway.Status.Addresses[0].Value != "192.0.2.55" || currentGateway.Status.Addresses[0].Type == nil || *currentGateway.Status.Addresses[0].Type != gatewayv1.IPAddressType {
				return fmt.Errorf("Gateway address was not copied from the Provisioner")
			}
			if len(currentRoute.Status.Parents) == 0 || !conditionIs(currentRoute.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, currentRoute.Generation) {
				return fmt.Errorf("HTTPRoute is not attached")
			}
			target = targetForGateway(currentGateway)
			return nil
		}).Should(Succeed())
		oldSecretResourceVersion := invalidCert.ResourceVersion
		oldSecretGeneration := invalidCert.Generation
		certificate, privateKey, err := envtestTLSKeyPair()
		Expect(err).NotTo(HaveOccurred())
		invalidCert.Data = map[string][]byte{corev1.TLSCertKey: certificate, corev1.TLSPrivateKeyKey: privateKey}
		Expect(testClient.Update(suiteContext, invalidCert)).To(Succeed())
		if invalidCert.ResourceVersion == oldSecretResourceVersion || invalidCert.Generation != oldSecretGeneration {
			Fail("Secret data update did not change only its resourceVersion")
		}
		Eventually(func() error {
			currentGateway := &gatewayv1.Gateway{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(gatewayObject), currentGateway); err != nil {
				return err
			}
			secureListener := findListenerStatus(currentGateway.Status.Listeners, "secure")
			if secureListener == nil || !conditionIs(secureListener.Conditions, string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionTrue, currentGateway.Generation) {
				return fmt.Errorf("Secret data change did not resolve the HTTPS listener")
			}
			statusJSON, err := json.Marshal(currentGateway.Status)
			if err != nil || strings.Contains(string(statusJSON), string(certificate)) || strings.Contains(string(statusJSON), string(privateKey)) {
				return fmt.Errorf("TLS certificate material appeared in Gateway status")
			}
			for _, change := range testDelivery.changesSince(0) {
				changeJSON, err := json.Marshal(change)
				if err != nil || strings.Contains(string(changeJSON), string(certificate)) || strings.Contains(string(changeJSON), string(privateKey)) {
					return fmt.Errorf("TLS certificate material appeared in a mock configuration change")
				}
			}
			return nil
		}).Should(Succeed())
		testProvisioner.setAddresses(nil)

		baseline := testDelivery.appliedCount()
		currentSlice := &discoveryv1.EndpointSlice{}
		Expect(testClient.Get(suiteContext, client.ObjectKeyFromObject(slice), currentSlice)).To(Succeed())
		ready := true
		currentSlice.Endpoints = []discoveryv1.Endpoint{{
			Addresses:  []string{"192.0.2.44"},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}}
		Expect(testClient.Update(suiteContext, currentSlice)).To(Succeed())
		Eventually(func() error {
			changes := testDelivery.changesSince(baseline)
			if len(changes) == 0 {
				return fmt.Errorf("EndpointSlice update produced no configuration change")
			}
			for _, change := range changes {
				if len(change.Upserts)+len(change.Deletes) == 0 {
					return fmt.Errorf("EndpointSlice update emitted an empty change set")
				}
				for _, resource := range change.Upserts {
					if resource.Key.Kind != config.KindEndpointSet {
						return fmt.Errorf("EndpointSlice update upserted %s", resource.Key.Kind)
					}
				}
				for _, key := range change.Deletes {
					if key.Kind != config.KindEndpointSet {
						return fmt.Errorf("EndpointSlice update deleted %s", key.Kind)
					}
				}
			}
			return nil
		}).Should(Succeed())

		classBefore := testProvisioner.releaseCount()
		currentClass := &gatewayv1.GatewayClass{}
		Expect(testClient.Get(suiteContext, client.ObjectKeyFromObject(class), currentClass)).To(Succeed())
		currentClass.Spec.ParametersRef = &gatewayv1.ParametersReference{Group: "example.test", Kind: "GatewayConfig", Name: "unsupported"}
		Expect(testClient.Update(suiteContext, currentClass)).To(Succeed())
		Eventually(func() error {
			currentGateway := &gatewayv1.Gateway{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(gatewayObject), currentGateway); err != nil {
				return err
			}
			if !conditionIs(currentGateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, currentGateway.Generation) ||
				!conditionIs(currentGateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, currentGateway.Generation) {
				return fmt.Errorf("Gateway did not report the unsupported class")
			}
			if len(currentGateway.Status.Addresses) != 0 || len(currentGateway.Status.Listeners) != 0 {
				return fmt.Errorf("Gateway retained stale addresses or listener status")
			}
			currentRoute := &gatewayv1.HTTPRoute{}
			if err := testClient.Get(suiteContext, client.ObjectKeyFromObject(route), currentRoute); err != nil {
				return err
			}
			for _, parent := range currentRoute.Status.Parents {
				if string(parent.ControllerName) == envtestControllerName && parentRefTargetsGateway(parent.ParentRef, currentRoute.Namespace, currentGateway) {
					return fmt.Errorf("HTTPRoute retained this Gateway parent status")
				}
			}
			observation, err := testDelivery.Observe(suiteContext, target)
			if err != nil || len(observedPraxisKeys(target, observation)) != 0 {
				return fmt.Errorf("Praxis resources were not withdrawn")
			}
			if testProvisioner.releaseCount() <= classBefore {
				return fmt.Errorf("Gateway target was not released")
			}
			return nil
		}).Should(Succeed())
	})
})

func findListenerStatus(listeners []gatewayv1.ListenerStatus, name gatewayv1.SectionName) *gatewayv1.ListenerStatus {
	for i := range listeners {
		if listeners[i].Name == name {
			return &listeners[i]
		}
	}
	return nil
}

func envtestTLSKeyPair() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certificate, privateKey, nil
}

func gatewayAPICRDDirectory() string {
	output, err := exec.Command("go", "env", "GOMODCACHE").Output()
	Expect(err).NotTo(HaveOccurred())
	moduleCache := strings.TrimSpace(string(output))
	return filepath.Join(moduleCache, "sigs.k8s.io", "gateway-api@"+gatewayAPIModuleVersion, "config", "crd", "standard")
}

type simulatedConfigurator struct {
	mu         sync.Mutex
	observed   map[string]delivery.Observation
	applyCount int
	changes    []config.ChangeSet
}

func newSimulatedConfigurator() *simulatedConfigurator {
	return &simulatedConfigurator{observed: make(map[string]delivery.Observation)}
}

func (c *simulatedConfigurator) Capabilities(_ context.Context, target config.Target) (delivery.Capabilities, error) {
	return delivery.Capabilities{
		ResourceKinds:               []config.Kind{config.KindListener, config.KindHTTPRoute, config.KindCluster, config.KindEndpointSet, config.KindSecretHandle},
		IndependentEndpointSets:     true,
		AtomicRelatedResourceGroups: true,
		Simulated:                   true,
	}, nil
}

func (c *simulatedConfigurator) Claim(_ context.Context, _ config.Target, ownerEpoch string) error {
	if ownerEpoch == "" {
		return fmt.Errorf("owner epoch is required")
	}
	return nil
}

func (c *simulatedConfigurator) Apply(_ context.Context, changes config.ChangeSet) (delivery.Receipt, error) {
	if err := changes.Validate(); err != nil {
		return delivery.Receipt{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	observation := c.observationLocked(changes.Target)
	c.changes = append(c.changes, config.ChangeSet{
		Target:     changes.Target,
		OwnerEpoch: changes.OwnerEpoch,
		Upserts:    append([]config.Resource(nil), changes.Upserts...),
		Deletes:    append([]config.Key(nil), changes.Deletes...),
	})
	resources := make(map[config.Key]delivery.ResourceResult, len(observation.Resources))
	for _, resource := range observation.Resources {
		resources[resource.Key] = resource
	}
	receipt := delivery.Receipt{Target: changes.Target, ChangeID: changes.ID, Simulated: true}
	for _, resource := range changes.Upserts {
		result := delivery.ResourceResult{
			Key:             resource.Key,
			DesiredRevision: resource.Revision,
			ActiveRevision:  resource.Revision,
			State:           delivery.ApplyActive,
		}
		resources[resource.Key] = result
		receipt.Resources = append(receipt.Resources, result)
	}
	for _, key := range changes.Deletes {
		delete(resources, key)
		receipt.Resources = append(receipt.Resources, delivery.ResourceResult{Key: key, State: delivery.ApplyDeleted})
	}
	observation.Resources = observation.Resources[:0]
	for _, resource := range resources {
		observation.Resources = append(observation.Resources, resource)
	}
	observation.Connected = true
	observation.Simulated = true
	c.observed[changes.Target.ID()] = observation
	c.applyCount++
	return receipt, nil
}

func (c *simulatedConfigurator) Observe(_ context.Context, target config.Target) (delivery.Observation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observationLocked(target), nil
}

func (c *simulatedConfigurator) observationLocked(target config.Target) delivery.Observation {
	observation, exists := c.observed[target.ID()]
	if !exists {
		return delivery.Observation{Target: target, Connected: true, Simulated: true}
	}
	observation.Resources = append([]delivery.ResourceResult(nil), observation.Resources...)
	observation.Rejected = append([]delivery.ResourceResult(nil), observation.Rejected...)
	return observation
}

func (c *simulatedConfigurator) appliedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applyCount
}

func (c *simulatedConfigurator) changesSince(index int) []config.ChangeSet {
	c.mu.Lock()
	defer c.mu.Unlock()
	if index < 0 || index >= len(c.changes) {
		return nil
	}
	return append([]config.ChangeSet(nil), c.changes[index:]...)
}

type simulatedProvisioner struct {
	mu           sync.Mutex
	ensureCalls  int
	releaseCalls int
	addresses    []string
}

func (p *simulatedProvisioner) Ensure(_ context.Context, intent delivery.TargetIntent) (delivery.TargetState, error) {
	p.mu.Lock()
	p.ensureCalls++
	addresses := append([]string(nil), p.addresses...)
	p.mu.Unlock()
	return delivery.TargetState{
		Target:    config.Target{Namespace: intent.Gateway.Namespace, Name: intent.Gateway.Name, UID: intent.Gateway.UID},
		Addresses: addresses,
		Ready:     true,
		Simulated: true,
	}, nil
}

func (p *simulatedProvisioner) Release(context.Context, config.Target) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseCalls++
	return nil
}

func (p *simulatedProvisioner) ensureCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureCalls
}

func (p *simulatedProvisioner) releaseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.releaseCalls
}

func (p *simulatedProvisioner) setAddresses(addresses []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addresses = append([]string(nil), addresses...)
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func conditionIs(conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus, generation int64) bool {
	condition := findCondition(conditions, conditionType)
	return condition != nil && condition.Status == status && condition.ObservedGeneration == generation
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func controllerPanicCount() float64 {
	families, err := metrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	var total float64
	for _, family := range families {
		if !strings.HasSuffix(family.GetName(), "_panics_total") {
			continue
		}
		for _, sample := range family.GetMetric() {
			if sample.GetCounter() != nil {
				total += sample.GetCounter().GetValue()
			}
		}
	}
	return total
}
