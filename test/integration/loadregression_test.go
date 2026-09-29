//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sei-protocol/sei-k8s-controller/harness/bench"
	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/test/integration/loadregression"
)

// TestNightlyLoadRegression runs the loadregression workload against a fresh
// chain on the image under test and gates block-included throughput,
// block-interval p50/p90 and the validators' CPU and memory against its
// recorded baseline.
//
// The chain is the vanilla image, not mock_balances, so balances are real: a
// throwaway root key is funded at genesis and sei-load's funder disperses from
// it to the account pool before load starts.
//
// Metrics come from the chain's block timestamps and Prometheus' cAdvisor
// series over the load window, which opens loadregression.WarmupMinutes after
// the sei-load container started and closes loadregression.DurationMinutes
// after it started. Every run logs a "load-regression result:" line already in
// the shape of a baseline run entry.
//
// Inputs (env): SEI_CHAIN_ID, SEID_IMAGE, SEILOAD_IMAGE (sei-load with
// root-key funding), PROMETHEUS_URL [required]; SEI_NAMESPACE,
// SEILOAD_COMMIT_ID [optional]. Run with -test.timeout 0 (see
// TestNightlyBenchmark).
func TestNightlyLoadRegression(t *testing.T) {
	requireCluster(t)

	baseline, err := loadregression.RecordedBaseline()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadregression.RunConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Before an hour of chain is spent on a run that could not be compared.
	if err := baseline.Comparable(cfg); err != nil {
		t.Fatalf("UNEVALUABLE: %v", err)
	}

	promURL := mustEnv(t, "PROMETHEUS_URL")
	chainID := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	s := spec{
		chainID: chainID,
		// The run id names the seiload Job (seiload-loadreg-*), which is how
		// the benchmark's alerts tell this suite's 200 TPS run apart from the
		// benchmark's own seiload pods under the shared chain-id base.
		runID:         "loadreg-" + chainID,
		namespace:     envOr("SEI_NAMESPACE", ""),
		seidImage:     mustEnv(t, "SEID_IMAGE"),
		validators:    loadregression.Validators,
		rpcNodes:      loadregression.RPCNodes,
		timeout:       110 * time.Minute,
		seiloadImage:  mustEnv(t, "SEILOAD_IMAGE"),
		seiloadCommit: envOr("SEILOAD_COMMIT_ID", ""),
		durationMin:   loadregression.DurationMinutes,
		storageConfig: memiavlStorageConfig,
		rpcConfig: map[string]string{
			"evm.worker_pool_size":  "32",
			"evm.worker_queue_size": "4000",
			"evm.max_tx_pool_txs":   "10000",
		},
	}

	root, err := keygen.DeriveEVM()
	if err != nil {
		t.Fatalf("derive root key: %v", err)
	}
	s.accounts = []sei.GenesisAccount{{Address: root.Address, Balance: loadregression.RootBalance}}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	c := openClient(ctx, t)
	cs := clientset(t)

	ch, err := provision(ctx, t, c, s)
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Logf("provisioned %s: %d validators + %d RPC followers; root %s (%s) funded %s",
		s.chainID, s.validators, len(ch.rpcNodes), root.EVMAddress, root.Address, loadregression.RootBalance)

	ns := ch.network.Namespace()
	hc := &http.Client{Timeout: 10 * time.Second}
	// rpc-0 takes the sends; rpc-1 takes none and is where receipts, heads and
	// the measured blocks are read.
	sendNode, receiptNode := ch.rpcNodes[0], ch.rpcNodes[1]
	tmRPC := receiptNode.TendermintRPC()
	startHeight := mustLatestHeight(ctx, t, hc, tmRPC, "pre-load")

	secretName := "seiload-root-" + s.runID
	createKeySecret(ctx, t, cs, ns, secretName, map[string]string{runLabelKey: s.runID},
		bench.RootKeySecretKey, root.PrivateKeyHex)

	profileCM := "seiload-profile-" + s.runID
	createProfileCM(ctx, t, cs, ns, profileCM, s.runID,
		loadregression.RenderProfile(s.chainID, sendNode.EVMRPC(), receiptNode.EVMRPC()))

	jobName := runSeiloadJob(ctx, t, cs, ns, bench.Params{
		RunID:           s.runID,
		ChainID:         s.chainID,
		Commit:          s.seiloadCommit,
		Image:           s.seiloadImage,
		DurationMinutes: s.durationMin,
		ProfileCM:       profileCM,
		Workload:        "load-regression",
		RootKeySecret:   secretName,
	})
	t.Logf("seiload job %s completed; log tail:\n%s", jobName, podLogTail(ctx, cs, ns, jobName))

	for _, n := range ch.rpcNodes {
		if err := sei.WaitCaughtUp(ctx, hc, n.TendermintRPC()); err != nil {
			t.Fatalf("UNEVALUABLE: post-load %s not caught up: %v", n.Name(), err)
		}
	}

	started, finished := seiloadRunTimes(ctx, t, cs, ns, jobName)
	from := started.Add(loadregression.WarmupMinutes * time.Minute)
	to := started.Add(loadregression.DurationMinutes * time.Minute)
	if finished.Before(to) {
		t.Fatalf("UNEVALUABLE: seiload exited at %s, before its %dm load window closed at %s",
			finished.Format(time.RFC3339), loadregression.DurationMinutes, to.Format(time.RFC3339))
	}

	endHeight := mustLatestHeight(ctx, t, hc, tmRPC, "post-load")
	blocks := blocksBetween(ctx, t, hc, tmRPC, startHeight-1, endHeight)
	m, err := loadregression.Measure(blocks, from, to)
	if err != nil {
		t.Fatalf("UNEVALUABLE: measure blocks %d..%d: %v", startHeight, endHeight, err)
	}
	t.Logf("load window %s..%s: %d blocks, %d txs over %s",
		from.Format(time.RFC3339), to.Format(time.RFC3339), m.Blocks, m.Txs, m.Window)

	// The window's last samples need a scrape or two to land in Prometheus.
	time.Sleep(time.Until(to.Add(2 * loadregression.ResourceStep)))
	pods := validatorPods(ctx, t, cs, ns, s.chainID)
	resources, err := loadregression.ValidatorResources(ctx, promURL, ns, pods, from, to)
	if err != nil {
		t.Fatalf("UNEVALUABLE: validator resources from %s: %v", promURL, err)
	}
	maps.Copy(m.Metrics, resources)
	for k, v := range m.Metrics {
		m.Metrics[k] = math.Round(v*1000) / 1000
	}

	result, err := json.Marshal(loadregression.BaselineRun{
		Metrics:    m.Metrics,
		SeidImage:  s.seidImage,
		RecordedAt: started.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	t.Logf("load-regression result: %s", result)

	v := loadregression.Check(baseline, m.Metrics)
	for _, line := range v.Lines {
		t.Log(line)
	}
	switch {
	case len(v.Unevaluable) > 0:
		t.Fatalf("UNEVALUABLE: %s", strings.Join(v.Unevaluable, "; "))
	case len(v.Regressions) > 0:
		t.Errorf("REGRESSION against the baseline mean of %d runs:\n  %s",
			len(baseline.Runs), strings.Join(v.Regressions, "\n  "))
	default:
		t.Logf("within tolerance of the baseline mean of %d runs", len(baseline.Runs))
	}
}

// validatorPods lists the network's validator pods, the ones the network's
// sei.io/nodedeployment label selects (RPC followers are separate SeiNodes).
func validatorPods(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, chainID string) []string {
	t.Helper()
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "sei.io/nodedeployment=" + chainID})
	if err != nil {
		t.Fatalf("UNEVALUABLE: list validator pods of %s: %v", chainID, err)
	}
	names := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		names = append(names, p.Name)
	}
	if len(names) != loadregression.Validators {
		t.Fatalf("UNEVALUABLE: %s has %d validator pods %v, want %d", chainID, len(names), names, loadregression.Validators)
	}
	return names
}

// seiloadRunTimes returns when the finished seiload container started and
// exited. Container start, not Job creation, anchors the window: image pull
// and scheduling happen before it and are not load.
func seiloadRunTimes(
	ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, jobName string,
) (started, finished time.Time) {
	t.Helper()
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + jobName,
	})
	if err != nil {
		t.Fatalf("list pods of job %q: %v", jobName, err)
	}
	for _, p := range pods.Items {
		for _, st := range p.Status.ContainerStatuses {
			if st.Name == "seiload" && st.State.Terminated != nil {
				return st.State.Terminated.StartedAt.Time, st.State.Terminated.FinishedAt.Time
			}
		}
	}
	t.Fatalf("UNEVALUABLE: no terminated seiload container in job %q (%d pods)", jobName, len(pods.Items))
	return time.Time{}, time.Time{}
}
