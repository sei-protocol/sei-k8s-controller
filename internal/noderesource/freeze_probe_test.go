package noderesource

import (
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Covers spec freeze-node: FN-8 and FN-9.
//
// A frozen node holds its height while peers advance, so its lag grows without
// bound and /lag_status fails once the lag threshold trips. The pod would then
// leave its Service endpoints and the RPC would answer nothing, even though
// seid is serving correctly at the frozen height.

func TestReadinessProbe_FrozenNode_TargetsRPCListener(t *testing.T) {
	tests := []struct {
		name string
		spec seiv1alpha1.SeiNodeSpec
	}{
		{
			name: "full node",
			spec: seiv1alpha1.SeiNodeSpec{
				FullNode: &seiv1alpha1.FullNodeSpec{
					Freeze: &seiv1alpha1.FreezeSpec{Height: 100},
				},
			},
		},
		{
			name: "archive",
			spec: seiv1alpha1.SeiNodeSpec{
				Archive: &seiv1alpha1.ArchiveSpec{
					Freeze: &seiv1alpha1.FreezeSpec{Height: 100},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			probe := readinessProbeForNode(&seiv1alpha1.SeiNode{Spec: tt.spec})

			g.Expect(probe).NotTo(BeNil())
			g.Expect(probe.HTTPGet).To(BeNil(),
				"a frozen node must not be gated on /lag_status: its lag grows without bound")
			g.Expect(probe.TCPSocket).NotTo(BeNil())
			g.Expect(probe.TCPSocket.Port.IntVal).To(Equal(seiconfig.PortRPC),
				"readiness must track the listener a caller actually uses")
		})
	}
}

// FN-9: the change is bounded. An unfrozen RPC-serving node keeps the lag
// probe, which is a stronger signal than an open socket.
func TestReadinessProbe_UnfrozenNode_KeepsLagStatus(t *testing.T) {
	tests := []struct {
		name string
		spec seiv1alpha1.SeiNodeSpec
	}{
		{name: "full node", spec: seiv1alpha1.SeiNodeSpec{FullNode: &seiv1alpha1.FullNodeSpec{}}},
		{name: "archive", spec: seiv1alpha1.SeiNodeSpec{Archive: &seiv1alpha1.ArchiveSpec{}}},
		{name: "validator", spec: seiv1alpha1.SeiNodeSpec{Validator: &seiv1alpha1.ValidatorSpec{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			probe := readinessProbeForNode(&seiv1alpha1.SeiNode{Spec: tt.spec})

			g.Expect(probe.TCPSocket).To(BeNil())
			g.Expect(probe.HTTPGet).NotTo(BeNil())
			g.Expect(probe.HTTPGet.Path).To(Equal("/lag_status"))
			g.Expect(probe.HTTPGet.Port.IntVal).To(Equal(seiconfig.PortRPC))
		})
	}
}

// A seed serves no query RPC, so freeze never applies and its probe is
// unchanged. This case exists to keep the ordering in readinessProbeForNode
// honest: the seed branch must win before the freeze branch is consulted.
func TestReadinessProbe_Seed_TargetsP2PListener(t *testing.T) {
	g := NewWithT(t)
	probe := readinessProbeForNode(&seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{Seed: &seiv1alpha1.SeedSpec{}},
	})

	g.Expect(probe.TCPSocket).NotTo(BeNil())
	g.Expect(probe.TCPSocket.Port.IntVal).To(Equal(seiconfig.PortP2P))
}

// The frozen probe must not react faster or slower than the probe it replaces,
// so a mode switch does not change how quickly the kubelet pulls a pod out of
// service.
func TestReadinessProbe_FrozenAndUnfrozen_ShareTimings(t *testing.T) {
	g := NewWithT(t)

	frozen := readinessProbeForNode(&seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{FullNode: &seiv1alpha1.FullNodeSpec{
			Freeze: &seiv1alpha1.FreezeSpec{Height: 100},
		}},
	})
	unfrozen := readinessProbeForNode(&seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{FullNode: &seiv1alpha1.FullNodeSpec{}},
	})

	g.Expect(frozen.InitialDelaySeconds).To(Equal(unfrozen.InitialDelaySeconds))
	g.Expect(frozen.PeriodSeconds).To(Equal(unfrozen.PeriodSeconds))
	g.Expect(frozen.FailureThreshold).To(Equal(unfrozen.FailureThreshold))
	g.Expect(frozen.TimeoutSeconds).To(Equal(unfrozen.TimeoutSeconds))
}

// FN-4 is structural: only fullNode and archive carry the field, so the shared
// accessor reports nil for every other mode. A validator that could freeze
// would fail at boot, because sei-tendermint refuses the combination.
func TestSpecFreeze_NilForNonRPCModes(t *testing.T) {
	g := NewWithT(t)

	for name, spec := range map[string]seiv1alpha1.SeiNodeSpec{
		"validator": {Validator: &seiv1alpha1.ValidatorSpec{}},
		"seed":      {Seed: &seiv1alpha1.SeedSpec{}},
		"replayer":  {Replayer: &seiv1alpha1.ReplayerSpec{}},
	} {
		g.Expect(spec.Freeze()).To(BeNil(), "%s must not report a freeze", name)
	}
}
