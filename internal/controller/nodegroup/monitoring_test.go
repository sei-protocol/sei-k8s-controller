package nodegroup

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
