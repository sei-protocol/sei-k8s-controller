package planner

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func TestMergeOverrides_ForcesLoggingLevelInfo(t *testing.T) {
	cases := []struct {
		name string
		user map[string]string
	}{
		{"user_debug", map[string]string{overrideKeyLoggingLevel: "debug"}},
		{"user_warn", map[string]string{overrideKeyLoggingLevel: "warn"}},
		{"user_info", map[string]string{overrideKeyLoggingLevel: "info"}},
		{"user_empty", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeOverrides(nil, tc.user)
			if got := merged[overrideKeyLoggingLevel]; got != enforcedLoggingLevel {
				t.Fatalf("logging.level = %q, want %q", got, enforcedLoggingLevel)
			}
		})
	}
}

func TestMergeOverrides_PreservesNonForcedUserKeys(t *testing.T) {
	merged := mergeOverrides(
		map[string]string{"network.p2p.external_address": "ctrl.address:26656"},
		map[string]string{
			"network.p2p.external_address": "user.address:26656",
			"storage.snapshot_interval":    "1000",
			overrideKeyLoggingLevel:        "debug",
		},
	)
	if got := merged["network.p2p.external_address"]; got != "user.address:26656" {
		t.Errorf("user override should win for non-forced key: got %q", got)
	}
	if got := merged["storage.snapshot_interval"]; got != "1000" {
		t.Errorf("user key should pass through: got %q", got)
	}
	if got := merged[overrideKeyLoggingLevel]; got != enforcedLoggingLevel {
		t.Errorf("forced key should override user: got %q, want %q", got, enforcedLoggingLevel)
	}
}

func TestMergeOverrides_NestedCallsConvergeToInfo(t *testing.T) {
	inner := mergeOverrides(
		map[string]string{"a": "1"},
		map[string]string{"b": "2"},
	)
	outer := mergeOverrides(inner, map[string]string{overrideKeyLoggingLevel: "debug"})
	if got := outer[overrideKeyLoggingLevel]; got != enforcedLoggingLevel {
		t.Fatalf("nested merge with user debug: logging.level = %q, want %q", got, enforcedLoggingLevel)
	}
}

func TestFullNodePlanner_ConfigApplyForcesLoggingLevel(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "fullnode-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "pacific-1",
			Image:    "seid:v6.4.1",
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Overrides: map[string]string{
				overrideKeyLoggingLevel: "debug",
			},
		},
	}

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var configApply *seiv1alpha1.PlannedTask
	for i := range plan.Tasks {
		if plan.Tasks[i].Type == TaskConfigApply {
			configApply = &plan.Tasks[i]
			break
		}
	}
	if configApply == nil {
		t.Fatal("no config-apply task in plan")
	}
	if configApply.Params == nil {
		t.Fatal("config-apply task has no params")
	}

	var params task.ConfigApplyParams
	if err := json.Unmarshal(configApply.Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal config-apply params: %v", err)
	}
	if got := params.Overrides[overrideKeyLoggingLevel]; got != enforcedLoggingLevel {
		t.Errorf("config-apply Overrides[%s] = %q, want %q (user-supplied debug should be rejected)",
			overrideKeyLoggingLevel, got, enforcedLoggingLevel)
	}
}
