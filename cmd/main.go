package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"sort"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	nodecontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/node"
	nodegroupcontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/nodegroup"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
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

	platformCfg := platform.Config{
		NodepoolName:        os.Getenv("SEI_NODEPOOL_NAME"),
		TolerationKey:       os.Getenv("SEI_TOLERATION_KEY"),
		TolerationVal:       os.Getenv("SEI_TOLERATION_VALUE"),
		ServiceAccount:      os.Getenv("SEI_SERVICE_ACCOUNT"),
		StorageClassPerf:    os.Getenv("SEI_STORAGE_CLASS_PERF"),
		StorageClassDefault: os.Getenv("SEI_STORAGE_CLASS_DEFAULT"),
		StorageSizeDefault:  os.Getenv("SEI_STORAGE_SIZE_DEFAULT"),
		StorageSizeArchive:  os.Getenv("SEI_STORAGE_SIZE_ARCHIVE"),
		ResourceCPUArchive:  os.Getenv("SEI_RESOURCE_CPU_ARCHIVE"),
		ResourceMemArchive:  os.Getenv("SEI_RESOURCE_MEM_ARCHIVE"),
		ResourceCPUDefault:  os.Getenv("SEI_RESOURCE_CPU_DEFAULT"),
		ResourceMemDefault:  os.Getenv("SEI_RESOURCE_MEM_DEFAULT"),
		SnapshotBucket:      os.Getenv("SEI_SNAPSHOT_BUCKET"),
		SnapshotRegion:      os.Getenv("SEI_SNAPSHOT_REGION"),
		ResultExportBucket:  os.Getenv("SEI_RESULT_EXPORT_BUCKET"),
		ResultExportRegion:  os.Getenv("SEI_RESULT_EXPORT_REGION"),
		ResultExportPrefix:  os.Getenv("SEI_RESULT_EXPORT_PREFIX"),
		GenesisBucket:       os.Getenv("SEI_GENESIS_BUCKET"),
		GenesisRegion:       os.Getenv("SEI_GENESIS_REGION"),
	}

	if err := platformCfg.Validate(); err != nil {
		setupLog.Error(err, "Invalid platform configuration")
		os.Exit(1)
	}

	objectStore := platform.NewS3ObjectStore()
	kc := mgr.GetClient()

	buildSidecarClient := func(node *seiv1alpha1.SeiNode) (task.SidecarClient, error) {
		return sidecar.NewSidecarClient(planner.SidecarURLForNode(node))
	}

	//nolint:staticcheck // TODO: migrate to GetEventRecorder (new events API)
	nodeRecorder := mgr.GetEventRecorderFor("seinode-controller")
	if err := (&nodecontroller.SeiNodeReconciler{
		Client:   kc,
		Scheme:   mgr.GetScheme(),
		Recorder: nodeRecorder,
		Platform: platformCfg,
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			Client: kc,
			ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: func() (task.SidecarClient, error) {
						return buildSidecarClient(node)
					},
					KubeClient:  kc,
					Scheme:      mgr.GetScheme(),
					Resource:    node,
					Platform:    platformCfg,
					ObjectStore: objectStore,
				}
			},
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SeiNode")
		os.Exit(1)
	}

	controllerSA := os.Getenv("SEI_CONTROLLER_SA_PRINCIPAL")
	gatewayName := os.Getenv("SEI_GATEWAY_NAME")
	gatewayNamespace := os.Getenv("SEI_GATEWAY_NAMESPACE")
	if gatewayName == "" || gatewayNamespace == "" {
		setupLog.Error(nil, "SEI_GATEWAY_NAME and SEI_GATEWAY_NAMESPACE are required")
		os.Exit(1)
	}
	//nolint:staticcheck // migrating to events.EventRecorder API is a separate effort
	recorder := mgr.GetEventRecorderFor("seinodegroup-controller")
	if err := (&nodegroupcontroller.SeiNodeGroupReconciler{
		Client:           kc,
		Scheme:           mgr.GetScheme(),
		Recorder:         recorder,
		ControllerSA:     controllerSA,
		GatewayName:      gatewayName,
		GatewayNamespace: gatewayNamespace,
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNodeGroup]{
			Client: kc,
			ConfigFor: func(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) task.ExecutionConfig {
				var assemblerNode *seiv1alpha1.SeiNode
				nodes := &seiv1alpha1.SeiNodeList{}
				if err := kc.List(ctx, nodes,
					client.InNamespace(group.Namespace),
					client.MatchingLabels{"sei.io/nodegroup": group.Name},
				); err == nil && len(nodes.Items) > 0 {
					sort.Slice(nodes.Items, func(i, j int) bool {
						return nodes.Items[i].Name < nodes.Items[j].Name
					})
					assemblerNode = &nodes.Items[0]
				}
				return task.ExecutionConfig{
					BuildSidecarClient: func() (task.SidecarClient, error) {
						if assemblerNode == nil {
							return nil, fmt.Errorf("no assembler node found for group %s", group.Name)
						}
						return buildSidecarClient(assemblerNode)
					},
					KubeClient:  kc,
					Scheme:      mgr.GetScheme(),
					Resource:    group,
					Platform:    platformCfg,
					ObjectStore: objectStore,
				}
			},
		},
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
