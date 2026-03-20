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

	spec := sm.Object["spec"].(map[string]interface{})
	selector := spec["selector"].(map[string]interface{})
	matchLabels := selector["matchLabels"].(map[string]interface{})
	g.Expect(matchLabels[groupLabel]).To(Equal("archive-rpc"))

	endpoints := spec["endpoints"].([]interface{})
	g.Expect(endpoints).To(HaveLen(1))

	ep := endpoints[0].(map[string]interface{})
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

	spec := sm.Object["spec"].(map[string]interface{})
	endpoints := spec["endpoints"].([]interface{})
	ep := endpoints[0].(map[string]interface{})
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

	metadata := sm.Object["metadata"].(map[string]interface{})
	labels := metadata["labels"].(map[string]interface{})
	g.Expect(labels["release"]).To(Equal("prometheus"))
	g.Expect(labels[groupLabel]).To(Equal("archive-rpc"))
}
