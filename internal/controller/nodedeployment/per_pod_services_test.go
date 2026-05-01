package nodedeployment

import (
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func nodeWithOrdinal(name, ordinal string, extraLabels map[string]string) seiv1alpha1.SeiNode {
	labels := map[string]string{
		groupLabel:        "pacific-1-wave",
		groupOrdinalLabel: ordinal,
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "pacific-1",
			Labels:    labels,
		},
	}
}

func TestPopulatePerPodServices_EmptyChildren(t *testing.T) {
	g := NewWithT(t)
	got := populatePerPodServices(logr.Discard(), nil)
	g.Expect(got).To(BeNil())
}

func TestPopulatePerPodServices_AllHealthySorted(t *testing.T) {
	g := NewWithT(t)
	// Out-of-order on purpose; helper must sort by ordinal.
	nodes := []seiv1alpha1.SeiNode{
		nodeWithOrdinal("pacific-1-wave-2", "2", nil),
		nodeWithOrdinal("pacific-1-wave-0", "0", nil),
		nodeWithOrdinal("pacific-1-wave-1", "1", nil),
	}

	got := populatePerPodServices(logr.Discard(), nodes)

	g.Expect(got).To(HaveLen(3))
	g.Expect(got[0].Name).To(Equal("pacific-1-wave-0"))
	g.Expect(got[1].Name).To(Equal("pacific-1-wave-1"))
	g.Expect(got[2].Name).To(Equal("pacific-1-wave-2"))
	for _, e := range got {
		g.Expect(e.Namespace).To(Equal("pacific-1"))
		g.Expect(e.Ports.EvmHttp).To(Equal(seiconfig.PortEVMHTTP))
		g.Expect(e.Ports.EvmWs).To(Equal(seiconfig.PortEVMWS))
	}
}

func TestPopulatePerPodServices_OrdinalGapAfterScaleDown(t *testing.T) {
	g := NewWithT(t)
	// Post-scale-down: ordinals 0, 2, 5 — non-contiguous but each entry
	// must still appear in sorted order.
	nodes := []seiv1alpha1.SeiNode{
		nodeWithOrdinal("pacific-1-wave-5", "5", nil),
		nodeWithOrdinal("pacific-1-wave-0", "0", nil),
		nodeWithOrdinal("pacific-1-wave-2", "2", nil),
	}

	got := populatePerPodServices(logr.Discard(), nodes)

	g.Expect(got).To(HaveLen(3))
	g.Expect(got[0].Name).To(Equal("pacific-1-wave-0"))
	g.Expect(got[1].Name).To(Equal("pacific-1-wave-2"))
	g.Expect(got[2].Name).To(Equal("pacific-1-wave-5"))
}

func TestPopulatePerPodServices_ExporterExcluded(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		nodeWithOrdinal("pacific-1-wave-0", "0", nil),
		nodeWithOrdinal("pacific-1-wave-1", "1", map[string]string{"sei.io/role": "exporter"}),
		nodeWithOrdinal("pacific-1-wave-2", "2", nil),
	}

	got := populatePerPodServices(logr.Discard(), nodes)

	g.Expect(got).To(HaveLen(2))
	g.Expect(got[0].Name).To(Equal("pacific-1-wave-0"))
	g.Expect(got[1].Name).To(Equal("pacific-1-wave-2"))
}

func TestPopulatePerPodServices_MissingOrdinalLabelSkipped(t *testing.T) {
	g := NewWithT(t)
	noOrdinal := seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pacific-1-wave-99",
			Namespace: "pacific-1",
			Labels:    map[string]string{groupLabel: "pacific-1-wave"},
		},
	}
	nodes := []seiv1alpha1.SeiNode{
		nodeWithOrdinal("pacific-1-wave-0", "0", nil),
		noOrdinal,
	}

	got := populatePerPodServices(logr.Discard(), nodes)

	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].Name).To(Equal("pacific-1-wave-0"))
}

func TestPopulatePerPodServices_UnparseableOrdinalSkipped(t *testing.T) {
	g := NewWithT(t)
	bad := nodeWithOrdinal("pacific-1-wave-junk", "not-a-number", nil)
	nodes := []seiv1alpha1.SeiNode{
		nodeWithOrdinal("pacific-1-wave-0", "0", nil),
		bad,
	}

	got := populatePerPodServices(logr.Discard(), nodes)

	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].Name).To(Equal("pacific-1-wave-0"))
}
