//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/test/manifests/bench"
)

// seiloadProfilesCM is the platform-owned ConfigMap holding the profile
// templates (placeholders __SEI_CHAIN_ID__ / __RPC_ENDPOINTS__ /
// __RECEIPT_ENDPOINT__). The harness reads it from the cluster rather than
// vendoring the profile, so the load shape stays owned by platform.
const seiloadProfilesCM = "seiload-profiles"

// clientset builds a client-go clientset from the ambient config — the harness
// uses it for the Job/ConfigMap operations the SDK does not cover.
func clientset(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	return cs
}

// renderProfile reads the platform profile template from seiload-profiles and
// substitutes the per-run chain id, the EVM endpoints load is sent to
// (JSON-quoted) and the EVM endpoint receipts are read from.
func renderProfile(
	ctx context.Context, t *testing.T, cs *kubernetes.Clientset,
	ns, profile, chainID string, endpoints []string, receiptEndpoint string,
) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, seiloadProfilesCM, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, seiloadProfilesCM, err)
	}
	tmpl, ok := cm.Data[profile+".json"]
	if !ok {
		t.Fatalf("profile %q.json absent from %s", profile, seiloadProfilesCM)
	}
	quoted := make([]string, len(endpoints))
	for i, e := range endpoints {
		quoted[i] = strconv.Quote(e)
	}
	tmpl = strings.ReplaceAll(tmpl, "__SEI_CHAIN_ID__", chainID)
	tmpl = strings.ReplaceAll(tmpl, "__RPC_ENDPOINTS__", strings.Join(quoted, ","))
	tmpl = strings.ReplaceAll(tmpl, "__RECEIPT_ENDPOINT__", receiptEndpoint)
	return tmpl
}

// createProfileCM writes the rendered profile to a per-run ConfigMap stamped
// with the run label so the GC sweep reaps it on an abnormal exit.
func createProfileCM(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, name, runID, profileJSON string) {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{runLabelKey: runID},
		},
		Data: map[string]string{"profile.json": profileJSON},
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create profile cm %q: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = cs.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{})
	})
}

// createKeySecret writes a key (a mnemonic, the seiload root key) to a
// single-entry Secret a harness pod mounts or reads via secretKeyRef. Labeled
// for the GC sweep and deleted on cleanup, matching how the suites manage
// everything else they create.
func createKeySecret(
	ctx context.Context, t *testing.T, cs *kubernetes.Clientset,
	ns, name string, labels map[string]string, key, value string,
) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{key: []byte(value)},
	}
	if _, err := cs.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create secret %q: %v", name, err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = cs.CoreV1().Secrets(ns).Delete(delCtx, name, metav1.DeleteOptions{})
	})
}

// renderJob renders the shared seiload Job manifest with the per-run params.
func renderJob(t *testing.T, p bench.Params) *batchv1.Job {
	t.Helper()
	out, err := bench.Render(p)
	if err != nil {
		t.Fatalf("render seiload job: %v", err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(out, &job); err != nil {
		t.Fatalf("unmarshal seiload job: %v", err)
	}
	return &job
}

// runSeiload renders the platform profile, applies seiload's Job manifest, waits
// for the Job to complete, and fails the run only when its data isn't valid:
// (1) the chain halted or a follower lagged (assertChainLive), (2) seiload's run
// was short, reverted too much or, where required, read too few execution
// statuses (assertSeiloadRun), or (3) the chain included no transactions during
// the load window. The inclusion gate is load-bearing because seiload exits 0
// on its duration deadline regardless of outcome: a chain that accepts every
// submission into mempools but includes none in blocks yields a Complete Job,
// live followers, and a green run despite being effectively write-only.
// Throughput and cost are judged by the metrics layer (podMonitor + alerts).
//
// Load goes to the first RPC follower only; inclusion (sei-load's and the
// gate's) is read from the second, which takes no sends, so it measures the
// chain rather than a loaded node. A non-empty s.seiloadRootKey is mounted as
// sei-load's funding root key.
func runSeiload(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ch *chain, s spec) {
	t.Helper()
	// The seiload Job co-locates with the chain; the network's resolved
	// namespace is authoritative (never re-resolve from env here).
	ns := ch.network.Namespace()
	if len(ch.rpcNodes) < 2 {
		t.Fatalf("runSeiload needs 2 RPC followers (one for sends, one for receipts), got %d", len(ch.rpcNodes))
	}
	send, receipt := ch.rpcNodes[0], ch.rpcNodes[1]

	// The inclusion window opens at the committed height before load starts.
	hc := &http.Client{Timeout: 10 * time.Second}
	tmRPC := receipt.TendermintRPC()
	startHeight := mustLatestHeight(ctx, t, hc, tmRPC, "pre-load")

	profileCM := "seiload-profile-" + s.runID
	profileJSON := renderProfile(ctx, t, cs, ns, s.seiloadProfile, s.chainID, []string{send.EVMRPC()}, receipt.EVMRPC())
	createProfileCM(ctx, t, cs, ns, profileCM, s.runID, profileJSON)

	var rootKeySecret string
	if s.seiloadRootKey != "" {
		rootKeySecret = "seiload-root-" + s.runID
		createKeySecret(ctx, t, cs, ns, rootKeySecret, map[string]string{runLabelKey: s.runID},
			bench.RootKeySecretKey, s.seiloadRootKey)
	}

	job := renderJob(t, bench.Params{
		RunID:           s.runID,
		ChainID:         s.chainID,
		Commit:          s.seiloadCommit,
		Image:           s.seiloadImage,
		DurationMinutes: s.durationMin,
		ProfileCM:       profileCM,
		Workload:        s.seiloadWorkload,
		RootKeySecret:   rootKeySecret,
	})
	job.Namespace = ns
	runJob(ctx, t, cs, job)

	assertChainLive(ctx, t, hc, ch)
	assertSeiloadRun(ctx, t, cs, job, s)

	// Chain included the load: at least one transaction landed in a block during
	// the window.
	endHeight := mustLatestHeight(ctx, t, hc, tmRPC, "post-load")
	included := includedTxCount(ctx, t, hc, tmRPC, startHeight, endHeight)
	if included == 0 {
		t.Errorf("seiload ran %dm against %s but 0 transactions were included in blocks %d..%d — "+
			"the chain accepted load without including any of it",
			s.durationMin, s.chainID, startHeight, endHeight)
		return
	}
	t.Logf("inclusion gate: >=%d transactions included in blocks %d..%d", included, startHeight, endHeight)
}

// mustLatestHeight reads the committed height with a bounded retry: a transient
// blip must not discard a run whose load already completed, but an endpoint that
// stays unreachable means inclusion cannot be verified, which fails closed.
func mustLatestHeight(ctx context.Context, t *testing.T, hc *http.Client, tmRPC, phase string) int64 {
	t.Helper()
	for range 3 {
		if h, ok := sei.LatestHeight(ctx, hc, tmRPC); ok {
			return h
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("read %s height from %s: endpoint unreachable — cannot verify the run", phase, tmRPC)
	return 0
}

const (
	// followerMaxLag is how many blocks a follower may trail the validators'
	// head after load: read skew between two RPCs, not a stall.
	followerMaxLag = 10
	// maxRevertRatio is the share of executed transactions that may revert
	// before a run stops describing its workload.
	maxRevertRatio = 0.01
	// seiloadSummaryLines is enough log tail to hold seiload's end-of-run summary.
	seiloadSummaryLines = 200
)

// assertChainLive fails unless the validators still produce blocks and every
// follower sits within followerMaxLag of their head. catching_up is a one-way
// latch that a follower stalled after its first catch-up never re-flips, so
// heights are compared directly.
func assertChainLive(ctx context.Context, t *testing.T, hc *http.Client, ch *chain) {
	t.Helper()
	validators := ch.network.TendermintRPC()
	advanceCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := sei.WaitHeightAdvances(advanceCtx, hc, validators, 2); err != nil {
		t.Errorf("post-load validators halted: %v", err)
		return
	}
	head := mustLatestHeight(ctx, t, hc, validators, "post-load validator")
	for _, n := range ch.rpcNodes {
		h := mustLatestHeight(ctx, t, hc, n.TendermintRPC(), "post-load "+n.Name())
		if lag := head - h; lag > followerMaxLag {
			t.Errorf("post-load %s at height %d trails the validator head %d by %d blocks (> %d)",
				n.Name(), h, head, lag, followerMaxLag)
		}
	}
}

// seiload's end-of-run summary lines: accepted transactions whose execution
// status was read, and executed transactions that reverted.
var (
	statusReadLine = regexp.MustCompile(`Execution status was read for (\d+) of (\d+) accepted`)
	revertedLine   = regexp.MustCompile(`Of the (\d+) the chain executed, (\d+) reverted`)
)

// summaryCounts returns the two counts line captures from seiload's log.
func summaryCounts(line *regexp.Regexp, log string) (first, second int64, ok bool) {
	m := line.FindStringSubmatch(log)
	if m == nil {
		return 0, 0, false
	}
	first, errFirst := strconv.ParseInt(m[1], 10, 64)
	second, errSecond := strconv.ParseInt(m[2], 10, 64)
	return first, second, errFirst == nil && errSecond == nil
}

// assertSeiloadRun fails a run whose data doesn't describe its workload:
// seiload exiting before its duration, more than maxRevertRatio of executed
// transactions reverting, or, when s.minReceiptCoverage is set, too few
// accepted transactions with a read execution status. The counts come from
// seiload's end-of-run summary, so a SEILOAD_IMAGE that stops printing it
// fails here rather than passing unverified.
func assertSeiloadRun(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, job *batchv1.Job, s spec) {
	t.Helper()
	pod, err := jobPod(ctx, cs, job.Namespace, job.Name)
	if err != nil {
		t.Errorf("%v — cannot verify the run", err)
		return
	}
	window := time.Duration(s.durationMin) * time.Minute
	for _, st := range pod.Status.ContainerStatuses {
		term := st.State.Terminated
		if term == nil {
			t.Errorf("seiload container %s has not terminated — cannot verify its run length", st.Name)
			continue
		}
		if ran := term.FinishedAt.Sub(term.StartedAt.Time); ran < window {
			t.Errorf("seiload ran %s, short of its %s load window", ran.Round(time.Second), window)
		}
	}

	log, err := podLog(ctx, cs, pod, seiloadSummaryLines)
	if err != nil {
		t.Errorf("read seiload log: %v — cannot verify the run", err)
		return
	}
	executed, reverted, ok := summaryCounts(revertedLine, log)
	switch {
	case !ok:
		t.Errorf("seiload summary has no revert count — cannot verify the run; log tail:\n%s", log)
	case float64(reverted) > maxRevertRatio*float64(executed):
		t.Errorf("%d of %d executed transactions reverted (> %.0f%%): the run did not exercise its workload",
			reverted, executed, maxRevertRatio*100)
	}
	if s.minReceiptCoverage == 0 {
		return
	}
	read, accepted, ok := summaryCounts(statusReadLine, log)
	switch {
	case !ok:
		t.Errorf("seiload summary has no execution-status coverage — cannot verify the run; log tail:\n%s", log)
	case float64(read) < s.minReceiptCoverage*float64(accepted):
		t.Errorf("execution status read for %d of %d accepted transactions (< %.0f%%): inclusion was not measured",
			read, accepted, s.minReceiptCoverage*100)
	}
}

// blockchainPageSize is CometBFT's cap on blocks per /blockchain response.
const blockchainPageSize = 20

// blockchainInfo models just enough of CometBFT /blockchain to sum per-block tx
// counts; like /status, the Sei fork may return it with or without the JSON-RPC
// envelope.
type blockchainInfo struct {
	Result *struct {
		BlockMetas []blockMeta `json:"block_metas"`
	} `json:"result,omitempty"`
	BlockMetas []blockMeta `json:"block_metas"`
}

type blockMeta struct {
	NumTxs string `json:"num_txs"`
}

func (b *blockchainInfo) metas() []blockMeta {
	if b.Result != nil {
		return b.Result.BlockMetas
	}
	return b.BlockMetas
}

// includedTxCount sums num_txs over blocks (from, to] via /blockchain, returning
// early once the sum is positive so a healthy chain pays for one page while only
// the failing case walks the whole window. Pages that stay unreachable after
// retries, or that return fewer blocks than requested (pruned range or an error
// envelope decoding to empty), fail the test — a partial sum that reads as zero
// would defeat the gate.
func includedTxCount(ctx context.Context, t *testing.T, hc *http.Client, tmRPC string, from, to int64) int64 {
	t.Helper()
	var total int64
	for lo := from + 1; lo <= to; lo += blockchainPageSize {
		hi := lo + blockchainPageSize - 1
		hi = min(hi, to)
		url := fmt.Sprintf("%s/blockchain?minHeight=%d&maxHeight=%d", tmRPC, lo, hi)
		var page blockchainInfo
		ok := false
		for attempt := 0; attempt < 3 && !ok; attempt++ {
			if attempt > 0 {
				time.Sleep(2 * time.Second)
			}
			page = blockchainInfo{}
			ok = getJSONInto(ctx, hc, url, &page)
		}
		if !ok {
			t.Fatalf("read %s: unreachable, non-200, or undecodable after retries — cannot verify inclusion", url)
		}
		if got, want := int64(len(page.metas())), hi-lo+1; got != want {
			t.Fatalf("%s returned %d block_metas, want %d — pruned range or error envelope; "+
				"a short page cannot be trusted as zero", url, got, want)
		}
		for _, m := range page.metas() {
			n, err := strconv.ParseInt(m.NumTxs, 10, 64)
			if err != nil {
				t.Fatalf("parse num_txs %q at %s: %v", m.NumTxs, url, err)
			}
			total += n
		}
		if total > 0 {
			return total
		}
	}
	return total
}

// runJob creates a one-shot harness Job (deleted on cleanup) and waits for it
// to complete; a Failed Job fails the suite.
func runJob(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, job *batchv1.Job) {
	t.Helper()
	if _, err := cs.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create job %q: %v", job.Name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		bg := metav1.DeletePropagationBackground
		_ = cs.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	})
	t.Logf("job %s launched (%s)", job.Name, job.Spec.Template.Spec.Containers[0].Image)
	waitJob(ctx, t, cs, job.Namespace, job.Name)
}

// waitJob blocks until a harness Job reaches a terminal condition. A Failed
// Job fails the suite; success returns. Bounded by ctx.
func waitJob(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, name string) {
	t.Helper()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		job, err := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get seiload job %q: %v", name, err)
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return
			}
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				t.Fatalf("job %q failed: %s\n--- pod log (tail) ---\n%s",
					name, cond.Message, podLogTail(ctx, cs, ns, name))
			}
		}
		select {
		case <-ctx.Done():
			// The suite ctx fired (deadline or SIGTERM) — grab the pod log on a
			// fresh ctx (the suite ctx is already dead) so the failure carries the
			// job's last output, not just "deadline".
			logCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			tail := podLogTail(logCtx, cs, ns, name)
			cancel()
			t.Fatalf("job %q did not finish before deadline: %v\n--- pod log (tail) ---\n%s", name, ctx.Err(), tail)
		case <-tick.C:
		}
	}
}

// podLogTail returns the tail of the seiload pod's log for a Job, best-effort —
// the failure-time signal a Job condition message alone cannot give.
func podLogTail(ctx context.Context, cs *kubernetes.Clientset, ns, jobName string) string {
	pod, err := jobPod(ctx, cs, ns, jobName)
	if err != nil {
		return fmt.Sprintf("(%v)", err)
	}
	log, err := podLog(ctx, cs, pod, 50)
	if err != nil {
		return fmt.Sprintf("(read logs failed: %v)", err)
	}
	return log
}

// jobPod returns a harness Job's pod; the Jobs run one (backoffLimit 0).
func jobPod(ctx context.Context, cs *kubernetes.Clientset, ns, jobName string) (*corev1.Pod, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod for job %q: %v", jobName, err)
	}
	return &pods.Items[0], nil
}

// podLog returns the last lines of a pod's log.
func podLog(ctx context.Context, cs *kubernetes.Clientset, pod *corev1.Pod, lines int64) (string, error) {
	raw, err := cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &lines}).DoRaw(ctx)
	return string(raw), err
}
