package noderesource

import (
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

// 011 Req 1.1: a footprint change on a nodeConfig node needs no controller
// plan. It changes the rendered pod template, and the RollingUpdate
// StatefulSet rolls the pod onto it.
func TestNodeConfigFootprintChangeReachesPodTemplate(t *testing.T) {
	g := NewWithT(t)

	withFootprint := func(cpu, mem string) *seiv1alpha1.SeiNode {
		node := nodeConfigNode()
		node.Spec.Resources = &seiv1alpha1.Resources{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}}
		return node
	}

	before := mustGenerateStatefulSet(t, withFootprint("4", "32Gi"), platformtest.Config())
	after := mustGenerateStatefulSet(t, withFootprint("8", "16Gi"), platformtest.Config())

	g.Expect(after.Spec.UpdateStrategy.Type).To(Equal(appsv1.RollingUpdateStatefulSetStrategyType),
		"the StatefulSet controller, not a plan, rolls a nodeConfig pod")
	g.Expect(equality.Semantic.DeepEqual(before.Spec.Template, after.Spec.Template)).To(BeFalse(),
		"a footprint change must change the pod template, or the roll never happens")

	seid := containerByName(after.Spec.Template.Spec, containerNameSeid)
	g.Expect(seid).NotTo(BeNil())
	g.Expect(seid.Resources.Requests.Cpu().String()).To(Equal("8"))
	g.Expect(seid.Resources.Requests.Memory().String()).To(Equal("16Gi"))
	g.Expect(seid.Resources.Limits.Memory().String()).To(Equal("16Gi"),
		"the memory limit follows the request down as well as up")
}

// 011 Req 3.1, 3.3: only a nodeConfig node with an explicit CRD size has a
// growable size. The per-mode default never counts, so an app-config change
// cannot grow a cell's volumes.
func TestGrowableStorageSize(t *testing.T) {
	sized := func(node *seiv1alpha1.SeiNode, size string) *seiv1alpha1.SeiNode {
		node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{Storage: &seiv1alpha1.DataVolumeStorage{
			Resources: &seiv1alpha1.VolumeClaimResources{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(size),
			}},
		}}
		return node
	}

	t.Run("nodeConfig with a size grows to it", func(t *testing.T) {
		g := NewWithT(t)
		got := GrowableStorageSize(sized(nodeConfigNode(), "4Ti"))
		g.Expect(got).NotTo(BeNil())
		g.Expect(got.String()).To(Equal("4Ti"))
	})
	t.Run("nodeConfig without a size has none", func(t *testing.T) {
		NewWithT(t).Expect(GrowableStorageSize(nodeConfigNode())).To(BeNil())
	})
	t.Run("a size without nodeConfig is create-only", func(t *testing.T) {
		NewWithT(t).Expect(GrowableStorageSize(sized(newSnapshotNode("snap-0", "default"), "4Ti"))).To(BeNil())
	})
}
