package taskimg

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cmFieldOwner identifies this library to server-side-apply conflict detection.
const cmFieldOwner client.FieldOwner = "seitask"

// EnsureWorkflowVarsCM creates the per-run workflow-vars ConfigMap with an
// ownerRef + optional seed entries. AlreadyExists is treated as success
// (idempotent). Called before any SetVar.
func EnsureWorkflowVarsCM(ctx context.Context, c client.Client, w WorkflowIdentity, seed map[VarKey]string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            WorkflowVarsName(w.Name),
			Namespace:       w.Namespace,
			OwnerReferences: []metav1.OwnerReference{w.OwnerRef()},
		},
		Data: stringifyKeys(seed),
	}
	err := c.Create(ctx, cm, cmFieldOwner)
	if err == nil {
		return nil
	}
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return Infra(fmt.Errorf("creating workflow-vars ConfigMap: %w", err))
}

// SetVar writes one key via SetVars.
func SetVar(ctx context.Context, c client.Client, w WorkflowIdentity, key VarKey, value string) error {
	return SetVars(ctx, c, w, map[VarKey]string{key: value})
}

// SetVars merges multiple keys with MergeFromWithOptimisticLock (matches the
// status-patch discipline in CLAUDE.md). Caller retries on IsConflict if the
// flow is known-racy.
func SetVars(ctx context.Context, c client.Client, w WorkflowIdentity, kv map[VarKey]string) error {
	if len(kv) == 0 {
		return nil
	}
	current := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: WorkflowVarsName(w.Name)}, current); err != nil {
		return Infra(fmt.Errorf("reading workflow-vars ConfigMap: %w", err))
	}
	patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if current.Data == nil {
		current.Data = map[string]string{}
	}
	for k, v := range kv {
		current.Data[string(k)] = v
	}
	if err := c.Patch(ctx, current, patch, cmFieldOwner); err != nil {
		return Infra(fmt.Errorf("patching workflow-vars ConfigMap: %w", err))
	}
	return nil
}

// WriteExitReason stamps EXIT_REASON from err's classification. Write errors
// are intentionally swallowed — the underlying failure already determined
// the exit code and shouldn't be masked by a CM-write failure.
func WriteExitReason(ctx context.Context, c client.Client, w WorkflowIdentity, err error) {
	_ = SetVar(ctx, c, w, KeyExitReason, string(ExitReasonFor(err)))
}

func stringifyKeys(in map[VarKey]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}
