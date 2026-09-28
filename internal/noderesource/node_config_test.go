package noderesource

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// TestNodeConfigRollsViaStatefulSet pins who rolls the pod. A node with
// nodeConfig runs no config task, so the StatefulSet controller replaces its
// pod on any template change. Parallel keeps a pod that is not Ready (seid
// halted at an upgrade height) from blocking the roll.
func TestNodeConfigRollsViaStatefulSet(t *testing.T) {
	g := NewWithT(t)

	sts := mustGenerateStatefulSet(t, nodeConfigNode(), platformtest.Config())
	g.Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.RollingUpdateStatefulSetStrategyType))
	g.Expect(sts.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))

	plain := mustGenerateStatefulSet(t, newSnapshotNode("snap-0", "default"), platformtest.Config())
	g.Expect(plain.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))
	g.Expect(plain.Spec.PodManagementPolicy).To(BeEmpty(), "the API default applies")
}

func nodeConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       data,
	}
}

func TestCheckNodeConfig(t *testing.T) {
	const validTOML = "moniker = \"node-1\"\n"
	both := map[string]string{ConfigTomlKey: validTOML, AppTomlKey: validTOML}

	cases := []struct {
		name    string
		objs    []client.Object
		wantErr string
	}{
		{"both keys valid", []client.Object{
			nodeConfigMap(testConfigMapName, both), nodeConfigMap(testAppConfigMapName, both)}, ""},
		{"config configmap absent", []client.Object{nodeConfigMap(testAppConfigMapName, both)}, "not found"},
		{"app configmap absent", []client.Object{nodeConfigMap(testConfigMapName, both)}, "not found"},
		{"app.toml missing", []client.Object{
			nodeConfigMap(testConfigMapName, both),
			nodeConfigMap(testAppConfigMapName, map[string]string{ConfigTomlKey: validTOML})}, `no "app.toml" key`},
		{"config.toml missing", []client.Object{
			nodeConfigMap(testConfigMapName, map[string]string{AppTomlKey: validTOML}),
			nodeConfigMap(testAppConfigMapName, both)}, `no "config.toml" key`},
		{"app.toml empty", []client.Object{
			nodeConfigMap(testConfigMapName, both),
			nodeConfigMap(testAppConfigMapName, map[string]string{AppTomlKey: "   \n"})}, "is empty"},
		{"config.toml malformed", []client.Object{
			nodeConfigMap(testConfigMapName, map[string]string{ConfigTomlKey: "moniker = \n[[["}),
			nodeConfigMap(testAppConfigMapName, both)}, "not valid TOML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(newSyncTestScheme(t)).WithObjects(tc.objs...).Build()

			err := CheckNodeConfig(context.Background(), c, nodeConfigNode())
			if tc.wantErr == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(errors.Is(err, ErrNodeConfigUnresolved)).To(BeTrue(), "got %v", err)
			g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
		})
	}

	t.Run("no nodeConfig reads nothing", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(newSyncTestScheme(t)).Build()
		g.Expect(CheckNodeConfig(context.Background(), c, newSnapshotNode("snap-0", "default"))).To(Succeed())
	})
}

// TestSyncStatefulSet_HoldsWhileNodeConfigUnresolved pins the pre-check. The
// StatefulSet rolls on any template change, so applying a reference that
// cannot be mounted would replace the working pod with a stuck one.
func TestSyncStatefulSet_HoldsWhileNodeConfigUnresolved(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	s := newSyncTestScheme(t)
	valid := map[string]string{ConfigTomlKey: "moniker = \"a\"\n", AppTomlKey: "pruning = \"default\"\n"}

	node := nodeConfigNode()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node, nodeConfigMap(testConfigMapName, valid), nodeConfigMap(testAppConfigMapName, valid)).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	_, err := SyncStatefulSet(ctx, c, c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	node.Spec.NodeConfig.ConfigRef.Name = "rpc-config-v2"
	_, err = SyncStatefulSet(ctx, c, c, s, node, platformtest.Config())
	g.Expect(errors.Is(err, ErrNodeConfigUnresolved)).To(BeTrue(), "got %v", err)

	live := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, live)).To(Succeed())
	cfgVol := findVolume(live.Spec.Template.Spec.Volumes, nodeConfigConfigVolumeName)
	g.Expect(cfgVol).NotTo(BeNil())
	g.Expect(cfgVol.ConfigMap.Name).To(Equal(testConfigMapName), "the live template is left as it was")

	g.Expect(c.Create(ctx, nodeConfigMap("rpc-config-v2", valid))).To(Succeed())
	_, err = SyncStatefulSet(ctx, c, c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, live)).To(Succeed())
	g.Expect(findVolume(live.Spec.Template.Spec.Volumes, nodeConfigConfigVolumeName).ConfigMap.Name).To(Equal("rpc-config-v2"))
}
