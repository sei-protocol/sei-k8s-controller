// Package keygen implements `seitask keygen`: derive a Sei account via the
// general internal/keygen primitive, write the mnemonic to a per-run Secret named
// "<keyName>-<workflowName>", and publish ADMIN_ADDRESS / ADMIN_SECRET_NAME
// to workflow-vars. All created resources carry an ownerRef to the parent
// Workflow CR for cascade GC. The key derivation itself lives in
// internal/keygen (k8s-free, reused by the test harness); this package is the
// seitask-runner's Secret/workflow-vars writer on top of it.
package keygen

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keyderive "github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

const fieldOwner client.FieldOwner = "seitask-keygen"

// Params carries the typed inputs to Run.
type Params struct {
	// KeyName is the logical identity (e.g. "admin"). Secret name is
	// "<KeyName>-<WorkflowName>" to disambiguate concurrent runs.
	KeyName  string
	Workflow taskruntime.WorkflowIdentity
}

type Result struct {
	SecretName string
	Address    string
}

// Run generates the keypair, writes the Secret, and stamps workflow-vars.
// Idempotent: re-running on an existing Secret reuses the key.
func Run(ctx context.Context, c client.Client, p Params) (Result, error) {
	if p.KeyName == "" {
		return Result{}, fmt.Errorf("keygen: empty KeyName")
	}
	if p.Workflow.Name == "" || p.Workflow.Namespace == "" {
		return Result{}, fmt.Errorf("keygen: workflow identity not loaded (downward-API env not projected)")
	}

	secretName := p.KeyName + "-" + p.Workflow.Name

	// Check for an existing Secret first — re-running keygen on an already-
	// initialized run should be a no-op so manual retries don't rotate the
	// key out from under downstream steps.
	existing := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: p.Workflow.Namespace, Name: secretName}, existing)
	switch {
	case err == nil:
		// Re-stamp the workflow-vars CM in case it was cleared, then return.
		addr, exists := existing.Data["address"]
		if !exists {
			return Result{}, taskruntime.Infra(fmt.Errorf("existing Secret %q is missing address data", secretName))
		}
		if err := writeWorkflowVars(ctx, c, p.Workflow, string(addr), secretName); err != nil {
			return Result{}, err
		}
		return Result{SecretName: secretName, Address: string(addr)}, nil
	case !apierrors.IsNotFound(err):
		return Result{}, taskruntime.Infra(fmt.Errorf("reading existing Secret %q: %w", secretName, err))
	}

	id, err := keyderive.Derive()
	if err != nil {
		return Result{}, taskruntime.Infra(fmt.Errorf("deriving identity: %w", err))
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretName,
			Namespace:       p.Workflow.Namespace,
			OwnerReferences: []metav1.OwnerReference{p.Workflow.OwnerRef()},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			keyderive.SecretMnemonicKey: []byte(id.Mnemonic),
			// address is duplicated into the Secret so a re-run of keygen
			// can reuse the existing identity without re-deriving from the
			// mnemonic (the Secret is the source of truth for both).
			"address": []byte(id.Address),
		},
	}
	if err := c.Create(ctx, secret, fieldOwner); err != nil {
		// Race: another keygen Pod won. Re-read and fall through to
		// idempotent path.
		if apierrors.IsAlreadyExists(err) {
			return Run(ctx, c, p)
		}
		return Result{}, taskruntime.Infra(fmt.Errorf("creating Secret %q: %w", secretName, err))
	}

	if err := writeWorkflowVars(ctx, c, p.Workflow, id.Address, secretName); err != nil {
		return Result{}, err
	}
	return Result{SecretName: secretName, Address: id.Address}, nil
}

func writeWorkflowVars(ctx context.Context, c client.Client, w taskruntime.WorkflowIdentity, address, secretName string) error {
	if err := taskruntime.EnsureWorkflowVarsCM(ctx, c, w, map[taskruntime.VarKey]string{
		taskruntime.KeyRunID: w.Name,
	}); err != nil {
		return err
	}
	return taskruntime.SetVars(ctx, c, w, map[taskruntime.VarKey]string{
		taskruntime.KeyAdminAddress:    address,
		taskruntime.KeyAdminSecretName: secretName,
	})
}
