package noderesource

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

// statefulSetFieldOwner is the SSA fieldManager recorded on every
// Apply issued by SyncStatefulSet. Sharing the same name across the
// controller and the apply-statefulset task keeps managedFields
// entries unified rather than fragmenting ownership. Matches the
// fieldOwner used by sibling tasks (apply_service,
// apply_rbac_proxy_config).
const statefulSetFieldOwner = client.FieldOwner("seinode-controller")

// SyncStatefulSet brings the live StatefulSet for node into line with
// the rendered desired spec using Server-Side Apply on the typed
// `*appsv1.StatefulSet` — defaulting-immune by design, so apiserver-
// applied PodSpec defaults (RestartPolicy, DNSPolicy, etc.) never
// surface as divergence.
//
// Behaviors:
//
//   - Spec.Paused=true forces Replicas=0 in the desired render.
//     Pausing during an in-flight rollout effectively fast-forwards
//     the rollout: pods terminate, and the next unpause brings up a
//     fresh pod from the current (post-update) template.
//
//   - When Status.StatefulSet tracks a UID and the live object has a
//     different one, the impostor is deleted and this call returns
//     (nil, nil) without applying. The next reconcile observes the
//     impostor gone and applies a fresh StatefulSet. Splitting the
//     delete and the apply across reconciles keeps each call against
//     a clean apiserver state — applying onto a still-deleting object
//     (DeletionTimestamp set) has undefined SSA semantics if a future
//     finalizer ever lands on the resource.
//
//   - A node with spec.nodeConfig is held at its current StatefulSet while
//     either referenced ConfigMap cannot be mounted or loaded; the call
//     returns an error wrapping ErrNodeConfigUnresolved without applying.
//     The StatefulSet rolls its pod as soon as the template changes, so
//     applying a reference that does not resolve would replace a working
//     pod with one stuck in ContainerCreating or crash-looping on bad TOML.
//     configMaps reads the ConfigMaps; pass an uncached reader.
//
// Returns the live StatefulSet on a normal Apply path. Returns
// (nil, nil) when the impostor was deleted this reconcile and callers
// should requeue without further work. Callers update
// Status.StatefulSet from the returned object; a nil return means the
// stored UID stays in place, and the next reconcile will observe
// NotFound (impostor gone) and Apply a fresh STS whose new UID gets
// stamped onto Status.StatefulSet.
//
// Deletion uses the default Background propagation. StatefulSet pods
// own no resources we care about preserving here; volumeClaimTemplates
// PVCs are not garbage-collected with the StatefulSet (they carry no
// blocking ownerRef back to the STS, and
// PersistentVolumeClaimRetentionPolicy defaults to WhenDeleted=Retain).
// Foreground propagation would add a finalizer that blocks STS
// removal until pods drain — unnecessary for impostor swap, and fatal
// in envtest where no kube-controller-manager exists to reap pods.
func SyncStatefulSet(
	ctx context.Context,
	c client.Client,
	configMaps client.Reader,
	scheme *runtime.Scheme,
	node *seiv1alpha1.SeiNode,
	platform PlatformConfig,
) (*appsv1.StatefulSet, error) {
	desired, err := GenerateStatefulSet(node, platform)
	if err != nil {
		return nil, fmt.Errorf("generating statefulset: %w", err)
	}
	desired.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	if err := ctrl.SetControllerReference(node, desired, scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference: %w", err)
	}
	if node.Spec.Paused {
		desired.Spec.Replicas = new(int32)
	}

	key := client.ObjectKeyFromObject(desired)

	// Impostor recovery: a UID mismatch between the tracked Status and
	// the live object means an operator deleted and recreated the
	// StatefulSet out-of-band. Issue Delete and return without applying
	// — the next reconcile (triggered by the StatefulSet Owns watch
	// firing on the delete event) observes a clean NotFound and Applies
	// fresh. The Delete is idempotent; re-entering this branch before
	// the cache catches up just no-ops.
	if node.Status.StatefulSet != nil {
		existing := &appsv1.StatefulSet{}
		switch err := c.Get(ctx, key, existing); {
		case err == nil:
			if existing.UID != node.Status.StatefulSet.UID {
				if err := c.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("deleting impostor statefulset: %w", err)
				}
				// Forget the stale identity so any subsequent
				// SyncStatefulSet call this reconcile (or the next)
				// routes through the adopt-on-observe path. Leaving
				// the stale UID would cause a freshly Applied STS to
				// be misclassified as another impostor on the very
				// next reconcile, producing a churn loop.
				node.Status.StatefulSet = nil
				return nil, nil
			}
		case apierrors.IsNotFound(err):
			// Live object missing; the Apply below recreates it.
		default:
			return nil, fmt.Errorf("fetching tracked statefulset: %w", err)
		}
	}

	if err := CheckNodeConfig(ctx, configMaps, node); err != nil {
		return nil, err
	}

	// client.Apply decodes the apiserver response into desired in-place,
	// so its UID and ResourceVersion reflect the live object after this
	// call returns. No re-Get is needed (and a re-Get against the cache
	// can race with watch propagation, returning NotFound on the very
	// first Apply).
	//nolint:staticcheck // migrating to typed ApplyConfiguration builders is a separate effort
	if err := c.Patch(ctx, desired, client.Apply, statefulSetFieldOwner, client.ForceOwnership); err != nil {
		return nil, fmt.Errorf("applying statefulset: %w", err)
	}
	return desired, nil
}

// ErrNodeConfigUnresolved marks a spec.nodeConfig whose ConfigMaps cannot be
// mounted or loaded. SyncStatefulSet holds the StatefulSet while it holds.
var ErrNodeConfigUnresolved = errors.New("nodeConfig unresolved")

// CheckNodeConfig verifies that both ConfigMaps named by spec.nodeConfig exist,
// carry their key, and parse as TOML. Nil for a node without nodeConfig.
// Failures wrap ErrNodeConfigUnresolved; a failed read is returned as is.
func CheckNodeConfig(ctx context.Context, r client.Reader, node *seiv1alpha1.SeiNode) error {
	cfg := node.Spec.NodeConfig
	if cfg == nil {
		return nil
	}
	files := []struct {
		ref  seiv1alpha1.ConfigFileRef
		file string
	}{
		{cfg.ConfigRef, ConfigTomlKey},
		{cfg.AppRef, AppTomlKey},
	}
	for _, f := range files {
		cm := &corev1.ConfigMap{}
		key := types.NamespacedName{Name: f.ref.Name, Namespace: node.Namespace}
		if err := r.Get(ctx, key, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("%w: configmap %q not found", ErrNodeConfigUnresolved, f.ref.Name)
			}
			return fmt.Errorf("getting configmap %q: %w", f.ref.Name, err)
		}
		content, ok := cm.Data[f.file]
		if !ok {
			raw, binary := cm.BinaryData[f.file]
			if !binary {
				return fmt.Errorf("%w: configmap %q has no %q key", ErrNodeConfigUnresolved, f.ref.Name, f.file)
			}
			content = string(raw)
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("%w: configmap %q key %q is empty", ErrNodeConfigUnresolved, f.ref.Name, f.file)
		}
		if _, err := tomlpatch.UnmarshalTOML([]byte(content)); err != nil {
			return fmt.Errorf("%w: configmap %q key %q is not valid TOML: %w", ErrNodeConfigUnresolved, f.ref.Name, f.file, err)
		}
	}
	return nil
}
