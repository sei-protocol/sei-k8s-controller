//go:build envtest

// Package envtest_test runs the SeiNodeDeployment controller against a
// real kube-apiserver started by controller-runtime's envtest harness.
// Build-tagged so default `go test ./...` skips it — the harness depends
// on a downloaded apiserver/etcd binary set (see Makefile `setup-envtest`).
//
// envtest has no kubelet: Pods never reach Running and StatefulSet status
// never updates. Tests must assert on resources the controllers own
// directly (SeiNode specs, SND status conditions, child counts) and
// never on pod-state-driven signals.
package envtest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	nodecontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/node"
	nodedeploymentcontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment"
	envtestpkg "github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// Package-level handles populated by TestMain and consumed by individual
// _test.go files via the helpers in helpers_test.go.
var (
	testCli   client.Client
	testCtx   context.Context
	testCncl  context.CancelFunc
	testFaker *envtestpkg.StatusFaker

	// testSNDReconciler is the live SND reconciler the manager runs.
	// Exposed so tests that need to flip controller-level state (e.g.
	// the publishable-P2P capability toggle) can do so without rebuilding
	// the manager. Mutation must happen before the test creates the SND
	// under test; controller-runtime serializes reconciles per-key, so
	// reads during reconcile are safe.
	testSNDReconciler *nodedeploymentcontroller.SeiNodeDeploymentReconciler
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest run failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run encapsulates setup/teardown so deferred cleanup actually executes;
// os.Exit in TestMain otherwise bypasses defers.
func run(m *testing.M) (int, error) {
	// CRD paths are resolved relative to the package directory (go test's
	// working directory). The controller CRDs live at the repo root under
	// config/crd; gateway-api CRDs are vendored next to the test code.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return 1, fmt.Errorf("runtime.Caller failed; cannot resolve test paths")
	}
	pkgDir := filepath.Dir(thisFile)
	repoRoot := filepath.Clean(filepath.Join(pkgDir, "..", "..", "..", ".."))
	controllerCRDs := filepath.Join(repoRoot, "config", "crd")
	gatewayCRDs := filepath.Join(pkgDir, "testdata", "crds")

	// Build the scheme up-front so the manager + client share it.
	sch := clientgoscheme.Scheme
	utilruntime.Must(seiv1alpha1.AddToScheme(sch))
	utilruntime.Must(gatewayv1.Install(sch))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{controllerCRDs, gatewayCRDs},
		ErrorIfCRDPathMissing: true,
		Scheme:                sch,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return 1, fmt.Errorf("starting envtest: %w", err)
	}
	defer func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			fmt.Fprintf(os.Stderr, "envtest stop: %v\n", stopErr)
		}
	}()

	cli, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		return 1, fmt.Errorf("building test client: %w", err)
	}
	testCli = cli

	testCtx, testCncl = context.WithCancel(context.Background())
	defer testCncl()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 sch,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		return 1, fmt.Errorf("creating manager: %w", err)
	}

	kc := mgr.GetClient()
	platformCfg := platformtest.Config()

	// Shared stub sidecar — both controllers' ExecutionConfig factories
	// hand out closures that resolve to this instance, so tasks
	// submitted by either reconciler are observable to the other (the
	// SND-side AwaitSpecUpdate poll path doesn't itself call the
	// sidecar, but the SeiNode init/update plans do).
	stubSC := envtestpkg.NewStubSidecarClient()
	// The closure signature is fixed by ExecutionConfig.BuildSidecarClient
	// — error is part of the contract even when this stub never returns one.
	buildStubSC := func() (task.SidecarClient, error) { //nolint:unparam // signature pinned by task.ExecutionConfig field
		return stubSC, nil
	}

	// StatefulSet status faker: envtest's apiserver has no StatefulSet
	// controller, so .Status stays empty and the SeiNode rollout plan
	// (ReplacePod + ObserveImage) cannot advance. The faker patches
	// every StatefulSet to look "fully rolled out at the current
	// generation," collapsing the rollout to instantaneous from the
	// controller's perspective. The test asserts on terminal state, so
	// this is indistinguishable from a real (already-completed)
	// rollout.
	testFaker = envtestpkg.StartStatefulSetStatusFaker(testCtx, kc)

	// SeiNode reconciler — wired with the stub sidecar so init plans
	// (genesis mode: ensure-data-pvc, apply-statefulset, apply-service,
	// configure-genesis, config-apply, config-validate, mark-ready)
	// complete in envtest. The InPlace rollout test depends on this:
	// once a SeiNode reaches PhaseRunning, its buildRunningPlan
	// detects spec.image vs status.currentImage drift and emits a
	// NodeUpdate plan whose observe-image task stamps currentImage.
	// That stamp is what the SND-side AwaitSpecUpdate task polls on.
	nodeRecorder := mgr.GetEventRecorderFor("seinode-controller") //nolint:staticcheck // new events API migration is a separate effort
	if err := (&nodecontroller.SeiNodeReconciler{
		Client:   kc,
		Scheme:   mgr.GetScheme(),
		Recorder: nodeRecorder,
		Platform: platformCfg,
		Planner: &planner.NodeResolver{
			BuildSidecarClient: func(_ *seiv1alpha1.SeiNode) (task.SidecarClient, error) {
				return stubSC, nil
			},
		},
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: buildStubSC,
					// NewSidecarClient returns the real concrete type
					// (*sidecar.SidecarClient) which the stub cannot
					// satisfy. The InPlace path never reaches a task
					// that consumes NewSidecarClient (only group-level
					// genesis ceremony tasks do), so leaving it nil
					// is safe; the stub guards via
					// ExecutionConfig.sidecarFor's nil check.
					KubeClient: kc,
					APIReader:  mgr.GetAPIReader(),
					Scheme:     mgr.GetScheme(),
					Resource:   node,
					Platform:   platformCfg,
				}
			},
		},
	}).SetupWithManager(mgr); err != nil {
		return 1, fmt.Errorf("setting up SeiNode controller: %w", err)
	}

	// SeiNodeDeployment reconciler. Its plan executor is wired with
	// the same stub sidecar for symmetry, even though the InPlace
	// deployment plan tasks (UpdateNodeSpecs, AwaitSpecUpdate) are
	// pure kube-client tasks and never call the sidecar. Genesis
	// ceremony tasks would; the InPlace fixtures don't trigger those.
	recorder := mgr.GetEventRecorderFor("seinodedeployment-controller") //nolint:staticcheck // new events API migration is a separate effort
	// Default capability state: publishability enabled, but the public
	// domain stays empty — the publishable-P2P tests flip both fields
	// themselves before creating their SND so they don't perturb the
	// HTTP-route tests' single-hostname expectations.
	testSNDReconciler = &nodedeploymentcontroller.SeiNodeDeploymentReconciler{
		Client:                  kc,
		Scheme:                  mgr.GetScheme(),
		Recorder:                recorder,
		GatewayName:             "sei-gateway",
		GatewayNamespace:        "gateway",
		GatewayDomain:           "test.local",
		GatewayPublicDomain:     "",
		PublishableDomain:       "",
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNodeDeployment]{
			ConfigFor: func(_ context.Context, group *seiv1alpha1.SeiNodeDeployment) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: buildStubSC,
					KubeClient:         kc,
					APIReader:          mgr.GetAPIReader(),
					Scheme:             mgr.GetScheme(),
					Resource:           group,
					Platform:           platformCfg,
				}
			},
		},
	}
	if err := testSNDReconciler.SetupWithManager(mgr); err != nil {
		return 1, fmt.Errorf("setting up SeiNodeDeployment controller: %w", err)
	}

	mgrErrCh := make(chan error, 1)
	go func() {
		if err := mgr.Start(testCtx); err != nil {
			mgrErrCh <- err
		}
		close(mgrErrCh)
	}()

	// Wait for informer caches before tests issue Gets/Lists; otherwise the
	// first read can lose to a cold cache.
	if !mgr.GetCache().WaitForCacheSync(testCtx) {
		return 1, fmt.Errorf("manager cache failed to sync")
	}

	code := m.Run()

	testCncl()
	select {
	case err := <-mgrErrCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "manager exited with error: %v\n", err)
		}
	case <-time.After(5 * time.Second):
		fmt.Fprintln(os.Stderr, "manager did not exit within 5s of cancel")
	}

	return code, nil
}
