package planner

import (
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func TestValidatorPlanner_Validate_SigningKey(t *testing.T) {
	cases := []struct {
		name    string
		spec    seiv1alpha1.ValidatorSpec
		wantErr string
	}{
		{
			name: "no signingKey is fine",
			spec: seiv1alpha1.ValidatorSpec{},
		},
		{
			name: "signingKey + nodeKey with distinct secretNames is valid",
			spec: seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "validator-0-key"},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "validator-0-nodekey"},
				},
			},
		},
		{
			name: "signingKey + nodeKey + bootstrap is valid (migration use case)",
			spec: seiv1alpha1.ValidatorSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					BootstrapImage: "ghcr.io/sei/bootstrap:v1",
					S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 12345678},
				},
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "validator-0-key"},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "validator-0-nodekey"},
				},
			},
		},
		{
			name: "signingKey without nodeKey is rejected",
			spec: seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "validator-0-key"},
				},
			},
			wantErr: "nodeKey is required when signingKey is set",
		},
		{
			name: "nodeKey without signingKey is rejected",
			spec: seiv1alpha1.ValidatorSpec{
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "validator-0-nodekey"},
				},
			},
			wantErr: "nodeKey requires signingKey to be set",
		},
		{
			name: "signingKey and nodeKey sharing a secretName is rejected",
			spec: seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "shared-keys"},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "shared-keys"},
				},
			},
			wantErr: "must reference distinct Secrets",
		},
		{
			name: "signingKey with empty secretName is rejected",
			spec: seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: ""},
				},
			},
			wantErr: "signingKey.secret.secretName is required",
		},
		{
			name: "signingKey with nil secret variant is rejected",
			spec: seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{},
			},
			wantErr: "signingKey.secret.secretName is required",
		},
		{
			name: "signingKey + genesisCeremony is rejected",
			spec: seiv1alpha1.ValidatorSpec{
				GenesisCeremony: &seiv1alpha1.GenesisCeremonyNodeConfig{
					ChainID:        "pacific-1",
					StakingAmount:  "1000000usei",
					AccountBalance: "1000000usei",
				},
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "validator-0-key"},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "validator-0-nodekey"},
				},
			},
			wantErr: "signingKey is mutually exclusive with genesisCeremony",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "validator-0", Namespace: "pacific-1"},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID:   "pacific-1",
					Image:     "seid:v6.4.1",
					Validator: &tc.spec,
				},
			}
			err := (&validatorPlanner{}).Validate(node)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate: error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// taskTypes returns the ordered list of task types in a plan, for assertions.
func taskTypes(plan *seiv1alpha1.TaskPlan) []string {
	out := make([]string, len(plan.Tasks))
	for i, t := range plan.Tasks {
		out[i] = t.Type
	}
	return out
}

// indexOfTaskType returns the position of taskType in plan, or -1.
func indexOfTaskType(plan *seiv1alpha1.TaskPlan, taskType string) int {
	return slices.Index(taskTypes(plan), taskType)
}

func TestValidatorPlanner_BuildPlan_IdentityInsertsValidateTasks_Bootstrap(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "seid:v6.4.1",
			Validator: &seiv1alpha1.ValidatorSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					BootstrapImage: "ghcr.io/sei/bootstrap:v1",
					S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 12345678},
				},
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "validator-0-key"},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "validator-0-nodekey"},
				},
			},
		},
	}
	plan, err := (&validatorPlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	pvcIdx := indexOfTaskType(plan, task.TaskTypeEnsureDataPVC)
	signingIdx := indexOfTaskType(plan, task.TaskTypeValidateSigningKey)
	nodeKeyIdx := indexOfTaskType(plan, task.TaskTypeValidateNodeKey)
	deployJobIdx := indexOfTaskType(plan, task.TaskTypeDeployBootstrapJob)

	if signingIdx < 0 {
		t.Fatalf("plan must contain %s; got %v", task.TaskTypeValidateSigningKey, taskTypes(plan))
	}
	if nodeKeyIdx < 0 {
		t.Fatalf("plan must contain %s; got %v", task.TaskTypeValidateNodeKey, taskTypes(plan))
	}
	if pvcIdx >= signingIdx || signingIdx >= nodeKeyIdx || nodeKeyIdx >= deployJobIdx {
		t.Fatalf("expected ordering pvc(%d) < signing(%d) < nodeKey(%d) < deploy-bootstrap-job(%d); got %v",
			pvcIdx, signingIdx, nodeKeyIdx, deployJobIdx, taskTypes(plan))
	}

	// Verify params are populated correctly.
	for _, pt := range plan.Tasks {
		switch pt.Type {
		case task.TaskTypeValidateSigningKey:
			if pt.Params == nil || !strings.Contains(string(pt.Params.Raw), "validator-0-key") {
				t.Fatalf("validate-signing-key params must reference secret name; got %q", string(pt.Params.Raw))
			}
		case task.TaskTypeValidateNodeKey:
			if pt.Params == nil || !strings.Contains(string(pt.Params.Raw), "validator-0-nodekey") {
				t.Fatalf("validate-node-key params must reference secret name; got %q", string(pt.Params.Raw))
			}
		}
	}
}

func TestValidatorPlanner_BuildPlan_IdentityInsertsValidateTasks_Base(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "seid:v6.4.1",
			Validator: &seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "validator-0-key"},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "validator-0-nodekey"},
				},
			},
		},
	}
	plan, err := (&validatorPlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	pvcIdx := indexOfTaskType(plan, task.TaskTypeEnsureDataPVC)
	signingIdx := indexOfTaskType(plan, task.TaskTypeValidateSigningKey)
	nodeKeyIdx := indexOfTaskType(plan, task.TaskTypeValidateNodeKey)
	stsIdx := indexOfTaskType(plan, task.TaskTypeApplyStatefulSet)

	if signingIdx < 0 || nodeKeyIdx < 0 {
		t.Fatalf("plan must contain both validate-signing-key and validate-node-key; got %v", taskTypes(plan))
	}
	if pvcIdx >= signingIdx || signingIdx >= nodeKeyIdx || nodeKeyIdx >= stsIdx {
		t.Fatalf("expected ordering pvc(%d) < signing(%d) < nodeKey(%d) < apply-statefulset(%d); got %v",
			pvcIdx, signingIdx, nodeKeyIdx, stsIdx, taskTypes(plan))
	}
}

func TestValidatorPlanner_BuildPlan_NoSigningKeyOmitsValidateTask(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   "pacific-1",
			Image:     "seid:v6.4.1",
			Validator: &seiv1alpha1.ValidatorSpec{},
		},
	}
	plan, err := (&validatorPlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if idx := indexOfTaskType(plan, task.TaskTypeValidateSigningKey); idx >= 0 {
		t.Fatalf("plan must not contain %s when SigningKey is unset; got %v at index %d",
			task.TaskTypeValidateSigningKey, taskTypes(plan), idx)
	}
}
