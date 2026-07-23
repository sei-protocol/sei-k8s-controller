//go:build integration

package integration

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// releaseAdminBalance funds the admin account in genesis so the release-test
// harness can sign and pay for the txs it issues.
const releaseAdminBalance = "1000000000000usei"

// The release-test solo-precompile suite verifies the solo precompile cannot
// drain a vested (locked) balance. Since sei-chain deprecated live
// MsgCreateVestingAccount on fresh chains, that fixture can no longer be built
// by a tx mid-test; instead the harness seeds it in genesis. vestingFixture*
// define a continuous vesting account: vestingFixtureLocked of its
// vestingFixtureBalance is locked on a linear schedule to vestingFixtureEndTime
// (2030-01-01), so the locked portion stays locked for the whole run while the
// remainder is spendable. The address + mnemonic reach the suite via the
// SEI_VESTING_* env below.
//
// Two load-bearing invariants (both confirmed by xreview):
//   - Balance MUST exceed Locked. The unlocked remainder (~1 sei here) is what
//     the account spends to associate + pay fees in the test; if Locked == Balance
//     the account has nothing spendable and the test fails in setup, not on its
//     assertion.
//   - The lock holds only because seictl's genesis assembler anchors the
//     continuous schedule's StartTime at genesis time (not epoch 0). That lives
//     in seictl, outside this file; if it ever changed, the fixture could be
//     near-fully-vested and the anti-drain assertion would weaken silently. A
//     run-time guard asserting nonzero locked balance is a tracked follow-up.
const (
	vestingFixtureBalance = "2000000usei"
	vestingFixtureLocked  = "1000000usei"
	// vestingFixtureEndTime is 2030-01-01T00:00:00Z: far past any run, and
	// safely > genesis time (the assembler rejects EndTime <= genesis).
	vestingFixtureEndTime = 1893456000
)

// releaseBaseConfig is the seid config the release chain runs with: the memiavl
// storage baseline (the nightly image rejects the cosmos_only default) plus kv tx
// indexing (the harness queries txs) and a short mempool TTL.
var releaseBaseConfig = mergeConfig(memiavlStorageConfig, map[string]string{
	"tx_index.indexer":     "kv",
	"mempool.ttl_duration": "60s",
})

// releaseLegacyEVMAPIs are the legacy sei_* EVM APIs the release-test's stateful
// sequences exercise (sei_newFilter / sei_getFilterLogs need a single consistent
// filter-store, which is why the suite runs exactly one RPC node).
const releaseLegacyEVMAPIs = "sei_getLogs,sei_getBlockByNumber,sei_getBlockByHash,sei_getSeiAddress," +
	"sei_getEVMAddress,sei_getCosmosTx,sei_getEvmTx,sei_newFilter,sei_getFilterLogs"

// releaseRPCConfig overlays the follower-only knobs on releaseBaseConfig: a low
// RPC lag threshold and the legacy EVM APIs above.
var releaseRPCConfig = map[string]string{
	"network.rpc.lag_threshold":   "2",
	"evm.enabled_legacy_sei_apis": releaseLegacyEVMAPIs,
}

// TestNightlyRelease drives the release-validation flow: provision a 4-validator
// chain + one EVM-serving RPC follower, generate a funded admin account, and run
// the external release-test image against the RPC node as a Job. The release-test
// image owns the functional assertions (TEST_TARGET=chain-agnostic); the suite's
// job is to stand up the chain, hand the harness its endpoints + admin key, and
// gate on the Job's exit code.
//
// One RPC node (not the load suite's two) is deliberate: the harness runs
// stateful EVM-filter and send-then-wait sequences that need one consistent
// mempool + filter-store view.
//
// Inputs (env): SEI_CHAIN_ID, SEID_IMAGE [required], RELEASE_TEST_IMAGE
// (the external harness) [required]; SEI_NAMESPACE [optional]. Run with
// -test.timeout 0 (see TestNightlyBenchmark).
func TestNightlyRelease(t *testing.T) {
	requireCluster(t)
	chainID := runChainID(mustEnv(t, "SEI_CHAIN_ID"))
	seid := mustEnv(t, "SEID_IMAGE")
	releaseImage := mustEnv(t, "RELEASE_TEST_IMAGE")
	ns := envOr("SEI_NAMESPACE", "")
	runLabels := map[string]string{runLabelKey: chainID}

	// Generous envelope: the external chain-agnostic harness is a large suite
	// (each test file re-creates + funds + associates users) and runs well past
	// half an hour against a single RPC node; size the ctx above the Job deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	c := openClient(ctx, t)
	cs := clientset(t)

	// Admin account: derive a funded identity the release-test harness signs with.
	admin, err := keygen.Derive()
	if err != nil {
		t.Fatalf("derive admin key: %v", err)
	}

	// Vesting fixture: a second identity seeded with a genesis vesting schedule
	// for the solo-precompile locked-balance test (see vestingFixture* above).
	vesting, err := keygen.Derive()
	if err != nil {
		t.Fatalf("derive vesting key: %v", err)
	}

	// Provision: 4 validators with the admin + vesting fixture funded in genesis,
	// + 1 RPC follower.
	ch, err := provision(ctx, t, c, spec{
		chainID:       chainID,
		runID:         chainID,
		namespace:     ns,
		seidImage:     seid,
		validators:    4,
		rpcNodes:      1,
		storageConfig: releaseBaseConfig,
		rpcConfig:     releaseRPCConfig,
		accounts: []sei.GenesisAccount{
			{Address: admin.Address, Balance: releaseAdminBalance},
			{Address: vesting.Address, Balance: vestingFixtureBalance, Vesting: &sei.GenesisAccountVesting{
				Amount:  vestingFixtureLocked,
				EndTime: vestingFixtureEndTime,
			}},
		},
	})
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	node := ch.rpcNodes[0]
	net := ch.network
	rpcName := node.Name()
	t.Logf("network %s: ready (4 validators, admin %s funded)", chainID, admin.Address)

	hc := &http.Client{Timeout: 10 * time.Second}

	// Run the external release-test image once against the RPC node; its exit code
	// is the functional verdict.
	runReleaseTest(ctx, t, cs, releaseRunParams{
		hc:        hc,
		net:       net,
		node:      node,
		admin:     admin,
		vesting:   vesting,
		image:     releaseImage,
		runLabels: runLabels,
		label:     "",
	})

	// The chain stayed live through the release suite: the follower is still
	// caught up (it can't catch up to a halted chain, so this covers quorum).
	if err := sei.WaitCaughtUp(ctx, hc, node.TendermintRPC()); err != nil {
		t.Errorf("post-release %s not caught up: %v", rpcName, err)
	}
	t.Logf("chain live post-release — TestNightlyRelease OK")
}

// releaseRunParams selects a single conformance run: the RPC node the external
// harness drives, the funded admin identity it signs with, and a DNS-safe label
// distinguishing this run's resources ("" for TestNightlyRelease's lone run, "v2"
// for the mixed-release suite's run against its v2 follower). The chain id and
// namespace are read from net, which is authoritative (provision names the network
// after the chain id; env may leave the namespace "" for the SDK to resolve).
type releaseRunParams struct {
	hc        *http.Client
	net       *sei.Network
	node      *sei.Node
	admin     keygen.Identity
	vesting   keygen.Identity // genesis-seeded vesting account for the locked-balance suite
	image     string
	runLabels map[string]string
	label     string
}

// runReleaseTest runs the external release-test conformance harness once against a
// single RPC node and gates on its exit code. In order it: probes REST actually
// serves (the status advertises REST as soon as the endpoint is composed, but the
// LCD listener binds later than the EVM one, so a cold REST surfaces here, not
// mid-test), hands the admin mnemonic to the pod via a labeled Secret, launches the
// harness as a one-shot Job wired to the node's endpoints, waits for the Job's
// verdict, and archives the harness log tail — even on success, since exit 0 alone
// doesn't show which sub-cases ran, so a skip-but-pass is otherwise invisible. The
// caller owns all pre/post chain-health assertions.
//
// Exactly one node with exclusive chain access is deliberate: the harness runs
// stateful EVM-filter and send-then-wait sequences that need one consistent mempool
// + filter-store view.
func runReleaseTest(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, p releaseRunParams) {
	t.Helper()
	chainID := p.net.Name()
	ns := p.net.Namespace()

	rest := p.node.REST()
	if rest == "" {
		t.Fatalf("rpc node %q exposes no REST endpoint (release-test needs SEI_REST_ENDPOINT)", p.node.Name())
	}
	if err := sei.WaitRESTServing(ctx, p.hc, rest); err != nil {
		t.Fatalf("rpc node %q REST serving: %v", p.node.Name(), err)
	}
	t.Logf("rpc node %s: REST serving at %s", p.node.Name(), rest)

	// DNS-safe per-run resource names: "admin-<chain>" / "release-test-<chain>" for
	// the lone run, "…-<label>-<chain>" when a suite runs more than one.
	namePart := chainID
	if p.label != "" {
		namePart = p.label + "-" + chainID
	}
	secretName := "admin-" + namePart
	vestingSecretName := "vesting-" + namePart

	// Hand the admin + vesting-fixture mnemonics to the harness via Secrets
	// (secretKeyRef), labeled for the GC sweep and deleted on cleanup.
	createMnemonicSecret(ctx, t, cs, ns, secretName, p.runLabels, p.admin.Mnemonic)
	createMnemonicSecret(ctx, t, cs, ns, vestingSecretName, p.runLabels, p.vesting.Mnemonic)

	job := releaseJob(releaseParams{
		name:              "release-test-" + namePart,
		namespace:         ns,
		image:             p.image,
		runID:             chainID,
		chainID:           chainID,
		adminAddr:         p.admin.Address,
		secretName:        secretName,
		vestingAddr:       p.vesting.Address,
		vestingSecretName: vestingSecretName,
		tmRPC:             p.node.TendermintRPC(),
		evmRPC:            p.node.EVMRPC(),
		rest:              rest,
	})
	if _, err := cs.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create release-test job %q: %v", job.Name, err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		bg := metav1.DeletePropagationBackground
		_ = cs.BatchV1().Jobs(ns).Delete(delCtx, job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	})
	t.Logf("release-test job %s launched (%s)", job.Name, p.image)

	waitJob(ctx, t, cs, ns, job.Name)
	t.Logf("release-test job %s completed; harness log tail:\n%s", job.Name, podLogTail(ctx, cs, ns, job.Name))
}

// createMnemonicSecret writes the admin mnemonic to a Secret the release-test pod
// reads via secretKeyRef. Labeled for the GC sweep and deleted on cleanup,
// matching how the suite manages everything else it creates.
func createMnemonicSecret(
	ctx context.Context, t *testing.T, cs *kubernetes.Clientset,
	ns, name string, labels map[string]string, mnemonic string,
) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{keygen.SecretMnemonicKey: []byte(mnemonic)},
	}
	if _, err := cs.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create mnemonic secret %q: %v", name, err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = cs.CoreV1().Secrets(ns).Delete(delCtx, name, metav1.DeleteOptions{})
	})
}

// releaseParams are the per-run inputs to the release-test Job.
type releaseParams struct {
	name, namespace, image, runID string
	chainID, adminAddr            string
	secretName                    string
	vestingAddr                   string
	vestingSecretName             string
	tmRPC, evmRPC, rest           string
}

// releaseJob builds the release-test Job: the external harness image, fed the
// chain endpoints + admin identity, run once (no retry) with a self-terminating
// deadline. No securityContext: nightly is an unenforced-PSS namespace, so this
// avoids imposing one the harness image may not tolerate (it writes a keyring).
func releaseJob(p releaseParams) *batchv1.Job {
	backoff := int32(0)
	deadline := int64(60 * 60) // the chain-agnostic harness runs >35m against one RPC node; generous cap
	ttl := int32(86400)        // GC the finished Job after a day (matches seiload)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.name,
			Namespace: p.namespace,
			Labels:    map[string]string{runLabelKey: p.runID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{runLabelKey: p.runID}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "release-test",
						Image: p.image,
						// The release-test image reads both the SEI_* names and the
						// RPC_*/CHAIN_ID/ADMIN_ADDRESS names; provide both so a
						// sub-case reading e.g. RPC_EVM_RPC_LIST isn't silently unset
						// (which would skip-but-exit-0).
						Env: []corev1.EnvVar{
							{Name: "TEST_TARGET", Value: "chain-agnostic"},
							{Name: "SEI_CHAIN_ID", Value: p.chainID},
							{Name: "SEI_ADMIN_ADDRESS", Value: p.adminAddr},
							{Name: "SEI_VESTING_ADDRESS", Value: p.vestingAddr},
							{Name: "SEI_TENDERMINT_RPC", Value: p.tmRPC},
							{Name: "SEI_EVM_JSON_RPC", Value: p.evmRPC},
							{Name: "SEI_REST_ENDPOINT", Value: p.rest},
							// The RPC_*/CHAIN_ID/ADMIN_ADDRESS aliases the image also reads.
							{Name: "CHAIN_ID", Value: p.chainID},
							{Name: "ADMIN_ADDRESS", Value: p.adminAddr},
							{Name: "VESTING_ADDRESS", Value: p.vestingAddr},
							{Name: "RPC_TM_RPC", Value: p.tmRPC},
							{Name: "RPC_EVM_RPC", Value: p.evmRPC},
							{Name: "RPC_EVM_RPC_LIST", Value: p.evmRPC}, // single RPC node → one-element list
							{Name: "RPC_REST", Value: p.rest},
							{Name: "SEI_ADMIN_MNEMONIC", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: p.secretName},
									Key:                  keygen.SecretMnemonicKey,
								},
							}},
							{Name: "SEI_VESTING_MNEMONIC", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: p.vestingSecretName},
									Key:                  keygen.SecretMnemonicKey,
								},
							}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
						},
					}},
				},
			},
		},
	}
}
