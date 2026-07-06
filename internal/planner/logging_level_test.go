package planner

import (
	"encoding/json"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const levelDebug = "debug"

// logging.level defaults to "error" (seeded by commonOverrides) but is
// user-overridable via spec.Overrides — mergeOverrides lets user keys win.

func TestLoggingLevel_DefaultsToError(t *testing.T) {
	node := &seiv1alpha1.SeiNode{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
	merged := mergeOverrides(commonOverrides(node), node.Spec.Overrides)
	if got := merged[overrideKeyLoggingLevel]; got != defaultLoggingLevel {
		t.Errorf("logging.level = %q, want default %q", got, defaultLoggingLevel)
	}
}

func TestLoggingLevel_UserOverrideWins(t *testing.T) {
	for _, lvl := range []string{levelDebug, "info", "warn"} {
		t.Run(lvl, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "n"},
				Spec: seiv1alpha1.SeiNodeSpec{
					Overrides: map[string]string{overrideKeyLoggingLevel: lvl},
				},
			}
			merged := mergeOverrides(commonOverrides(node), node.Spec.Overrides)
			if got := merged[overrideKeyLoggingLevel]; got != lvl {
				t.Errorf("logging.level = %q, want user value %q", got, lvl)
			}
		})
	}
}

func TestMergeOverrides_UserKeysWin(t *testing.T) {
	merged := mergeOverrides(
		map[string]string{
			"network.p2p.external_address": "ctrl.address:26656",
			overrideKeyLoggingLevel:        defaultLoggingLevel,
		},
		map[string]string{
			"network.p2p.external_address": "user.address:26656",
			"storage.snapshot_interval":    "1000",
			overrideKeyLoggingLevel:        levelDebug,
		},
	)
	if got := merged["network.p2p.external_address"]; got != "user.address:26656" {
		t.Errorf("user override should win for controller key: got %q", got)
	}
	if got := merged["storage.snapshot_interval"]; got != "1000" {
		t.Errorf("user-only key should pass through: got %q", got)
	}
	if got := merged[overrideKeyLoggingLevel]; got != levelDebug {
		t.Errorf("user logging.level should win: got %q, want %q", got, levelDebug)
	}
}

// The init-path plan folds commonOverrides + spec.Overrides into the
// config-apply ConfigIntent, so the "error" default reaches the node when
// unset and a user value reaches it when set.
func TestFullNodePlanner_ConfigApplyRespectsLoggingLevel(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"default_error", nil, defaultLoggingLevel},
		{"user_debug", map[string]string{overrideKeyLoggingLevel: levelDebug}, levelDebug},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "fullnode-0", Namespace: sourceChainID},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID:   sourceChainID,
					Image:     testImageV2,
					FullNode:  &seiv1alpha1.FullNodeSpec{},
					Overrides: tc.overrides,
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

			var params seiconfig.ConfigIntent
			if err := json.Unmarshal(configApply.Params.Raw, &params); err != nil {
				t.Fatalf("unmarshal config-apply params: %v", err)
			}
			if got := params.Overrides[overrideKeyLoggingLevel]; got != tc.want {
				t.Errorf("config-apply logging.level = %q, want %q", got, tc.want)
			}
		})
	}
}
