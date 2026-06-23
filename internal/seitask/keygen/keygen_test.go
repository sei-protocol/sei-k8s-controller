package keygen

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keyderive "github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

const (
	testKeyName        = "admin"
	testNamespace      = "nightly"
	testWorkflowName   = "wf-test"
	testSecretName     = "admin-wf-test"
	testWorkflowVarsCM = "workflow-vars-wf-test"
)

func testWorkflow() taskruntime.WorkflowIdentity {
	return taskruntime.WorkflowIdentity{Name: testWorkflowName, UID: "uid-test", Namespace: testNamespace}
}

// Sanity check: keygen produces a Secret with the right shape + a
// workflow-vars ConfigMap with the right keys + the address is a valid
// "sei1..." bech32.
func TestRun_CreatesSecretAndWorkflowVars(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := testWorkflow()

	res, err := Run(context.Background(), c, Params{KeyName: testKeyName, Workflow: w})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SecretName != testSecretName {
		t.Fatalf("SecretName: got %q, want admin-wf-test", res.SecretName)
	}
	if !strings.HasPrefix(res.Address, "sei1") {
		t.Fatalf("Address %q does not have sei1 prefix", res.Address)
	}

	// Secret must carry the mnemonic + address.
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testSecretName}, secret); err != nil {
		t.Fatalf("Get Secret: %v", err)
	}
	mnemonic, ok := secret.Data[keyderive.SecretMnemonicKey]
	if !ok || len(mnemonic) == 0 {
		t.Fatalf("mnemonic missing from Secret")
	}
	// 24 words separated by spaces.
	if got := len(strings.Fields(string(mnemonic))); got != 24 {
		t.Fatalf("mnemonic word count: got %d, want 24", got)
	}
	if got := string(secret.Data["address"]); got != res.Address {
		t.Fatalf("Secret address %q != Result.Address %q", got, res.Address)
	}

	// workflow-vars ConfigMap must carry ADMIN_ADDRESS + ADMIN_SECRET_NAME.
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, cm); err != nil {
		t.Fatalf("Get CM: %v", err)
	}
	if got := cm.Data[string(taskruntime.KeyAdminAddress)]; got != res.Address {
		t.Fatalf("CM ADMIN_ADDRESS = %q, want %q", got, res.Address)
	}
	if got := cm.Data[string(taskruntime.KeyAdminSecretName)]; got != testSecretName {
		t.Fatalf("CM ADMIN_SECRET_NAME = %q, want admin-wf-test", got)
	}
	if got := cm.Data[string(taskruntime.KeyRunID)]; got != testWorkflowName {
		t.Fatalf("CM RUN_ID = %q, want wf-test", got)
	}
}

// Idempotency: re-running keygen with an existing Secret reuses the key
// rather than rotating it. Manual scenario retries shouldn't blow away the
// identity downstream Tasks already consumed.
func TestRun_Idempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := testWorkflow()

	first, err := Run(context.Background(), c, Params{KeyName: testKeyName, Workflow: w})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Drop the workflow-vars CM to simulate it being cleared somewhere.
	cm := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, cm)
	if err := c.Delete(context.Background(), cm); err != nil {
		t.Fatalf("delete CM: %v", err)
	}

	second, err := Run(context.Background(), c, Params{KeyName: testKeyName, Workflow: w})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Address != first.Address {
		t.Fatalf("identity rotated on second Run: %q -> %q", first.Address, second.Address)
	}
	// CM should be re-created.
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testWorkflowVarsCM}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("CM not re-created on idempotent run: %v", err)
	}
}

// Reject missing inputs.
func TestRun_RejectsBadInputs(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	for _, tc := range []struct {
		name string
		p    Params
	}{
		{"empty key name", Params{KeyName: "", Workflow: testWorkflow()}},
		{"missing workflow name", Params{KeyName: testKeyName, Workflow: taskruntime.WorkflowIdentity{Namespace: "ns"}}},
		{"missing workflow namespace", Params{KeyName: testKeyName, Workflow: taskruntime.WorkflowIdentity{Name: "wf"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(context.Background(), c, tc.p)
			if err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

// Secret + ConfigMap carry ownerReferences to the parent Workflow so
// Workflow deletion cascades. Bench-chain scenarios depend on this for
// cleanup (no trap-on-EXIT logic).
func TestRun_StampsOwnerReferences(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := testWorkflow()

	if _, err := Run(context.Background(), c, Params{KeyName: testKeyName, Workflow: w}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, target := range []client.Object{
		&corev1.Secret{},
		&corev1.ConfigMap{},
	} {
		var name string
		if _, ok := target.(*corev1.Secret); ok {
			name = testSecretName
		} else {
			name = testWorkflowVarsCM
		}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, target); err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		refs := target.GetOwnerReferences()
		if len(refs) != 1 || refs[0].Kind != "Workflow" || refs[0].Name != testWorkflowName || string(refs[0].UID) != "uid-test" {
			t.Fatalf("%s ownerRefs = %+v", name, refs)
		}
	}
}
