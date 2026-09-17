package noderesource

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const (
	testConfigMapName    = "rpc-config-v1"
	testAppConfigMapName = "rpc-app-v1"
)

func nodeConfigNode() *seiv1alpha1.SeiNode {
	node := newSnapshotNode("snap-0", "default")
	node.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
		ConfigRef: seiv1alpha1.ConfigFileRef{Name: testConfigMapName},
		AppRef:    seiv1alpha1.ConfigFileRef{Name: testAppConfigMapName},
	}
	return node
}

// containerByName searches both container lists: the sidecar and seid-init
// are init containers, and seid is not.
func containerByName(spec corev1.PodSpec, name string) *corev1.Container {
	if c := findContainer(spec.Containers, name); c != nil {
		return c
	}
	return findContainer(spec.InitContainers, name)
}

func mountNames(c *corev1.Container) []string {
	names := make([]string, 0, len(c.VolumeMounts))
	for _, m := range c.VolumeMounts {
		names = append(names, m.Name)
	}
	return names
}

func TestNodeConfigRendersOneVolumePerFile(t *testing.T) {
	g := NewWithT(t)

	spec, err := buildNodePodSpec(nodeConfigNode(), platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	cases := []struct {
		volumeName    string
		configMapName string
		dataKey       string
	}{
		{nodeConfigConfigVolumeName, testConfigMapName, configTomlDataKey},
		{nodeConfigAppVolumeName, testAppConfigMapName, appTomlDataKey},
	}
	for _, tc := range cases {
		v := findVolume(spec.Volumes, tc.volumeName)
		g.Expect(v).NotTo(BeNil(), "volume %s", tc.volumeName)
		g.Expect(v.ConfigMap).NotTo(BeNil())
		g.Expect(v.ConfigMap.Name).To(Equal(tc.configMapName))
		g.Expect(*v.ConfigMap.DefaultMode).To(Equal(int32(0o444)))
		g.Expect(v.ConfigMap.Items).To(Equal(
			[]corev1.KeyToPath{{Key: tc.dataKey, Path: tc.dataKey}}))
	}

	want := []corev1.VolumeMount{
		{
			Name:      nodeConfigConfigVolumeName,
			MountPath: dataDir + "/config/config.toml",
			SubPath:   configTomlDataKey,
			ReadOnly:  true,
		},
		{
			Name:      nodeConfigAppVolumeName,
			MountPath: dataDir + "/config/app.toml",
			SubPath:   appTomlDataKey,
			ReadOnly:  true,
		},
	}
	seid := containerByName(spec, containerNameSeid)
	g.Expect(seid).NotTo(BeNil())
	g.Expect(seid.VolumeMounts).To(ContainElements(want))
}

// TestNodeConfigMountsOnSidecarIsASafetyProperty guards a cleanup that reads
// as harmless. The sidecar does not read config.toml, so a future change could
// drop this mount — and that is exactly what must not happen. A rename onto a
// mounted path from a container WITHOUT the mount succeeds and silently
// detaches it; from a container WITH the mount the same rename returns EBUSY
// and fails the task. This mount is what makes a stray writer loud.
func TestNodeConfigMountsOnSidecarIsASafetyProperty(t *testing.T) {
	g := NewWithT(t)

	spec, err := buildNodePodSpec(nodeConfigNode(), platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	sidecar := containerByName(spec, containerNameSidecar)
	g.Expect(sidecar).NotTo(BeNil())
	g.Expect(mountNames(sidecar)).To(ContainElements(nodeConfigConfigVolumeName, nodeConfigAppVolumeName))
}

// TestNodeConfigNotMountedOnWritingContainers keeps the mount off every
// container that must write the config directory. seid-init runs
// `seid init --overwrite` on a fresh volume; the other two never touch it.
func TestNodeConfigNotMountedOnWritingContainers(t *testing.T) {
	g := NewWithT(t)

	spec, err := buildNodePodSpec(nodeConfigNode(), platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range []string{"seid-init", containerNameRBACProxy, containerNameCosmosExporter} {
		c := containerByName(spec, name)
		if c == nil {
			continue
		}
		g.Expect(mountNames(c)).NotTo(ContainElements(nodeConfigConfigVolumeName, nodeConfigAppVolumeName), "container %s", name)
	}
}

func TestNodeConfigUnsetRendersNothing(t *testing.T) {
	g := NewWithT(t)

	spec, err := buildNodePodSpec(newSnapshotNode("snap-0", "default"), platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	rendered := []string{nodeConfigConfigVolumeName, nodeConfigAppVolumeName}
	for _, v := range spec.Volumes {
		g.Expect(rendered).NotTo(ContainElement(v.Name))
	}
	for i := range spec.Containers {
		g.Expect(mountNames(&spec.Containers[i])).NotTo(ContainElements(rendered))
	}
	for i := range spec.InitContainers {
		g.Expect(mountNames(&spec.InitContainers[i])).NotTo(ContainElements(rendered))
	}
}
