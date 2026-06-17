//go:build envtest

// Package envtest_test runs the SeiNetwork controller against a
// real kube-apiserver started by controller-runtime's envtest harness.
// Build-tagged so default `go test ./...` skips it — the harness depends
// on a downloaded apiserver/etcd binary set (see Makefile `setup-envtest`).
//
// envtest has no kubelet: Pods never reach Running and StatefulSet status
// never updates. Tests must assert on resources the controllers own
// directly (SeiNode specs, SeiNetwork status conditions, child counts) and
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

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	nodecontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/node"
	seinetworkcontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork"
	envtestpkg "github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest"
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
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return 1, fmt.Errorf("runtime.Caller failed; cannot resolve test paths")
	}
	pkgDir := filepath.Dir(thisFile)
	repoRoot := filepath.Clean(filepath.Join(pkgDir, "..", "..", "..", ".."))
	controllerCRDs := filepath.Join(repoRoot, "config", "crd")

	sch := clientgoscheme.Scheme
	utilruntime.Must(seiv1alpha1.AddToScheme(sch))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{controllerCRDs},
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
	// hand out closures that resolve to this instance, so tasks submitted
	// by either reconciler are observable to the other.
	stubSC := envtestpkg.NewStubSidecarClient()
	buildStubSC := func() (task.SidecarClient, error) { //nolint:unparam // signature pinned by task.ExecutionConfig field
		return stubSC, nil
	}
	// NewSidecarClient returns the concrete *sidecar.SidecarClient that the
	// genesis-ceremony collect-and-set-peers task drives for GetNodeID. The
	// narrow StubSidecarClient can't satisfy the concrete type, so back a real
	// client with a fake /v0/node-id transport (see envtestpkg.nodeIDDoer).
	// Without this the ceremony stalls at collect-and-set-peers
	// ("NewSidecarClient not configured") and the network never reaches Ready.
	// node arg ignored: the fake transport answers regardless of host.
	newNodeIDSC := func(_ *seiv1alpha1.SeiNode) (*sidecar.SidecarClient, error) {
		return envtestpkg.NewNodeIDSidecarClient()
	}

	// StatefulSet status faker: envtest's apiserver has no StatefulSet
	// controller, so .Status stays empty and the SeiNode rollout plan cannot
	// advance. The faker patches every StatefulSet to look "fully rolled out
	// at the current generation."
	testFaker = envtestpkg.StartStatefulSetStatusFaker(testCtx, kc)

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
			Platform: platformCfg,
		},
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: buildStubSC,
					NewSidecarClient:   newNodeIDSC,
					KubeClient:         kc,
					APIReader:          mgr.GetAPIReader(),
					Scheme:             mgr.GetScheme(),
					Resource:           node,
					Platform:           platformCfg,
				}
			},
		},
	}).SetupWithManager(mgr); err != nil {
		return 1, fmt.Errorf("setting up SeiNode controller: %w", err)
	}

	// SeiNetwork reconciler. Its plan executor is wired with the same stub
	// sidecar; the only network-level plan is the genesis ceremony, whose
	// tasks call the sidecar and resolve to the stub.
	recorder := mgr.GetEventRecorderFor("seinetwork-controller") //nolint:staticcheck // new events API migration is a separate effort
	reconciler := &seinetworkcontroller.SeiNetworkReconciler{
		Client:   kc,
		Scheme:   mgr.GetScheme(),
		Recorder: recorder,
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNetwork]{
			ConfigFor: func(_ context.Context, network *seiv1alpha1.SeiNetwork) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: buildStubSC,
					NewSidecarClient:   newNodeIDSC,
					KubeClient:         kc,
					APIReader:          mgr.GetAPIReader(),
					Scheme:             mgr.GetScheme(),
					Resource:           network,
					Platform:           platformCfg,
				}
			},
		},
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return 1, fmt.Errorf("setting up SeiNetwork controller: %w", err)
	}

	mgrErrCh := make(chan error, 1)
	go func() {
		if err := mgr.Start(testCtx); err != nil {
			mgrErrCh <- err
		}
		close(mgrErrCh)
	}()

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
