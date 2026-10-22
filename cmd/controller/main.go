package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/praxis-proxy/controller/internal/controller"
	"github.com/praxis-proxy/controller/internal/delivery"
	"github.com/praxis-proxy/controller/internal/delivery/file"
	"github.com/praxis-proxy/controller/internal/delivery/mock"
)

var runtimeScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(scheme.AddToScheme(runtimeScheme))
	utilruntime.Must(gatewayv1.AddToScheme(runtimeScheme))
}

func main() {
	klog.InitFlags(nil)
	var (
		controllerName   string
		metricsAddress   string
		metricsCertDir   string
		probeAddress     string
		leaderElection   bool
		leaderElectionNS string
		shutdownTimeout  time.Duration
		adapter          string
		fileOptions      file.Options
	)
	flag.StringVar(&controllerName, "controller-name", "gateway.praxis-proxy.io/controller", "Gateway API controllerName claimed by this controller")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8443", "Authenticated TLS metrics bind address; use 0 to disable")
	flag.StringVar(&metricsCertDir, "metrics-cert-dir", "/var/run/praxis/metrics/tls", "Directory containing tls.crt and tls.key for metrics HTTPS")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "Health and readiness probe bind address")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Enable leader election")
	flag.StringVar(&leaderElectionNS, "leader-election-namespace", "", "Leader election Lease namespace; defaults to the pod namespace in-cluster")
	flag.DurationVar(&shutdownTimeout, "graceful-shutdown-timeout", 30*time.Second, "Maximum time to drain workers during shutdown")
	flag.StringVar(&adapter, "delivery-adapter", "mock", "Delivery adapter: mock or file")
	flag.StringVar(&fileOptions.Root, "praxis-config-root", "/var/run/praxis/config", "Root directory for the file delivery adapter")
	flag.StringVar(&fileOptions.TLSRoot, "praxis-tls-root", "/etc/praxis/tls", "Root directory containing mounted TLS certificates")
	flag.StringVar(&fileOptions.ListenerBindAddress, "praxis-listener-bind-address", "0.0.0.0", "Bind address used for rendered listeners")
	flag.BoolVar(&fileOptions.AllowPrivateEndpoints, "praxis-allow-private-endpoints", false, "Allow private backend addresses in generated configuration")
	flag.DurationVar(&fileOptions.OwnerStaleAfter, "praxis-owner-stale-after", time.Minute, "Owner epoch takeover threshold")
	flag.Func("praxis-gateway-address", "Static Gateway status address; repeatable", func(value string) error {
		fileOptions.Addresses = append(fileOptions.Addresses, value)
		return nil
	})
	flag.Parse()

	ctrl.SetLogger(klog.Background())
	log := ctrl.Log.WithName("setup")
	if err := run(controllerName, metricsAddress, metricsCertDir, probeAddress, leaderElection, leaderElectionNS, shutdownTimeout, adapter, fileOptions, log); err != nil {
		log.Error(err, "Controller stopped")
		os.Exit(1)
	}
}

func run(controllerName, metricsAddress, metricsCertDir, probeAddress string, leaderElection bool, leaderElectionNS string, shutdownTimeout time.Duration, adapter string, fileOptions file.Options, log logr.Logger) error {
	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: runtimeScheme,
		Cache: cache.Options{
			DefaultTransform:            cache.TransformStripManagedFields(),
			ReaderFailOnMissingInformer: true,
		},
		Metrics: metricsserver.Options{
			BindAddress:    metricsAddress,
			SecureServing:  true,
			CertDir:        metricsCertDir,
			FilterProvider: filters.WithAuthenticationAndAuthorization,
		},
		HealthProbeBindAddress:  probeAddress,
		LeaderElection:          leaderElection,
		LeaderElectionID:        "gateway-controller.praxis-proxy.io",
		LeaderElectionNamespace: leaderElectionNS,
		GracefulShutdownTimeout: &shutdownTimeout,
		Controller:              config.Controller{CacheSyncTimeout: 2 * time.Minute},
	})
	if err != nil {
		return err
	}
	ownerEpoch, err := newOwnerEpoch()
	if err != nil {
		return err
	}
	var configurator delivery.Configurator
	var provisioner delivery.Provisioner
	switch adapter {
	case "mock":
		configurator, provisioner = mock.NewConfigurator(), mock.NewProvisioner()
	case "file":
		configurator, err = file.NewConfigurator(fileOptions)
		if err != nil {
			return err
		}
		provisioner, err = file.NewProvisioner(fileOptions)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown delivery adapter %q", adapter)
	}
	if err := controller.SetupWithManager(mgr, configurator, provisioner, controllerName, ownerEpoch); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	log.Info("Starting Gateway API controller", "controllerName", controllerName, "deliveryAdapter", adapter)
	return mgr.Start(ctrl.SetupSignalHandler())
}

func newOwnerEpoch() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}
