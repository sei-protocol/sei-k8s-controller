package seinetwork

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func nodeAtHeight(h int64, readAt time.Time) seiv1alpha1.SeiNode {
	t := metav1.NewTime(readAt)
	return seiv1alpha1.SeiNode{Status: seiv1alpha1.SeiNodeStatus{
		Phase:               seiv1alpha1.PhaseRunning,
		CommittedHeight:     &h,
		CommittedHeightTime: &t,
	}}
}

func producing(network *seiv1alpha1.SeiNetwork) *metav1.Condition {
	return apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionProducing)
}

func TestProducing_NoNodes(t *testing.T) {
	g := NewWithT(t)
	network := &seiv1alpha1.SeiNetwork{}
	setProducingCondition(network, nil, time.Now())
	c := producing(network)
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal(seiv1alpha1.ReasonNoNodes))
	g.Expect(network.Status.ObservedHeight).To(BeNil())
}

func TestProducing_AdvancesAndRecordsObservedHeight(t *testing.T) {
	g := NewWithT(t)
	now := time.Now()
	network := &seiv1alpha1.SeiNetwork{}

	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(10, now), nodeAtHeight(12, now)}, now)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightAdvancing))
	g.Expect(network.Status.ObservedHeight.Height).To(Equal(int64(12)))
	g.Expect(network.Status.ObservedHeight.Time.Time).To(BeTemporally("==", now))

	later := now.Add(30 * time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(11, later), nodeAtHeight(40, later)}, later)
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightAdvancing))
	g.Expect(network.Status.ObservedHeight.Height).To(Equal(int64(40)))
	g.Expect(network.Status.ObservedHeight.Time.Time).To(BeTemporally("==", later))
}

func TestProducing_StaleReadingsExcluded(t *testing.T) {
	g := NewWithT(t)
	now := time.Now()
	network := &seiv1alpha1.SeiNetwork{}
	nodes := []seiv1alpha1.SeiNode{
		nodeAtHeight(500, now.Add(-heightReadingMaxAge-time.Second)), // stale: ignored
		nodeAtHeight(7, now),
	}
	setProducingCondition(network, nodes, now)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(network.Status.ObservedHeight.Height).To(Equal(int64(7)))
}

func TestProducing_AllStaleOrUnread_SignalUnreadable(t *testing.T) {
	g := NewWithT(t)
	now := time.Now()
	network := &seiv1alpha1.SeiNetwork{Status: seiv1alpha1.SeiNetworkStatus{
		ObservedHeight: &seiv1alpha1.ObservedHeight{Height: 99, Time: metav1.NewTime(now.Add(-time.Hour))},
	}}
	nodes := []seiv1alpha1.SeiNode{
		nodeAtHeight(100, now.Add(-2*heightReadingMaxAge)),
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}}, // never read
	}
	setProducingCondition(network, nodes, now)
	c := producing(network)
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal(seiv1alpha1.ReasonSignalUnreadable))
	g.Expect(c.Message).To(ContainSubstring("1 stale, 1 never read"))
	g.Expect(network.Status.ObservedHeight.Height).To(Equal(int64(99)), "high-water mark kept")
}

func TestProducing_HeightZero_GraceThenStall(t *testing.T) {
	g := NewWithT(t)
	start := time.Now()
	network := &seiv1alpha1.SeiNetwork{}

	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(0, start)}, start)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonAwaitingFirstBlock))
	g.Expect(network.Status.ObservedHeight.Height).To(Equal(int64(0)))

	inGrace := start.Add(producingWindow - time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(0, inGrace)}, inGrace)
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonAwaitingFirstBlock))

	pastGrace := start.Add(producingWindow + time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(0, pastGrace)}, pastGrace)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightStalled))
}

func TestProducing_UnchangedInsideWindowStaysTrue(t *testing.T) {
	g := NewWithT(t)
	start := time.Now()
	network := &seiv1alpha1.SeiNetwork{}
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, start)}, start)

	later := start.Add(30 * time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, later)}, later)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightAdvancing))
	g.Expect(network.Status.ObservedHeight.Time.Time).To(BeTemporally("==", start), "time marks the last advance")
}

func TestProducing_TendermintStall(t *testing.T) {
	g := NewWithT(t)
	start := time.Now()
	network := &seiv1alpha1.SeiNetwork{}
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, start)}, start)

	later := start.Add(producingWindow + time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, later)}, later)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightStalled))
}

func TestProducing_AutobahnIdleIsNotAStall(t *testing.T) {
	g := NewWithT(t)
	start := time.Now()
	network := &seiv1alpha1.SeiNetwork{Spec: seiv1alpha1.SeiNetworkSpec{
		Consensus: &seiv1alpha1.NetworkConsensusSpec{
			ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
		},
	}}
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, start)}, start)

	later := start.Add(producingWindow + time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, later)}, later)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonIdle))

	// Load arrives: back to producing.
	resumed := later.Add(30 * time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(51, resumed)}, resumed)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightAdvancing))
}

func TestProducing_AutobahnWithEmptyBlocksStalls(t *testing.T) {
	g := NewWithT(t)
	start := time.Now()
	allow := true
	network := &seiv1alpha1.SeiNetwork{Spec: seiv1alpha1.SeiNetworkSpec{
		Consensus: &seiv1alpha1.NetworkConsensusSpec{
			ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
			Autobahn:      &seiv1alpha1.AutobahnCeremonySpec{AllowEmptyBlocks: &allow},
		},
	}}
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, start)}, start)

	later := start.Add(producingWindow + time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(50, later)}, later)
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightStalled),
		"an engine that commits empty blocks has no idle state")
}

func TestProducing_NeverRead_AwaitingFirstBlock(t *testing.T) {
	g := NewWithT(t)
	network := &seiv1alpha1.SeiNetwork{}
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}},
	}
	setProducingCondition(network, nodes, time.Now())
	c := producing(network)
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal(seiv1alpha1.ReasonAwaitingFirstBlock), "bootstrap is not a fault")
	g.Expect(network.Status.ObservedHeight).To(BeNil())
}

func TestProducing_LowerHeightResetsHighWaterMark(t *testing.T) {
	g := NewWithT(t)
	start := time.Now()
	network := &seiv1alpha1.SeiNetwork{Status: seiv1alpha1.SeiNetworkStatus{
		ObservedHeight: &seiv1alpha1.ObservedHeight{Height: 9000, Time: metav1.NewTime(start.Add(-time.Hour))},
	}}
	// Every reporting child, including the one whose reading is stale, sits
	// below the mark: the chain was rebuilt from genesis.
	nodes := []seiv1alpha1.SeiNode{
		nodeAtHeight(3, start),
		nodeAtHeight(2, start.Add(-2*heightReadingMaxAge)),
	}
	setProducingCondition(network, nodes, start)
	g.Expect(network.Status.ObservedHeight.Height).To(Equal(int64(3)))
	g.Expect(network.Status.ObservedHeight.Time.Time).To(BeTemporally("==", start))
	g.Expect(producing(network).Status).To(Equal(metav1.ConditionTrue), "fresh mark is inside the window")
}

func TestProducing_LeaderAgingOutDoesNotResetMark(t *testing.T) {
	g := NewWithT(t)
	start := time.Now()
	network := &seiv1alpha1.SeiNetwork{Spec: seiv1alpha1.SeiNetworkSpec{}}

	// Two children; the leader at 100, a lagger at 99. The chain halts.
	leaderRead := start
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(100, leaderRead), nodeAtHeight(99, start)}, start)
	g.Expect(producing(network).Reason).To(Equal(seiv1alpha1.ReasonHeightAdvancing))

	// The leader's reading ages out while the lagger keeps reporting 99: the
	// fresh maximum drops, but 100 is still on record, so the mark holds.
	later := start.Add(heightReadingMaxAge + time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(100, leaderRead), nodeAtHeight(99, later)}, later)
	g.Expect(network.Status.ObservedHeight.Height).To(Equal(int64(100)))
	g.Expect(network.Status.ObservedHeight.Time.Time).To(BeTemporally("==", start))

	// The leader comes back at 100 past the window: not an advance.
	past := start.Add(producingWindow + time.Second)
	setProducingCondition(network, []seiv1alpha1.SeiNode{nodeAtHeight(100, past), nodeAtHeight(99, past)}, past)
	c := producing(network)
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal(seiv1alpha1.ReasonHeightStalled))
	g.Expect(c.Message).To(ContainSubstring("observed height 100 has not advanced"))
}
