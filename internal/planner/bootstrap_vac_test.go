package planner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// The re-bootstrap route into the VolumeAttributesClass gate, driven through
// the real plan builder and the real executor rather than the task alone.
//
// buildBootstrapPlan opens with ensure-data-pvc, exactly as buildBasePlan does,
// so a node that is re-bootstrapped re-executes the task against a data PVC
// that already exists. That is the realistic trigger: the node is healthy, its
// claim is bound, and the class it named at provision time has since left the
// catalog — the catalog suffixes generations (…-v1, …-v2) precisely because VAC
// parameters are immutable, so retiring a -v1 is routine.
//
// If the gate ran ahead of the owned-claim short-circuit, ensure-data-pvc would
// return a plain error here and the plan would never leave task 0: a
// non-Terminal Execute error requeues on TaskPollInterval without charging
// RetryCount or consulting MaxRetries, so the node loops with no floor.
func TestExecutePlan_Bootstrap_ExistingOwnedPVC_ClassRetired_AdvancesPastEnsurePVC(t *testing.T) {
	const retiredClass = "sei-gp3-performance-v1"
	s := testScheme(t)
	node := bootstrapNodeSelectingVAC("rebootstrap-node", retiredClass)

	// What the reconciler resolved this reconcile: the class is gone.
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:    seiv1alpha1.ConditionVolumeAttributesClassReady,
		Status:  metav1.ConditionFalse,
		Reason:  seiv1alpha1.ReasonVolumeAttributesClassNotFound,
		Message: "VolumeAttributesClass \"" + retiredClass + "\" not found",
	})

	if !NeedsBootstrap(node) {
		t.Fatal("fixture must take the bootstrap route — buildBasePlan is the other caller")
	}
	plan, err := (&fullNodePlanner{platform: platformtest.Config()}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Tasks) == 0 || plan.Tasks[0].Type != task.TaskTypeEnsureDataPVC {
		t.Fatalf("bootstrap plan must open with %q, got %+v", task.TaskTypeEnsureDataPVC, plan.Tasks)
	}
	node.Status.Plan = plan

	existing := provisionedDataPVC(t, node, s)
	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node, existing).
		WithStatusSubresource(&seiv1alpha1.SeiNode{})
	executor := vacPlatformExecutor(builder, s)

	if _, err := executor.ExecutePlan(context.Background(), node, plan); err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	if got := plan.Tasks[0].Status; got != seiv1alpha1.TaskComplete {
		t.Fatalf("ensure-data-pvc status = %q, want %q — a re-bootstrap on a provisioned "+
			"volume must not be held by a class the claim already spent (error: %q)",
			got, seiv1alpha1.TaskComplete, plan.Tasks[0].Error)
	}
	if plan.Phase == seiv1alpha1.TaskPlanFailed {
		t.Fatalf("plan failed: %+v", plan.Tasks[0])
	}
}

// bootstrapNodeSelectingVAC returns a full node that takes the bootstrap route
// (NeedsBootstrap) and selects the named VolumeAttributesClass.
func bootstrapNodeSelectingVAC(name, vac string) *seiv1alpha1.SeiNode {
	node := testNode()
	node.Name = name
	node.UID = k8stypes.UID("uid-" + name)
	node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
		Storage: &seiv1alpha1.DataVolumeStorage{VolumeAttributesClassName: &vac},
	}
	node.Spec.FullNode = &seiv1alpha1.FullNodeSpec{
		Snapshot: &seiv1alpha1.SnapshotSource{
			BootstrapImage: "bootstrap:v1",
			S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 1000},
		},
	}
	return node
}

// provisionedDataPVC returns the data PVC as an earlier run of ensure-data-pvc
// left it: controller-owned, Bound, and carrying the class that was current
// when it was created.
func provisionedDataPVC(t *testing.T, node *seiv1alpha1.SeiNode, s *k8sruntime.Scheme) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := noderesource.GenerateDataPVC(node, platformtest.Config())
	if err := ctrl.SetControllerReference(node, pvc, s); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = "pv-" + pvc.Name
	pvc.Status.Phase = corev1.ClaimBound
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2000Gi")}
	return pvc
}

// vacPlatformExecutor is nodeExecutor with a populated Platform, which
// ensure-data-pvc needs to resolve the claim's storage class and size.
func vacPlatformExecutor(c *fake.ClientBuilder, s *k8sruntime.Scheme) *Executor[*seiv1alpha1.SeiNode] {
	fc := c.Build()
	return &Executor[*seiv1alpha1.SeiNode]{
		ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
			return task.ExecutionConfig{
				BuildSidecarClient: func() (task.SidecarClient, error) { return &mockSidecarClient{}, nil },
				KubeClient:         fc,
				APIReader:          fc,
				Scheme:             s,
				Resource:           node,
				Platform:           platformtest.Config(),
			}
		},
	}
}
