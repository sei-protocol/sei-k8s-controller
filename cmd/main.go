package main

import (
	"crypto/tls"
	"flag"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	nodecontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/node"
	nodegroupcontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/nodegroup"
	nodepoolcontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/nodepool"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(seiv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to. Use 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt",
		"The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key",
		"The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server.")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Disable HTTP/2 by default due to HTTP/2 Stream Cancellation and Rapid Reset CVEs:
	// https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// https://github.com/advisories/GHSA-4374-p667-p6c8
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		})
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Using provided metrics certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)
		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsServerOptions,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "bc1f5b0a.sei.io",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&nodepoolcontroller.SeiNodePoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SeiNodePool")
		os.Exit(1)
	}

	platform := nodecontroller.DefaultPlatformConfig()
	if v := os.Getenv("SEI_NODEPOOL_NAME"); v != "" {
		platform.NodepoolName = v
	}
	if v := os.Getenv("SEI_TOLERATION_KEY"); v != "" {
		platform.TolerationKey = v
	}
	if v := os.Getenv("SEI_TOLERATION_VALUE"); v != "" {
		platform.TolerationVal = v
	}
	if v := os.Getenv("SEI_SERVICE_ACCOUNT"); v != "" {
		platform.ServiceAccount = v
	}
	if v := os.Getenv("SEI_STORAGE_CLASS_PERF"); v != "" {
		platform.StorageClassPerf = v
	}
	if v := os.Getenv("SEI_STORAGE_CLASS_DEFAULT"); v != "" {
		platform.StorageClassDefault = v
	}
	if v := os.Getenv("SEI_STORAGE_SIZE_DEFAULT"); v != "" {
		platform.StorageSizeDefault = v
	}
	if v := os.Getenv("SEI_STORAGE_SIZE_ARCHIVE"); v != "" {
		platform.StorageSizeArchive = v
	}
	if v := os.Getenv("SEI_RESOURCE_CPU_ARCHIVE"); v != "" {
		platform.ResourceCPUArchive = v
	}
	if v := os.Getenv("SEI_RESOURCE_MEM_ARCHIVE"); v != "" {
		platform.ResourceMemArchive = v
	}
	if v := os.Getenv("SEI_RESOURCE_CPU_DEFAULT"); v != "" {
		platform.ResourceCPUDefault = v
	}
	if v := os.Getenv("SEI_RESOURCE_MEM_DEFAULT"); v != "" {
		platform.ResourceMemDefault = v
	}
	if v := os.Getenv("SEI_SNAPSHOT_REGION"); v != "" {
		platform.SnapshotRegion = v
	}

	nodeRecorder := mgr.GetEventRecorderFor("seinode-controller")
	if err := (&nodecontroller.SeiNodeReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Recorder:     nodeRecorder,
		Platform:     platform,
		PlanExecutor: &planner.Executor{Client: mgr.GetClient()},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SeiNode")
		os.Exit(1)
	}

	controllerSA := os.Getenv("SEI_CONTROLLER_SA_PRINCIPAL")
	//nolint:staticcheck // migrating to events.EventRecorder API is a separate effort
	recorder := mgr.GetEventRecorderFor("seinodegroup-controller")
	if err := (&nodegroupcontroller.SeiNodeGroupReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Recorder:     recorder,
		ControllerSA: controllerSA,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SeiNodeGroup")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
