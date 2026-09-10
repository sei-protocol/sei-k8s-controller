package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeEnsureDataPVC = "ensure-data-pvc"

type EnsureDataPVCParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type ensureDataPVCExecution struct {
	taskBase
	params EnsureDataPVCParams
	cfg    ExecutionConfig
}

func deserializeEnsureDataPVC(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p EnsureDataPVCParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing ensure-data-pvc params: %w", err)
		}
	}
	return &ensureDataPVCExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *ensureDataPVCExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	if dv := node.Spec.DataVolume; dv != nil && dv.Import != nil && dv.Import.PVCName != "" {
		return e.executeImport(ctx, node, dv.Import.PVCName)
	}

	if err := holdForVolumeAttributesClass(node); err != nil {
		return err
	}
	return e.executeCreate(ctx, node)
}

// holdForVolumeAttributesClass blocks provisioning while the node's selected
// VolumeAttributesClass has not pre-flighted True. The node reconciler resolves
// that pre-flight (and owns the condition) before the plan executes, so this
// reads the outcome rather than repeating the cluster read — one read per
// reconcile, and no second writer racing the reconciler's status patch.
//
// A missing class holds rather than failing the plan terminally: a Terminal
// error parks the node in Failed, which needs a delete-and-recreate to leave
// even after the platform adds the class, while a hold lets the same node
// proceed on the next poll. Not provisioning meanwhile is deliberate too — a
// claim binds the name once and the name itself is create-only, so a claim
// created against a missing class can only ever be resolved by adding a class
// of that same name.
//
// Nothing selected is never held: there is no reference to verify, so a
// selection-free node cannot be blocked by this gate even if the condition were
// somehow unresolved.
func holdForVolumeAttributesClass(node *seiv1alpha1.SeiNode) error {
	name := noderesource.VolumeAttributesClassForNode(node)
	if name == nil {
		return nil
	}

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionVolumeAttributesClassReady)
	if cond == nil {
		// Unreachable while the reconciler resolves the condition ahead of plan
		// execution. Fails closed if that order ever changes: provisioning
		// against an unverified name would bind it to the volume for good.
		return fmt.Errorf("VolumeAttributesClass %q pre-flight has not resolved yet", *name)
	}
	if cond.Status != metav1.ConditionTrue {
		return fmt.Errorf("VolumeAttributesClass %q is not ready (%s): %s", *name, cond.Reason, cond.Message)
	}
	return nil
}

// executeCreate is Get-then-Create, failing if an unexpected PVC already
// exists that the SeiNode does not own.
//
// Once-only by design, not a missing update path: an owned PVC is accepted
// as-is. dataVolume.storage is create-only so a size edit cannot diverge it, but
// NodeMode is not, so a mode switch still can (pre-existing). Resizing means
// deleting the node, which discards the volume via its ownerReference.
func (e *ensureDataPVCExecution) executeCreate(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	desired := noderesource.GenerateDataPVC(node, e.cfg.Platform)
	if err := ctrl.SetControllerReference(node, desired, e.cfg.Scheme); err != nil {
		return Terminal(fmt.Errorf("setting owner reference on data PVC: %w", err))
	}

	existing := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	switch err := e.cfg.KubeClient.Get(ctx, key, existing); {
	case apierrors.IsNotFound(err):
		// proceed to Create
	case err != nil:
		return fmt.Errorf("checking for existing data PVC: %w", err)
	default:
		if metav1.IsControlledBy(existing, node) {
			e.complete()
			return nil
		}
		return Terminal(fmt.Errorf(
			"data PVC %q already exists and is not owned by SeiNode %q; "+
				"set spec.dataVolume.import.pvcName to adopt, or delete the PVC",
			existing.Name, node.Name))
	}

	if err := e.cfg.KubeClient.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost the race between Get and Create; requeue so the next
			// reconcile's Get resolves ownership.
			return fmt.Errorf("data PVC created concurrently: %w", err)
		}
		return fmt.Errorf("creating data PVC: %w", err)
	}
	e.complete()
	return nil
}

// executeImport validates an externally-managed PVC and sets the
// ImportPVCReady condition on the node from the validation outcome.
//
// Storage class is intentionally NOT validated — imports are an explicit
// operator act; matching the PVC's StorageClass to the workload's needs
// is the importer's responsibility.
func (e *ensureDataPVCExecution) executeImport(ctx context.Context, node *seiv1alpha1.SeiNode, name string) error {
	err := e.validateImport(ctx, node, name)
	switch {
	case err == nil:
		setImportPVCCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonPVCValidated,
			fmt.Sprintf("PVC %q passes all import requirements", name))
		e.complete()
		return nil
	case isTerminal(err):
		setImportPVCCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonPVCInvalid, err.Error())
		return err
	default:
		setImportPVCCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonPVCNotReady, err.Error())
		return nil
	}
}

// validateImport returns nil when the PVC passes all checks, a Terminal
// error for defects the user must fix, or a plain error for transient
// conditions (retry on next poll).
func (e *ensureDataPVCExecution) validateImport(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	name string,
) error {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: name, Namespace: node.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("PVC %q not found in namespace %q", name, node.Namespace)
		}
		return fmt.Errorf("getting PVC %q: %w", name, err)
	}

	if pvc.DeletionTimestamp != nil {
		return fmt.Errorf("PVC %q is being deleted (deletionTimestamp=%s)", name, pvc.DeletionTimestamp)
	}

	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		// continue
	case corev1.ClaimLost:
		return Terminal(fmt.Errorf("PVC %q phase is Lost; underlying PV is gone", name))
	default:
		return fmt.Errorf("PVC %q phase is %q, waiting for Bound", name, pvc.Status.Phase)
	}

	hasSingleWriter := slices.Contains(pvc.Spec.AccessModes, corev1.ReadWriteOnce) ||
		slices.Contains(pvc.Spec.AccessModes, corev1.ReadWriteOncePod)
	if !hasSingleWriter {
		return Terminal(fmt.Errorf("PVC %q accessModes %v must include ReadWriteOnce or ReadWriteOncePod", name, pvc.Spec.AccessModes))
	}

	// Resolved size, so the floor tracks what this node would be provisioned at.
	_, requiredStr := noderesource.StorageForNode(node, e.cfg.Platform)
	required, parseErr := resource.ParseQuantity(requiredStr)
	if parseErr != nil {
		return Terminal(fmt.Errorf("cannot parse required storage %q: %w", requiredStr, parseErr))
	}
	actual, haveActual := pvc.Status.Capacity[corev1.ResourceStorage]
	if !haveActual {
		return fmt.Errorf("PVC %q has no status.capacity.storage reported yet", name)
	}
	if actual.Cmp(required) < 0 {
		return Terminal(fmt.Errorf("PVC %q capacity %s is less than required %s", name, actual.String(), required.String()))
	}

	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return fmt.Errorf("PVC %q is Bound but has no spec.volumeName", name)
	}
	pv := &corev1.PersistentVolume{}
	if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("underlying PV %q for PVC %q not found", pvName, name)
		}
		return fmt.Errorf("getting PV %q: %w", pvName, err)
	}
	if pv.Status.Phase == corev1.VolumeFailed {
		return Terminal(fmt.Errorf("underlying PV %q for PVC %q is in phase Failed", pvName, name))
	}
	pvCap, havePVCap := pv.Spec.Capacity[corev1.ResourceStorage]
	if !havePVCap || pvCap.Cmp(actual) != 0 {
		return Terminal(fmt.Errorf("underlying PV %q capacity %s does not match PVC %q capacity %s",
			pvName, pvCap.String(), name, actual.String()))
	}
	return nil
}

func (e *ensureDataPVCExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}

func isTerminal(err error) bool {
	var te *TerminalError
	return errors.As(err, &te)
}

func setImportPVCCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionImportPVCReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
