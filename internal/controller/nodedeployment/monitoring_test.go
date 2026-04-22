package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestGenerateServiceMonitor_BasicFields(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Monitoring = &seiv1alpha1.MonitoringConfig{
		ServiceMonitor: &seiv1alpha1.ServiceMonitorConfig{
			Interval: "15s",
		},
	}

	sm := generateServiceMonitor(group)

	g.Expect(sm.GetName()).To(Equal("archive-rpc"))
	g.Expect(sm.GetNamespace()).To(Equal("sei"))

	spec := sm.Object["spec"].(map[string]any)
	selector := spec["selector"].(map[string]any)
	matchLabels := selector["matchLabels"].(map[string]any)
	g.Expect(matchLabels[groupLabel]).To(Equal("archive-rpc"))

	endpoints := spec["endpoints"].([]any)
	g.Expect(endpoints).To(HaveLen(1))

	ep := endpoints[0].(map[string]any)
	g.Expect(ep["port"]).To(Equal("metrics"))
	g.Expect(ep["interval"]).To(Equal("15s"))
}

func TestGenerateServiceMonitor_DefaultInterval(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Monitoring = &seiv1alpha1.MonitoringConfig{
		ServiceMonitor: &seiv1alpha1.ServiceMonitorConfig{},
	}

	sm := generateServiceMonitor(group)

	spec := sm.Object["spec"].(map[string]any)
	endpoints := spec["endpoints"].([]any)
	ep := endpoints[0].(map[string]any)
	g.Expect(ep["port"]).To(Equal("metrics"))
	g.Expect(ep["interval"]).To(Equal("30s"))
}

func TestGenerateServiceMonitor_CustomLabels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Monitoring = &seiv1alpha1.MonitoringConfig{
		ServiceMonitor: &seiv1alpha1.ServiceMonitorConfig{
			Labels: map[string]string{
				"release": "prometheus",
			},
		},
	}

	sm := generateServiceMonitor(group)

	metadata := sm.Object["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	g.Expect(labels["release"]).To(Equal("prometheus"))
	g.Expect(labels[groupLabel]).To(Equal("archive-rpc"))
}

func TestGenerateServiceMonitor_ComponentRelabeling(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*seiv1alpha1.SeiNodeSpec)
		expected string
	}{
		{"fullNode", func(s *seiv1alpha1.SeiNodeSpec) {}, "node"},
		{"validator", func(s *seiv1alpha1.SeiNodeSpec) {
			s.FullNode = nil
			s.Validator = &seiv1alpha1.ValidatorSpec{}
		}, "validator"},
		{"archive", func(s *seiv1alpha1.SeiNodeSpec) {
			s.FullNode = nil
			s.Archive = &seiv1alpha1.ArchiveSpec{}
		}, "archive"},
		{"replayer", func(s *seiv1alpha1.SeiNodeSpec) {
			s.FullNode = nil
			s.Replayer = &seiv1alpha1.ReplayerSpec{}
		}, "replayer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			group := newTestGroup("role-test", "sei")
			group.Spec.Monitoring = &seiv1alpha1.MonitoringConfig{
				ServiceMonitor: &seiv1alpha1.ServiceMonitorConfig{},
			}
			tc.mutate(&group.Spec.Template.Spec)

			sm := generateServiceMonitor(group)
			spec := sm.Object["spec"].(map[string]any)
			ep := spec["endpoints"].([]any)[0].(map[string]any)
			relabelings := ep["metricRelabelings"].([]any)
			g.Expect(findRelabeling(relabelings, "component")).To(Equal(tc.expected))
		})
	}
}

func TestGenerateServiceMonitor_ChainIDRelabeling(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("role-test", "sei")
	group.Spec.Monitoring = &seiv1alpha1.MonitoringConfig{
		ServiceMonitor: &seiv1alpha1.ServiceMonitorConfig{},
	}

	sm := generateServiceMonitor(group)
	spec := sm.Object["spec"].(map[string]any)
	ep := spec["endpoints"].([]any)[0].(map[string]any)
	relabelings := ep["metricRelabelings"].([]any)
	g.Expect(findRelabeling(relabelings, "chain_id")).To(Equal("pacific-1"))
}

func TestGenerateServiceMonitor_NoRelabelingsWhenNothingDerivable(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("role-test", "sei")
	group.Spec.Monitoring = &seiv1alpha1.MonitoringConfig{
		ServiceMonitor: &seiv1alpha1.ServiceMonitorConfig{},
	}
	group.Spec.Template.Spec.FullNode = nil
	group.Spec.Template.Spec.ChainID = ""

	sm := generateServiceMonitor(group)
	spec := sm.Object["spec"].(map[string]any)
	ep := spec["endpoints"].([]any)[0].(map[string]any)
	_, has := ep["metricRelabelings"]
	g.Expect(has).To(BeFalse())
}

// findRelabeling returns the `replacement` string for a metricRelabeling
// targeting the given label, or "" if no such rule exists.
func findRelabeling(relabelings []any, targetLabel string) string {
	for _, r := range relabelings {
		rule := r.(map[string]any)
		if rule["targetLabel"] == targetLabel {
			return rule["replacement"].(string)
		}
	}
	return ""
}
