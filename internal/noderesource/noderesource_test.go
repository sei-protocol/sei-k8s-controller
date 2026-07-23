package noderesource

import (
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const (
	testChainID   = "sei-test"
	testNodeImage = "ghcr.io/sei-protocol/seid:latest"
)

func newGenesisNode(name, namespace string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   testChainID,
			Image:     testNodeImage,
			Validator: &seiv1alpha1.ValidatorSpec{},
			Sidecar:   &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

func newSnapshotNode(name, namespace string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: testChainID,
			Image:   testNodeImage,
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{
						TargetHeight: 100000000,
					},
					TrustPeriod: "9999h0m0s",
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

func findInitContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func findContainer(containers []corev1.Container, name string) *corev1.Container { //nolint:unparam // test helper designed for reuse
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func envValue(envs []corev1.EnvVar, name string) string {
	for _, e := range envs {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// mustGenerateStatefulSet wraps GenerateStatefulSet with a t.Fatal on
// invariant violation. Used by tests that construct a valid SeiNode and
// expect the runtime guard to pass.
func mustGenerateStatefulSet(t *testing.T, node *seiv1alpha1.SeiNode, p PlatformConfig) *appsv1.StatefulSet {
	t.Helper()
	sts, err := GenerateStatefulSet(node, p)
	if err != nil {
		t.Fatalf("GenerateStatefulSet: %v", err)
	}
	return sts
}

// --- Pod labels ---

func TestResourceLabelsForNode_DefaultsToSystemLabels(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	labels := ResourceLabels(node)

	// newSnapshotNode sets ChainID=testChainID + FullNode mode, so chain
	// + role labels are stamped alongside sei.io/node.
	g.Expect(labels).To(Equal(map[string]string{
		NodeLabel:      node.Name,
		"sei.io/chain": testChainID,
		"sei.io/role":  roleFullNode,
	}))
}

func TestResourceLabelsForNode_MergesPodLabels(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.PodLabels = map[string]string{
		"sei.io/nodedeployment": "my-group",
		"team":                  "platform",
	}
	labels := ResourceLabels(node)

	g.Expect(labels).To(Equal(map[string]string{
		NodeLabel:               node.Name,
		"sei.io/chain":          testChainID,
		"sei.io/role":           roleFullNode,
		"sei.io/nodedeployment": "my-group",
		"team":                  "platform",
	}))
}

func TestResourceLabelsForNode_SystemLabelWins(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.PodLabels = map[string]string{
		NodeLabel: "should-be-overridden",
	}
	labels := ResourceLabels(node)

	g.Expect(labels).To(HaveKeyWithValue(NodeLabel, "snap-0"))
}

func TestGenerateNodeStatefulSet_PodLabelsPropagate(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.PodLabels = map[string]string{
		"sei.io/nodedeployment": "my-group",
	}

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(sts.Labels).To(HaveKeyWithValue("sei.io/nodedeployment", "my-group"))
	g.Expect(sts.Spec.Template.Labels).To(HaveKeyWithValue("sei.io/nodedeployment", "my-group"))
	g.Expect(sts.Spec.Selector.MatchLabels).To(Equal(map[string]string{"sei.io/node": "snap-0"}))
}

// --- StatefulSet generation ---

func TestGenerateNodeStatefulSet_BasicFields(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(sts.Name).To(Equal("mynet-0"))
	g.Expect(sts.Namespace).To(Equal("default"))
	g.Expect(sts.Labels).To(HaveKeyWithValue(NodeLabel, "mynet-0"))
	g.Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(sts.Spec.ServiceName).To(Equal("mynet-0"))
	g.Expect(sts.Spec.Selector.MatchLabels).To(Equal(map[string]string{NodeLabel: "mynet-0"}))
	g.Expect(sts.Spec.VolumeClaimTemplates).To(BeEmpty())
}

func TestGenerateNodeStatefulSet_UsesOnDeleteUpdateStrategy(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))
}

func TestGenerateNodeStatefulSet_AlwaysHasSidecar(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(initContainers).To(HaveLen(3))
	g.Expect(initContainers[0].Name).To(Equal("seid-init"))
	g.Expect(initContainers[1].Name).To(Equal("sei-sidecar"))
	g.Expect(initContainers[2].Name).To(Equal(containerNameRBACProxy))
	g.Expect(findInitContainer(initContainers, "snapshot-restore")).To(BeNil())
}

// --- Pod spec ---

func TestBuildNodePodSpec_Genesis_MountsExistingPVC(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	spec, err := buildNodePodSpec(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(spec.ServiceAccountName).To(Equal(platformtest.Config().ServiceAccount))
	g.Expect(spec.Volumes).To(HaveLen(4)) // data PVC + sidecar-tmp emptyDir + home emptyDir + rbac-proxy-config ConfigMap
	g.Expect(spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-mynet-0"))
	g.Expect(spec.Volumes[1].Name).To(Equal(sidecarTmpVolumeName))
	g.Expect(spec.Volumes[1].EmptyDir).NotTo(BeNil())
	g.Expect(spec.Volumes[2].Name).To(Equal(homeVolumeName))
	g.Expect(spec.Volumes[2].EmptyDir).NotTo(BeNil())
	g.Expect(spec.Volumes[3].Name).To(Equal(rbacProxyConfigVolumeName))
	g.Expect(spec.Volumes[3].ConfigMap).NotTo(BeNil())
}

func TestBuildNodePodSpec_Snapshot_MountsNodePVC(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	spec, err := buildNodePodSpec(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-snap-0"))
}

func TestBuildNodePodSpec_SharedPIDNamespace(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(sts.Spec.Template.Spec.ShareProcessNamespace).NotTo(BeNil())
	g.Expect(*sts.Spec.Template.Spec.ShareProcessNamespace).To(BeTrue())
}

// --- PVC ---

func TestNodeDataPVCClaimName_Genesis(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	g.Expect(DataPVCName(node)).To(Equal("data-mynet-0"))
}

func TestNodeDataPVCClaimName_Snapshot(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	g.Expect(DataPVCName(node)).To(Equal("data-snap-0"))
}

func TestNodeDataPVCClaimName_HonorsImport(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
		Import: &seiv1alpha1.DataVolumeImport{
			PVCName: "preprovisioned-archive-0",
		},
	}
	g.Expect(DataPVCName(node)).To(Equal("preprovisioned-archive-0"))
}

func TestNodeDataPVCClaimName_FallsBackWhenImportEmpty(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{}
	g.Expect(DataPVCName(node)).To(Equal("data-mynet-0"))
}

// --- Main container ---

func TestBuildNodeMainContainer_ImageAndEnv(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	c := buildNodeMainContainer(node)

	g.Expect(c.Name).To(Equal("seid"))
	g.Expect(c.Image).To(Equal(testNodeImage))
	g.Expect(c.Command).To(Equal([]string{"seid"}))
	g.Expect(c.Args).To(Equal([]string{seidStartSubcommand, seidHomeFlag, dataDir}),
		"controller injects canonical command with an explicit --home <dataDir>, decoupled from HOME")

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	g.Expect(env).To(HaveKeyWithValue("HOME", homeMountPath),
		"HOME is the parent of dataDir (emptyDir at homeMountPath); a bare seid resolves $HOME/.sei onto the data dir, while seid start keeps an explicit --home")
	g.Expect(env).To(HaveKeyWithValue("TMPDIR", dataDir+"/tmp"))
}

func TestBuildNodeMainContainer_DataVolumeMount(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	c := buildNodeMainContainer(node)

	g.Expect(c.VolumeMounts).To(HaveLen(2))
	g.Expect(c.VolumeMounts[0].Name).To(Equal("data"))
	g.Expect(c.VolumeMounts[0].MountPath).To(Equal(dataDir))
	g.Expect(c.VolumeMounts[1].Name).To(Equal(homeVolumeName))
	g.Expect(c.VolumeMounts[1].MountPath).To(Equal(homeMountPath))
}

func TestBuildNodeMainContainer_StartupProbe_Genesis(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	c := buildNodeMainContainer(node)

	g.Expect(c.StartupProbe).NotTo(BeNil())
	g.Expect(c.StartupProbe.TCPSocket).NotTo(BeNil())
	g.Expect(c.StartupProbe.TCPSocket.Port.IntValue()).To(Equal(26657))
	g.Expect(c.StartupProbe.FailureThreshold).To(Equal(int32(30)))
}

func TestBuildNodeMainContainer_StartupProbe_Snapshot_HigherThreshold(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	c := buildNodeMainContainer(node)

	g.Expect(c.StartupProbe).NotTo(BeNil())
	g.Expect(c.StartupProbe.FailureThreshold).To(Equal(int32(1800)))
}

// --- Sidecar defaults ---

func TestSidecarImage_DefaultWhenEmpty(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{}

	g.Expect(sidecarImage(node, platformtest.Config())).To(Equal(platformtest.Config().SidecarImage))
}

func TestSidecarImage_DefaultWhenNil(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = nil

	g.Expect(sidecarImage(node, platformtest.Config())).To(Equal(platformtest.Config().SidecarImage))
}

func TestSidecarImage_OverriddenWhenSet(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Image: "custom/sidecar:v2"}

	g.Expect(sidecarImage(node, platformtest.Config())).To(Equal("custom/sidecar:v2"))
}

func TestSidecarPort_DefaultWhenZero(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{}

	g.Expect(SidecarPort(node)).To(Equal(seiconfig.PortSidecar))
}

func TestSidecarPort_DefaultWhenNil(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = nil

	g.Expect(SidecarPort(node)).To(Equal(seiconfig.PortSidecar))
}

func TestSidecarPort_OverriddenWhenSet(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	g.Expect(SidecarPort(node)).To(Equal(int32(9999)))
}

// --- Sidecar container ---

func TestSidecarContainer_DefaultImage(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 7777}

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal(platformtest.Config().SidecarImage))
}

func TestSidecarContainer_CustomImage(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Image: "custom/seictl:v3", Port: 7777}

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal("custom/seictl:v3"))
}

// The sidecar mirrors the seid containers' home setup: the `home` emptyDir at
// homeMountPath (giving the nested data-PVC mount a writable-volume parent under
// the RO rootfs) and HOME pointing at it. Keeps all three seid-touching
// containers symmetric so a bare seid command resolves $HOME/.sei consistently.
func TestSidecarContainer_HomeSymmetryWithSeid(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithOperatorKeyring("validator-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sc).NotTo(BeNil())

	g.Expect(envValue(sc.Env, "HOME")).To(Equal(homeMountPath),
		"sidecar HOME must mirror the seid containers")
	homeMount := findVolumeMount(sc.VolumeMounts, homeVolumeName)
	g.Expect(homeMount).NotTo(BeNil(),
		"sidecar must mount the home emptyDir so the nested data mount has a writable-volume parent")
	g.Expect(homeMount.MountPath).To(Equal(homeMountPath))
}

func TestSidecarContainer_RestartPolicyAlways(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc).NotTo(BeNil())
	g.Expect(sc.RestartPolicy).NotTo(BeNil())
	g.Expect(*sc.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
}

func TestSidecarContainer_EnvVars(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	cfg := platformtest.Config()
	g.Expect(envValue(sc.Env, "SEI_CHAIN_ID")).To(Equal(node.Spec.ChainID))
	g.Expect(envValue(sc.Env, "SEI_SIDECAR_PORT")).To(Equal("7777"))
	g.Expect(envValue(sc.Env, "SEI_HOME")).To(Equal(dataDir))
	g.Expect(envValue(sc.Env, "SEI_GENESIS_BUCKET")).To(Equal(cfg.GenesisBucket))
	g.Expect(envValue(sc.Env, "SEI_GENESIS_REGION")).To(Equal(cfg.GenesisRegion))
}

func TestSidecarContainer_DataVolumeMount(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	dataMount := findVolumeMount(sc.VolumeMounts, "data")
	g.Expect(dataMount).NotTo(BeNil())
	g.Expect(dataMount.MountPath).To(Equal(dataDir))
	tmpMount := findVolumeMount(sc.VolumeMounts, sidecarTmpVolumeName)
	g.Expect(tmpMount).NotTo(BeNil())
	g.Expect(tmpMount.MountPath).To(Equal("/tmp"))
}

func TestSidecarContainer_CustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Ports).To(HaveLen(1))
	g.Expect(sc.Ports[0].ContainerPort).To(Equal(int32(9999)))
}

func TestSidecarContainer_CustomResources(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{
		Port: 7777,
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests.Cpu().String()).To(Equal("250m"))
	g.Expect(sc.Resources.Requests.Memory().String()).To(Equal("128Mi"))
	g.Expect(sc.Resources.Limits.Cpu().String()).To(Equal("500m"))
	g.Expect(sc.Resources.Limits.Memory().String()).To(Equal("256Mi"))
}

func TestSidecarContainer_NoResources_DefaultsToEmpty(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests).To(BeNil())
	g.Expect(sc.Resources.Limits).To(BeNil())
}

// --- Sidecar main container (seid with wait wrapper) ---

func TestSidecarMainContainer_StartupProbeTargetsHealthz(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid).NotTo(BeNil())
	probe := seid.StartupProbe
	g.Expect(probe).NotTo(BeNil())
	g.Expect(probe.HTTPGet).NotTo(BeNil())
	g.Expect(probe.HTTPGet.Path).To(Equal("/v0/healthz"))
	g.Expect(probe.HTTPGet.Port.IntValue()).To(Equal(int(RBACProxyPort)))
	g.Expect(probe.InitialDelaySeconds).To(Equal(int32(5)))
	g.Expect(probe.PeriodSeconds).To(Equal(int32(5)))
	g.Expect(probe.FailureThreshold).To(Equal(int32(86400)))
}

// Custom spec.sidecar.port flows to the proxy --upstream, not to the
// seid startup probe (probe always targets the proxy port).
func TestProxyUpstreamUsesCustomSidecarPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	proxy := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameRBACProxy)

	g.Expect(proxy.Args).To(ContainElement("--upstream=http://127.0.0.1:9999/"))
}

func TestSidecarMainContainer_ReadinessProbeTargetsLagStatus(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid).NotTo(BeNil())
	probe := seid.ReadinessProbe
	g.Expect(probe).NotTo(BeNil())
	g.Expect(probe.HTTPGet).NotTo(BeNil())
	g.Expect(probe.HTTPGet.Path).To(Equal("/lag_status"))
	g.Expect(probe.HTTPGet.Port.IntValue()).To(Equal(26657))
	g.Expect(probe.InitialDelaySeconds).To(Equal(int32(30)))
	g.Expect(probe.PeriodSeconds).To(Equal(int32(10)))
	g.Expect(probe.FailureThreshold).To(Equal(int32(3)))
	g.Expect(probe.TimeoutSeconds).To(Equal(int32(5)))
}

func TestSidecarMainContainer_WaitWrapper_PollsHealthzBeforeExec(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Command).To(Equal([]string{"/bin/bash", "-c"}))
	g.Expect(seid.Args).To(HaveLen(1))
	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/7777"))
	g.Expect(seid.Args[0]).To(ContainSubstring("GET /v0/healthz HTTP/1.0"))
	g.Expect(seid.Args[0]).To(ContainSubstring(`grep -q "200"`))
	g.Expect(seid.Args[0]).To(ContainSubstring("exec seid"))
}

func TestSidecarMainContainer_WaitWrapper_DefaultsSeidStart(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Command).To(Equal([]string{"/bin/bash", "-c"}))
	// seid is started with an explicit --home <dataDir>, decoupled from HOME.
	g.Expect(seid.Args[0]).To(ContainSubstring(fmt.Sprintf(`exec seid "start" "--home" %q`, dataDir)))
}

func TestSidecarMainContainer_NilSidecarConfig_UsesDefaults(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = nil

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(int(RBACProxyPort)))
	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/7777"))

	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sc.Image).To(Equal(platformtest.Config().SidecarImage))
	g.Expect(sc.Ports[0].ContainerPort).To(Equal(seiconfig.PortSidecar))
}

func TestSidecarMainContainer_WaitWrapper_UsesCustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/9999"))
}

// --- Genesis mode specifics ---

func TestGenesisMode_SidecarPresent(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(initContainers).To(HaveLen(3))
	g.Expect(initContainers[0].Name).To(Equal("seid-init"))
	g.Expect(initContainers[1].Name).To(Equal("sei-sidecar"))
	g.Expect(initContainers[2].Name).To(Equal(containerNameRBACProxy))
}

func TestGenesisMode_NoSnapshotRestoreInitContainer(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(findInitContainer(initContainers, "snapshot-restore")).To(BeNil())
}

func TestGenesisMode_SharedPIDNamespace(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(sts.Spec.Template.Spec.ShareProcessNamespace).NotTo(BeNil())
	g.Expect(*sts.Spec.Template.Spec.ShareProcessNamespace).To(BeTrue())
}

// --- Service ---

func TestGenerateNodeHeadlessService(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "ns1")

	svc := GenerateHeadlessService(node)

	g.Expect(svc.Name).To(Equal("mynet-0"))
	g.Expect(svc.Namespace).To(Equal("ns1"))
	g.Expect(svc.Labels).To(HaveKeyWithValue(NodeLabel, "mynet-0"))
	g.Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
	g.Expect(svc.Spec.PublishNotReadyAddresses).To(BeTrue())
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{NodeLabel: "mynet-0"}))
	g.Expect(svc.Spec.Ports).To(HaveLen(8))
}

func TestServicePorts_SevenExpectedPorts(t *testing.T) {
	g := NewWithT(t)
	ports := ServicePorts()
	g.Expect(ports).To(HaveLen(7))
	portNums := make([]int32, len(ports))
	for i, p := range ports {
		portNums[i] = p.Port
	}
	g.Expect(portNums).To(ConsistOf(int32(1317), int32(8545), int32(8546), int32(9090), int32(26656), int32(26657), int32(26660)))
}

func TestContainerPorts_SevenExpectedPorts(t *testing.T) {
	g := NewWithT(t)
	ports := ContainerPorts()
	g.Expect(ports).To(HaveLen(7))
}

// --- PVC generation ---

func TestGenerateNodeDataPVC(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "ns1")

	pvc := GenerateDataPVC(node, platformtest.Config())

	g.Expect(pvc.Name).To(Equal("data-snap-0"))
	g.Expect(pvc.Namespace).To(Equal("ns1"))
	g.Expect(pvc.Labels).To(HaveKeyWithValue(NodeLabel, "snap-0"))
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))

	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	g.Expect(storage.String()).To(Equal("2000Gi"))
}

func newArchiveNode(name, namespace string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "ghcr.io/sei-protocol/seid:v6.4.1",
			Archive: &seiv1alpha1.ArchiveSpec{},
			Sidecar: &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

func TestGenerateNodeDataPVC_Archive(t *testing.T) {
	g := NewWithT(t)
	node := newArchiveNode("archive-0", "pacific-1")

	pvc := GenerateDataPVC(node, platformtest.Config())

	g.Expect(*pvc.Spec.StorageClassName).To(Equal("gp3-archive"))

	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	g.Expect(storage.String()).To(Equal("40Ti"))
}

func TestBuildNodePodSpec_Archive_SchedulesOnArchiveNodepool(t *testing.T) {
	g := NewWithT(t)
	node := newArchiveNode("archive-0", "pacific-1")

	spec, err := buildNodePodSpec(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(spec.Tolerations).To(HaveLen(1))
	g.Expect(spec.Tolerations[0].Key).To(Equal("sei.io/workload"))
	g.Expect(spec.Tolerations[0].Value).To(Equal("sei-archive"))

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	g.Expect(terms).To(HaveLen(1))
	g.Expect(terms[0].MatchExpressions).To(HaveLen(1))
	g.Expect(terms[0].MatchExpressions[0].Key).To(Equal("karpenter.sh/nodepool"))
	g.Expect(terms[0].MatchExpressions[0].Values).To(ConsistOf("sei-archive"))
}

func TestBuildNodePodSpec_Validator_SchedulesOnValidatorNodepool(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("validator-0", "pacific-1")

	spec, err := buildNodePodSpec(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(spec.Tolerations).To(HaveLen(1))
	g.Expect(spec.Tolerations[0].Key).To(Equal("sei.io/workload"))
	g.Expect(spec.Tolerations[0].Value).To(Equal("sei-validator"))

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	g.Expect(terms).To(HaveLen(1))
	g.Expect(terms[0].MatchExpressions).To(HaveLen(1))
	g.Expect(terms[0].MatchExpressions[0].Key).To(Equal("karpenter.sh/nodepool"))
	g.Expect(terms[0].MatchExpressions[0].Values).To(ConsistOf("sei-validator"))
}

func TestBuildNodePodSpec_FullNode_SchedulesOnDefaultNodepool(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("syncer-0", "pacific-1")

	spec, err := buildNodePodSpec(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(spec.Tolerations[0].Value).To(Equal("sei-node"))

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	g.Expect(terms[0].MatchExpressions[0].Values).To(ConsistOf("sei-node"))
}

func TestBuildNodePodSpec_NonDedicated_OnlyDefensiveAntiAffinity(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("syncer-0", "pacific-1")

	spec, err := buildNodePodSpec(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	terms := spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	g.Expect(terms).To(HaveLen(1))
	g.Expect(terms[0].TopologyKey).To(Equal("kubernetes.io/hostname"))
	g.Expect(terms[0].NamespaceSelector).NotTo(BeNil()) // empty = all namespaces
	g.Expect(terms[0].LabelSelector.MatchExpressions[0].Key).To(Equal(DedicatedNodeKey))

	// Non-exclusive pods are not labeled exclusive.
	g.Expect(ResourceLabels(node)).NotTo(HaveKey(DedicatedNodeKey))
}

func TestBuildNodePodSpec_Dedicated_AddsRequesterTermAndLabel(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("syncer-0", "pacific-1")
	node.Annotations = map[string]string{DedicatedNodeKey: "true"}

	spec, err := buildNodePodSpec(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())

	terms := spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	g.Expect(terms).To(HaveLen(2))
	// Defensive term keys on the exclusive label; requester term keys on
	// sei.io/node Exists (spans StatefulSet + bootstrap pods).
	g.Expect(terms[0].LabelSelector.MatchExpressions[0].Key).To(Equal(DedicatedNodeKey))
	g.Expect(terms[1].LabelSelector.MatchExpressions[0].Key).To(Equal(NodeLabel))
	g.Expect(terms[1].LabelSelector.MatchExpressions[0].Operator).To(Equal(metav1.LabelSelectorOpExists))
	for _, term := range terms {
		g.Expect(term.TopologyKey).To(Equal("kubernetes.io/hostname"))
		g.Expect(term.NamespaceSelector).NotTo(BeNil())
	}

	// The annotation is mirrored to a pod-template label so anti-affinity
	// selectors on other pods can match it.
	g.Expect(spec).NotTo(BeNil())
	g.Expect(ResourceLabels(node)).To(HaveKeyWithValue(DedicatedNodeKey, "true"))
}

func TestDefaultStorageForMode_Archive(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()

	sc, size := DefaultStorageForMode(string(seiconfig.ModeArchive), cfg)
	g.Expect(sc).To(Equal("gp3-archive"))
	g.Expect(size).To(Equal("40Ti"))
}

func TestDefaultStorageForMode_FullNode(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()

	sc, size := DefaultStorageForMode(string(seiconfig.ModeFull), cfg)
	g.Expect(sc).To(Equal("gp3-10k-750"))
	g.Expect(size).To(Equal("2000Gi"))
}

// nodeForRole builds a minimal SeiNode whose populated sub-spec selects the
// given sei.io/role. An empty role string yields a full node (no sub-spec).
func nodeForRole(role string) *seiv1alpha1.SeiNode {
	spec := seiv1alpha1.SeiNodeSpec{ChainID: testChainID, Image: testNodeImage}
	switch role {
	case roleValidator:
		spec.Validator = &seiv1alpha1.ValidatorSpec{}
	case roleArchive:
		spec.Archive = &seiv1alpha1.ArchiveSpec{}
	case roleReplayer:
		spec.Replayer = &seiv1alpha1.ReplayerSpec{}
	default:
		spec.FullNode = &seiv1alpha1.FullNodeSpec{}
	}
	return &seiv1alpha1.SeiNode{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"}, Spec: spec}
}

// TestResourcesForNode_DefaultsPerMode locks the code-authoritative per-mode
// footprint: memory request==limit per mode (the blast-radius fix) and never a
// CPU limit.
func TestResourcesForNode_DefaultsPerMode(t *testing.T) {
	cfg := platformtest.Config()
	cases := []struct {
		role   string
		cpuReq string
		memory string
	}{
		{roleValidator, cpuValidator, memValidator},
		{"", cpuRPCClass, memRPCClass}, // fullNode / rpc
		{roleReplayer, cpuRPCClass, memRPCClass},
		{roleArchive, cpuArchive, memArchive},
	}
	for _, tc := range cases {
		t.Run("mode="+tc.role, func(t *testing.T) {
			g := NewWithT(t)
			res := ResourcesForNode(nodeForRole(tc.role), cfg)

			mem := resource.MustParse(tc.memory)
			g.Expect(res.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse(tc.cpuReq)))
			g.Expect(res.Requests[corev1.ResourceMemory]).To(Equal(mem))
			// Memory request == limit (memory-Guaranteed) for every mode.
			g.Expect(res.Limits[corev1.ResourceMemory]).To(Equal(mem))

			// CPU is request-only by design — a CPU limit would throttle seid.
			_, hasCPULimit := res.Limits[corev1.ResourceCPU]
			g.Expect(hasCPULimit).To(BeFalse(), "seid must never carry a CPU limit")
		})
	}
}

// TestResourcesForNode_OverridePerMode verifies an app-config resources.<mode>
// block overrides the code default for that mode only, as memory request==limit.
func TestResourcesForNode_OverridePerMode(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()
	cfg.NodeResourcesReplayer = platform.ResourceOverride{CPURequest: "12", Memory: "180Gi"}

	res := ResourcesForNode(nodeForRole(roleReplayer), cfg)
	mem := resource.MustParse("180Gi")
	g.Expect(res.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("12")))
	g.Expect(res.Requests[corev1.ResourceMemory]).To(Equal(mem))
	g.Expect(res.Limits[corev1.ResourceMemory]).To(Equal(mem))

	// A different mode is unaffected (still on its code default).
	fn := ResourcesForNode(nodeForRole(""), cfg)
	g.Expect(fn.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse(memRPCClass)))
}

// TestResourcesForNode_PartialOverride verifies a memory-only override still
// yields request==limit and leaves the CPU request on the code default.
func TestResourcesForNode_PartialOverride(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()
	cfg.NodeResourcesNode = platform.ResourceOverride{Memory: "200Gi"}

	res := ResourcesForNode(nodeForRole(""), cfg)
	mem := resource.MustParse("200Gi")
	g.Expect(res.Requests[corev1.ResourceMemory]).To(Equal(mem))
	g.Expect(res.Limits[corev1.ResourceMemory]).To(Equal(mem))
	g.Expect(res.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse(cpuRPCClass)))
}

// --- Signing key (validator) ---

func newValidatorNodeWithSigningKey(name, namespace, secretName string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   testNodeImage,
			Validator: &seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: secretName},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: secretName + "-nodekey"},
				},
			},
		},
	}
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func findVolumeMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func TestSigningKey_SecretVolumePresentOnPodTemplate(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	vol := findVolume(sts.Spec.Template.Spec.Volumes, signingKeyVolumeName)
	g.Expect(vol).NotTo(BeNil(), "signing-key volume must be present on the StatefulSet pod template")
	g.Expect(vol.Secret).NotTo(BeNil(), "signing-key volume must be Secret-backed")
	g.Expect(vol.Secret.SecretName).To(Equal("validator-0-key"))
	g.Expect(*vol.Secret.DefaultMode).To(Equal(int32(0o400)))
	g.Expect(vol.Secret.Items).To(HaveLen(1))
	g.Expect(vol.Secret.Items[0].Key).To(Equal(privValidatorKeyDataKey))
	g.Expect(vol.Secret.Items[0].Path).To(Equal(privValidatorKeyDataKey))
}

func TestSigningKey_SeidContainerHasSubPathMount(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil(), "seid main container must exist")

	mount := findVolumeMount(seid.VolumeMounts, signingKeyVolumeName)
	g.Expect(mount).NotTo(BeNil(), "seid container must have signing-key mount")
	g.Expect(mount.MountPath).To(Equal(dataDir + "/config/" + privValidatorKeyDataKey))
	g.Expect(mount.SubPath).To(Equal(privValidatorKeyDataKey))
	g.Expect(mount.ReadOnly).To(BeTrue())
}

func TestSigningKey_SidecarContainerHasNoSigningMount(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil(), "sei-sidecar init container must exist")

	g.Expect(findVolumeMount(sidecar.VolumeMounts, signingKeyVolumeName)).To(BeNil(),
		"sidecar container must NOT mount signing-key — has no business reading consensus material")
}

func TestSigningKey_Unset_NoSigningVolume(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default") // FullNode mode, no SigningKey

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(findVolume(sts.Spec.Template.Spec.Volumes, signingKeyVolumeName)).To(BeNil(),
		"non-signing-key SeiNode must not have a signing-key volume")

	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil())
	g.Expect(findVolumeMount(seid.VolumeMounts, signingKeyVolumeName)).To(BeNil(),
		"seid container must not have a signing-key mount when SigningKey is unset")
}

// --- Node key (validator) ---

func TestNodeKey_SecretVolumePresentOnPodTemplate(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	vol := findVolume(sts.Spec.Template.Spec.Volumes, nodeKeyVolumeName)
	g.Expect(vol).NotTo(BeNil(), "node-key volume must be present on the StatefulSet pod template")
	g.Expect(vol.Secret).NotTo(BeNil(), "node-key volume must be Secret-backed")
	g.Expect(vol.Secret.SecretName).To(Equal("validator-0-key-nodekey"))
	g.Expect(*vol.Secret.DefaultMode).To(Equal(int32(0o400)))
	g.Expect(vol.Secret.Items).To(HaveLen(1))
	g.Expect(vol.Secret.Items[0].Key).To(Equal(nodeKeyDataKey))
	g.Expect(vol.Secret.Items[0].Path).To(Equal(nodeKeyDataKey))
}

func TestNodeKey_SeidContainerHasSubPathMount(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil(), "seid main container must exist")

	mount := findVolumeMount(seid.VolumeMounts, nodeKeyVolumeName)
	g.Expect(mount).NotTo(BeNil(), "seid container must have node-key mount")
	g.Expect(mount.MountPath).To(Equal(dataDir + "/config/" + nodeKeyDataKey))
	g.Expect(mount.SubPath).To(Equal(nodeKeyDataKey))
	g.Expect(mount.ReadOnly).To(BeTrue())
}

func TestNodeKey_SidecarContainerHasNoNodeKeyMount(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil(), "sei-sidecar init container must exist")

	g.Expect(findVolumeMount(sidecar.VolumeMounts, nodeKeyVolumeName)).To(BeNil(),
		"sidecar container must NOT mount node-key — has no business reading P2P identity")
}

func TestNodeKey_Unset_NoNodeKeyVolume(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default") // FullNode mode, no NodeKey

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(findVolume(sts.Spec.Template.Spec.Volumes, nodeKeyVolumeName)).To(BeNil(),
		"non-validator SeiNode must not have a node-key volume")

	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil())
	g.Expect(findVolumeMount(seid.VolumeMounts, nodeKeyVolumeName)).To(BeNil(),
		"seid container must not have a node-key mount when NodeKey is unset")
}

func TestNodeKey_BothMountsCoexist(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil())

	signingMount := findVolumeMount(seid.VolumeMounts, signingKeyVolumeName)
	nodeMount := findVolumeMount(seid.VolumeMounts, nodeKeyVolumeName)
	g.Expect(signingMount).NotTo(BeNil())
	g.Expect(nodeMount).NotTo(BeNil())
	g.Expect(signingMount.MountPath).NotTo(Equal(nodeMount.MountPath),
		"signing-key and node-key mounts must target distinct paths under dataDir/config/")
}

// --- Operator keyring (validator) ---

func newValidatorNodeWithOperatorKeyring(name, namespace string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   testNodeImage,
			Validator: &seiv1alpha1.ValidatorSpec{
				OperatorKeyring: &seiv1alpha1.OperatorKeyringSource{
					Secret: &seiv1alpha1.SecretOperatorKeyringSource{
						SecretName: "validator-0-opk",
						KeyName:    "node_admin",
						PassphraseSecretRef: seiv1alpha1.PassphraseSecretRef{
							SecretName: "validator-0-opk-passphrase",
							Key:        "passphrase",
						},
					},
				},
			},
		},
	}
}

func TestOperatorKeyring_SecretVolumePresentOnPodTemplate(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithOperatorKeyring("validator-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	vol := findVolume(sts.Spec.Template.Spec.Volumes, operatorKeyringVolumeName)
	g.Expect(vol).NotTo(BeNil(), "operator-keyring volume must be present on the StatefulSet pod template")
	g.Expect(vol.Secret).NotTo(BeNil())
	g.Expect(vol.Secret.SecretName).To(Equal("validator-0-opk"))
	g.Expect(*vol.Secret.DefaultMode).To(Equal(int32(0o400)))
	g.Expect(vol.Secret.Items).To(BeNil(),
		"operator-keyring projects the whole Secret as a directory under keyring-file/")
}

func TestOperatorKeyring_SidecarContainerHasMountAndEnv(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithOperatorKeyring("validator-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil(), "sei-sidecar init container must exist")

	mount := findVolumeMount(sidecar.VolumeMounts, operatorKeyringVolumeName)
	g.Expect(mount).NotTo(BeNil(), "sidecar must mount operator-keyring volume")
	g.Expect(mount.MountPath).To(Equal(dataDir + "/" + operatorKeyringDirName))
	g.Expect(mount.ReadOnly).To(BeTrue())
	g.Expect(mount.SubPath).To(BeEmpty(),
		"operator-keyring is a directory mount, not subPath — sidecar needs the whole dir")

	g.Expect(envValue(sidecar.Env, keyringBackendEnvVar)).To(Equal(keyringBackendFile),
		"SEI_KEYRING_BACKEND=file required; SDK 'os' default has no distroless analogue")
	g.Expect(envValue(sidecar.Env, keyringDirEnvVar)).To(Equal(dataDir),
		"SEI_KEYRING_DIR=$SEI_HOME; SDK appends keyring-file/ (doubled path otherwise)")

	var passphraseEnv *corev1.EnvVar
	for i := range sidecar.Env {
		if sidecar.Env[i].Name == keyringPassphraseEnvVar {
			passphraseEnv = &sidecar.Env[i]
		}
	}
	g.Expect(passphraseEnv).NotTo(BeNil(), "sidecar must have %s env injected", keyringPassphraseEnvVar)
	g.Expect(passphraseEnv.ValueFrom).NotTo(BeNil())
	g.Expect(passphraseEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(passphraseEnv.ValueFrom.SecretKeyRef.Name).To(Equal("validator-0-opk-passphrase"))
	g.Expect(passphraseEnv.ValueFrom.SecretKeyRef.Key).To(Equal("passphrase"))
}

// Validator without .secret defaults to a test-backend keyring rooted at
// $SEI_HOME — SDK resolves to $SEI_HOME/keyring-test/, where generate-gentx
// writes the validator key.
func TestOperatorKeyring_ValidatorWithoutSecret_TestBackend(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithSigningKey("validator-0", "default", "validator-0-key")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil(), "sei-sidecar init container must exist")

	g.Expect(envValue(sidecar.Env, keyringBackendEnvVar)).To(Equal(keyringBackendTest),
		"default test backend reads the gentx-written keyring on the data PVC")
	g.Expect(envValue(sidecar.Env, keyringDirEnvVar)).To(Equal(dataDir),
		"SEI_KEYRING_DIR=$SEI_HOME; SDK appends keyring-test/ (doubled path otherwise)")
	g.Expect(envValue(sidecar.Env, keyringPassphraseEnvVar)).To(BeEmpty(),
		"test backend is unencrypted; no passphrase env")

	g.Expect(findVolume(sts.Spec.Template.Spec.Volumes, operatorKeyringVolumeName)).To(BeNil(),
		"no .secret means no projected Secret volume — the keyring lives on the data PVC")

	dataMount := findVolumeMount(sidecar.VolumeMounts, "data")
	g.Expect(dataMount).NotTo(BeNil(),
		"sidecar mounts the data PVC: generate-gentx writes the operator key, sign-and-broadcast reads it")
	g.Expect(dataMount.MountPath).To(Equal(dataDir))
	g.Expect(dataMount.ReadOnly).To(BeFalse(),
		"data mount must be RW for generate-gentx to write $SEI_HOME/keyring-test/")
}

// Non-validator SeiNodes get no keyring env vars — nothing to sign for.
func TestOperatorKeyring_NonValidator_NoEnv(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil())

	g.Expect(envValue(sidecar.Env, keyringBackendEnvVar)).To(BeEmpty(),
		"non-validator SeiNodes must not have SEI_KEYRING_BACKEND set")
	g.Expect(envValue(sidecar.Env, keyringDirEnvVar)).To(BeEmpty())
	g.Expect(envValue(sidecar.Env, keyringPassphraseEnvVar)).To(BeEmpty())
}

func TestOperatorKeyring_SeidMainContainerHasNoMountOrEnv(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithOperatorKeyring("validator-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil(), "seid main container must exist")

	g.Expect(findVolumeMount(seid.VolumeMounts, operatorKeyringVolumeName)).To(BeNil(),
		"seid main container must NOT mount operator-keyring by default — SeidMount is off")
	g.Expect(envValue(seid.Env, keyringPassphraseEnvVar)).To(BeEmpty(),
		"seid main container must not carry the keyring passphrase env by default")

	seidInit := findInitContainer(sts.Spec.Template.Spec.InitContainers, "seid-init")
	g.Expect(seidInit).NotTo(BeNil(), "seid-init container must exist")
	g.Expect(findVolumeMount(seidInit.VolumeMounts, operatorKeyringVolumeName)).To(BeNil(),
		"seid-init must NOT mount operator-keyring")
}

func newValidatorNodeWithOperatorKeyringSeidMount(name, namespace string) *seiv1alpha1.SeiNode {
	node := newValidatorNodeWithOperatorKeyring(name, namespace)
	node.Spec.Validator.OperatorKeyring.Secret.SeidMount = true
	return node
}

// TestOperatorKeyring_SeidMount_MountsOnSeidMain proves the opt-in path:
// setting .secret.SeidMount additionally mounts the keyring + passphrase
// on seid, without disturbing the sidecar's own mount or seid-init.
func TestOperatorKeyring_SeidMount_MountsOnSeidMain(t *testing.T) {
	g := NewWithT(t)
	node := newValidatorNodeWithOperatorKeyringSeidMount("validator-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil(), "seid main container must exist")

	seidMount := findVolumeMount(seid.VolumeMounts, operatorKeyringVolumeName)
	g.Expect(seidMount).NotTo(BeNil(), "SeidMount must mount operator-keyring onto seid main")
	g.Expect(seidMount.MountPath).To(Equal(dataDir+"/"+operatorKeyringDirName),
		"seid must use the same keyring path as the sidecar — the SDK keyring-dir convention")
	g.Expect(seidMount.ReadOnly).To(BeTrue())

	g.Expect(envValue(seid.Env, keyringBackendEnvVar)).To(Equal(keyringBackendFile))
	g.Expect(envValue(seid.Env, keyringDirEnvVar)).To(Equal(dataDir))

	var passphraseEnv *corev1.EnvVar
	for i := range seid.Env {
		if seid.Env[i].Name == keyringPassphraseEnvVar {
			passphraseEnv = &seid.Env[i]
		}
	}
	g.Expect(passphraseEnv).NotTo(BeNil(),
		"seid must carry the passphrase env once exposed — the volume alone can't decrypt non-interactively")
	g.Expect(passphraseEnv.ValueFrom).NotTo(BeNil())
	g.Expect(passphraseEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(passphraseEnv.ValueFrom.SecretKeyRef.Name).To(Equal("validator-0-opk-passphrase"))

	seidInit := findInitContainer(sts.Spec.Template.Spec.InitContainers, "seid-init")
	g.Expect(seidInit).NotTo(BeNil())
	g.Expect(findVolumeMount(seidInit.VolumeMounts, operatorKeyringVolumeName)).To(BeNil(),
		"seid-init must NOT mount operator-keyring even when SeidMount is set — exposure is main-container only")

	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil())
	g.Expect(findVolumeMount(sidecar.VolumeMounts, operatorKeyringVolumeName)).NotTo(BeNil(),
		"the sidecar's own mount must be unaffected by seid's exposure")
}

func TestOperatorKeyring_Unset_NoVolumeOrMount(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	g.Expect(findVolume(sts.Spec.Template.Spec.Volumes, operatorKeyringVolumeName)).To(BeNil())

	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil())
	g.Expect(findVolumeMount(sidecar.VolumeMounts, operatorKeyringVolumeName)).To(BeNil(),
		"sidecar must not mount operator-keyring when OperatorKeyring is unset")
	g.Expect(envValue(sidecar.Env, keyringPassphraseEnvVar)).To(BeEmpty())
}

func TestSidecarContainer_SecurityContext(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil())
	g.Expect(sidecar.SecurityContext).NotTo(BeNil())

	sc := sidecar.SecurityContext
	g.Expect(*sc.RunAsNonRoot).To(BeTrue())
	g.Expect(*sc.RunAsUser).To(Equal(int64(65532)))
	g.Expect(*sc.RunAsGroup).To(Equal(int64(65532)))
	g.Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
	g.Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
	g.Expect(sc.Capabilities).NotTo(BeNil())
	g.Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
	g.Expect(sc.SeccompProfile).NotTo(BeNil())
	g.Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
}

func TestPodSpec_FSGroup(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	g.Expect(sts.Spec.Template.Spec.SecurityContext).NotTo(BeNil())
	g.Expect(*sts.Spec.Template.Spec.SecurityContext.FSGroup).To(Equal(int64(65532)),
		"pod-level fsGroup is the migration primitive — kubelet OnRootMismatch chowns the volume gid on first mount")
	g.Expect(*sts.Spec.Template.Spec.SecurityContext.FSGroupChangePolicy).To(Equal(corev1.FSGroupChangeOnRootMismatch),
		"OnRootMismatch keeps the chown cost to once-per-PVC")
	g.Expect(sts.Spec.Template.Spec.SecurityContext.SupplementalGroups).To(BeEmpty(),
		"workaround for root-seid is no longer needed once seid runs as gid 65532 natively")
}

func TestSeidMainContainer_RunsAsNonRoot(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil())
	expectSeidNonRootSecurityContext(g, seid.SecurityContext)
}

func TestSeidInitContainer_RunsAsNonRoot(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seidInit := findInitContainer(sts.Spec.Template.Spec.InitContainers, "seid-init")
	g.Expect(seidInit).NotTo(BeNil())
	expectSeidNonRootSecurityContext(g, seidInit.SecurityContext)
}

func TestSeidInitContainer_BareScript(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seidInit := findInitContainer(sts.Spec.Template.Spec.InitContainers, "seid-init")
	g.Expect(seidInit).NotTo(BeNil())
	g.Expect(seidInit.Command).To(HaveLen(3))

	script := seidInit.Command[2]
	// Migration logic (chown, chmod, sentinel) lives in the kubelet via
	// fsGroup OnRootMismatch — not in this script.
	g.Expect(script).NotTo(ContainSubstring("chown"))
	g.Expect(script).NotTo(ContainSubstring("chmod"))
	// Script targets the literal data dir for --home + the genesis guard + tmp,
	// decoupled from HOME (a non-data-dir emptyDir).
	g.Expect(script).To(ContainSubstring(fmt.Sprintf(`seid init sei-test --chain-id sei-test --home "%s" --overwrite`, dataDir)))
	g.Expect(script).To(ContainSubstring(fmt.Sprintf(`mkdir -p "%s/tmp"`, dataDir)))
	g.Expect(script).To(ContainSubstring(fmt.Sprintf(`[ -f "%s/config/genesis.json" ]`, dataDir)),
		"the init skip-guard must check the data dir, not $HOME")
	g.Expect(script).NotTo(ContainSubstring("$HOME"),
		"script must not depend on $HOME for the seid home")

	env := map[string]string{}
	for _, e := range seidInit.Env {
		env[e.Name] = e.Value
	}
	g.Expect(env).To(HaveKeyWithValue("HOME", homeMountPath),
		"HOME is the parent of dataDir; the init script targets --home <dataDir> explicitly, and a bare seid would resolve $HOME/.sei onto the same data dir")
}

// TestHomeDirIsDataDirParent locks the constant relationship the whole
// home-convergence design rests on: HOME must be the PARENT of the data dir
// (dataDir == homeMountPath/.sei), so a bare `seid` resolving $HOME/.sei lands
// on the data dir without nesting (#449). Both derive from platform, so this is
// belt-and-suspenders against a future edit decoupling them.
func TestHomeDirIsDataDirParent(t *testing.T) {
	g := NewWithT(t)
	g.Expect(dataDir).To(Equal(homeMountPath+"/.sei"),
		"dataDir must be homeMountPath/.sei so a bare seid resolves $HOME/.sei onto the data dir")
	g.Expect(dataDir).NotTo(Equal(homeMountPath),
		"HOME must never equal the data dir itself — that is the #449 nesting bug")
}

func seidInitScript(t *testing.T, node *seiv1alpha1.SeiNode) string {
	t.Helper()
	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	seidInit := findInitContainer(sts.Spec.Template.Spec.InitContainers, "seid-init")
	if seidInit == nil {
		t.Fatal("seid-init container not found")
	}
	return seidInit.Command[2]
}

// TestSeidInitContainer_ClientTomlKeyringBackend asserts the init script writes
// keyring-backend into client.toml with the backend matching the node's keyring
// decision: "test" for a stock validator (newGenesisNode is a test-backend
// validator), "file" for a .secret validator, and absent for a non-validator.
func TestSeidInitContainer_ClientTomlKeyringBackend(t *testing.T) {
	clientToml := dataDir + "/config/client.toml"

	t.Run("test-backend validator writes backend=test", func(t *testing.T) {
		g := NewWithT(t)
		script := seidInitScript(t, newGenesisNode("v-0", "default"))
		g.Expect(script).To(ContainSubstring(clientToml))
		g.Expect(script).To(ContainSubstring(`keyring-backend = "test"`))
		g.Expect(script).NotTo(ContainSubstring(`keyring-backend = "file"`))
	})

	t.Run(".secret validator writes backend=file", func(t *testing.T) {
		g := NewWithT(t)
		script := seidInitScript(t, newValidatorNodeWithOperatorKeyring("v-0", "default"))
		g.Expect(script).To(ContainSubstring(clientToml))
		g.Expect(script).To(ContainSubstring(`keyring-backend = "file"`))
		g.Expect(script).NotTo(ContainSubstring(`keyring-backend = "test"`))
	})

	t.Run("non-validator leaves client.toml stock", func(t *testing.T) {
		g := NewWithT(t)
		script := seidInitScript(t, newSnapshotNode("fn-0", "default"))
		g.Expect(script).NotTo(ContainSubstring("client.toml"),
			"non-validators have no keys; the init script must not touch client.toml")
		g.Expect(script).NotTo(ContainSubstring("keyring-backend"))
	})
}

// TestSeidInitContainer_ClientTomlWriteOutsideGenesisGuard asserts the
// client.toml keyring-backend write runs on EVERY boot, not just first init —
// it must sit after the `if [ -f genesis.json ] ... fi` skip-guard so
// already-provisioned PVCs receive the write. A write trapped inside the guard
// is the silent-stale-backend regression this guards against.
func TestSeidInitContainer_ClientTomlWriteOutsideGenesisGuard(t *testing.T) {
	g := NewWithT(t)
	script := seidInitScript(t, newGenesisNode("v-0", "default"))

	guardEnd := strings.Index(script, "fi && mkdir -p")
	g.Expect(guardEnd).To(BeNumerically(">", 0), "genesis skip-guard must be present")
	writeStart := strings.Index(script, "CLIENT_TOML=")
	g.Expect(writeStart).To(BeNumerically(">", 0), "client.toml write must be present")
	g.Expect(writeStart).To(BeNumerically(">", guardEnd),
		"client.toml write must come after the genesis skip-guard's fi, so it runs on every boot")
}

func expectSeidNonRootSecurityContext(g Gomega, sc *corev1.SecurityContext) {
	g.Expect(sc).NotTo(BeNil())
	g.Expect(sc.RunAsNonRoot).To(Equal(ptr.To(true)))              //nolint:modernize
	g.Expect(sc.RunAsUser).To(Equal(ptr.To(int64(65532))))         //nolint:modernize
	g.Expect(sc.RunAsGroup).To(Equal(ptr.To(int64(65532))))        //nolint:modernize
	g.Expect(sc.AllowPrivilegeEscalation).To(Equal(ptr.To(false))) //nolint:modernize
	g.Expect(sc.Capabilities).NotTo(BeNil())
	g.Expect(sc.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
	g.Expect(sc.Capabilities.Add).To(BeEmpty())
	g.Expect(sc.SeccompProfile).NotTo(BeNil())
	g.Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
}

// --- assertOperatorKeyringContainment ---

const (
	keyringTestSeidInitName        = "seid-init"
	keyringTestSidecarName         = "sei-sidecar"
	keyringTestMountPath           = dataDir + "/" + operatorKeyringDirName
	keyringTestPassphraseSecret    = "validator-0-opk-pass"
	keyringTestPassphraseSecretKey = "passphrase"
)

func validatorNodeWithOperatorKeyring() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "v-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: testChainID,
			Image:   testNodeImage,
			Validator: &seiv1alpha1.ValidatorSpec{
				OperatorKeyring: &seiv1alpha1.OperatorKeyringSource{
					Secret: &seiv1alpha1.SecretOperatorKeyringSource{
						SecretName: "validator-0-opk",
						PassphraseSecretRef: seiv1alpha1.PassphraseSecretRef{
							SecretName: keyringTestPassphraseSecret,
							Key:        keyringTestPassphraseSecretKey,
						},
					},
				},
			},
		},
	}
}

func TestAssertOperatorKeyringContainment_NoKeyringConfigured(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("v-0", "default")
	// Even a deliberately mis-mounted volume must be ignored when no
	// operator-keyring is configured — the guard is opt-in by spec.
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: containerNameSeid, VolumeMounts: []corev1.VolumeMount{
				{Name: operatorKeyringVolumeName, MountPath: "/somewhere"},
			}},
		},
	}
	g.Expect(assertOperatorKeyringContainment(node, spec)).To(Succeed())
}

func TestAssertOperatorKeyringContainment_SidecarOnlyMount_Passes(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: containerNameSeid},
		},
		InitContainers: []corev1.Container{
			{Name: keyringTestSeidInitName},
			{Name: keyringTestSidecarName, VolumeMounts: []corev1.VolumeMount{
				{Name: operatorKeyringVolumeName, MountPath: keyringTestMountPath},
			}},
		},
	}
	g.Expect(assertOperatorKeyringContainment(node, spec)).To(Succeed())
}

func TestAssertOperatorKeyringContainment_SeidMainMisMounted_Rejects(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: containerNameSeid, VolumeMounts: []corev1.VolumeMount{
				{Name: operatorKeyringVolumeName, MountPath: keyringTestMountPath},
			}},
		},
	}
	err := assertOperatorKeyringContainment(node, spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`container "seid"`))
}

// TestAssertOperatorKeyringContainment_SeidMountOnSeidMain_Passes proves the
// opt-in carve-out: the identical mount that TestAssertOperatorKeyringContainment_SeidMainMisMounted_Rejects
// rejects by default is permitted once .secret.SeidMount is set.
func TestAssertOperatorKeyringContainment_SeidMountOnSeidMain_Passes(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	node.Spec.Validator.OperatorKeyring.Secret.SeidMount = true
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: containerNameSeid, VolumeMounts: []corev1.VolumeMount{
				{Name: operatorKeyringVolumeName, MountPath: keyringTestMountPath},
			}},
		},
		InitContainers: []corev1.Container{
			{Name: keyringTestSidecarName, VolumeMounts: []corev1.VolumeMount{
				{Name: operatorKeyringVolumeName, MountPath: keyringTestMountPath},
			}},
		},
	}
	g.Expect(assertOperatorKeyringContainment(node, spec)).To(Succeed())
}

// TestAssertOperatorKeyringContainment_SeidMountButSeidInitMisMounted_Rejects
// proves the opt-in is main-container-only: SeidMount never widens the
// permit set to seid-init.
func TestAssertOperatorKeyringContainment_SeidMountButSeidInitMisMounted_Rejects(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	node.Spec.Validator.OperatorKeyring.Secret.SeidMount = true
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: keyringTestSeidInitName, VolumeMounts: []corev1.VolumeMount{
				{Name: operatorKeyringVolumeName, MountPath: keyringTestMountPath},
			}},
		},
	}
	err := assertOperatorKeyringContainment(node, spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`container "seid-init"`))
}

func TestAssertOperatorKeyringContainment_SeidInitMisMounted_Rejects(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: keyringTestSeidInitName, VolumeMounts: []corev1.VolumeMount{
				{Name: operatorKeyringVolumeName, MountPath: keyringTestMountPath},
			}},
		},
	}
	err := assertOperatorKeyringContainment(node, spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`container "seid-init"`))
}

func TestAssertOperatorKeyringContainment_PassphraseEnvOnSeidMain_Rejects(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: containerNameSeid, Env: []corev1.EnvVar{{
				Name: "SEI_KEYRING_PASSPHRASE",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: keyringTestPassphraseSecret},
						Key:                  keyringTestPassphraseSecretKey,
					},
				},
			}}},
		},
	}
	err := assertOperatorKeyringContainment(node, spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`passphrase Secret "` + keyringTestPassphraseSecret + `"`))
	g.Expect(err.Error()).To(ContainSubstring(`container "seid"`))
}

func TestAssertOperatorKeyringContainment_PassphraseEnvFromOnSeidInit_Rejects(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: keyringTestSeidInitName, EnvFrom: []corev1.EnvFromSource{{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: keyringTestPassphraseSecret},
				},
			}}},
		},
	}
	err := assertOperatorKeyringContainment(node, spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`envFrom`))
	g.Expect(err.Error()).To(ContainSubstring(`container "seid-init"`))
}

func TestGenerateStatefulSet_ProductionPodSpec_PassesGuard(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	_, err := GenerateStatefulSet(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
}

func TestGenerateStatefulSet_SeidMount_PassesGuard(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithOperatorKeyring()
	node.Spec.Validator.OperatorKeyring.Secret.SeidMount = true
	_, err := GenerateStatefulSet(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Cosmos exporter ---

func TestCosmosExporter_AlwaysPresent(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)
	g.Expect(ce).NotTo(BeNil())
	g.Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(2))
}

func TestCosmosExporter_DefaultImage(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

	g.Expect(ce.Image).To(Equal(platformtest.Config().CosmosExporterImage))
}

func TestCosmosExporter_PortIsFixed(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

	// Port is intentionally not user-configurable: the platform
	// PodMonitor targets the named port `cosmos-metrics`.
	g.Expect(ce.Ports[0].ContainerPort).To(Equal(int32(9300)))
	g.Expect(ce.Ports[0].Name).To(Equal("cosmos-metrics"))
}

func TestCosmosExporter_ErrorWhenImageUnset(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")
	cfg := platformtest.Config()
	cfg.CosmosExporterImage = ""

	_, err := GenerateStatefulSet(node, cfg)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("images.cosmosExporter is required"))
}

func TestCosmosExporter_ReadinessProbe_TargetsExporterListener(t *testing.T) {
	for _, name := range []string{"full", roleValidator} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			var node *seiv1alpha1.SeiNode
			if name == roleValidator {
				node = newGenesisNode("ce-val-0", "default")
			} else {
				node = newSnapshotNode("ce-fn-0", "default")
			}

			sts := mustGenerateStatefulSet(t, node, platformtest.Config())
			ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

			g.Expect(ce.StartupProbe).To(BeNil())
			g.Expect(ce.ReadinessProbe).NotTo(BeNil())
			g.Expect(ce.ReadinessProbe.TCPSocket).NotTo(BeNil())
			g.Expect(ce.ReadinessProbe.TCPSocket.Port.IntVal).To(Equal(int32(9300)))
			g.Expect(ce.ReadinessProbe.PeriodSeconds).To(Equal(int32(10)))
			g.Expect(ce.ReadinessProbe.FailureThreshold).To(Equal(int32(6)))

			g.Expect(ce.LivenessProbe).NotTo(BeNil())
			g.Expect(ce.LivenessProbe.TCPSocket).NotTo(BeNil())
			g.Expect(ce.LivenessProbe.TCPSocket.Port.IntVal).To(Equal(int32(9300)))
			g.Expect(ce.LivenessProbe.InitialDelaySeconds).To(Equal(int32(600)))
			g.Expect(ce.LivenessProbe.FailureThreshold).To(Equal(int32(3)))
		})
	}
}

func TestCosmosExporter_CommandIsBashWaitWrapper(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

	g.Expect(ce.Command).To(Equal([]string{"/bin/bash", "-c"}))
	g.Expect(ce.Args).To(HaveLen(1))
	g.Expect(ce.Args[0]).To(ContainSubstring("/dev/tcp/localhost/9090"))
	g.Expect(ce.Args[0]).To(ContainSubstring("exec /usr/local/bin/sei-cosmos-exporter"))
}

func TestCosmosExporter_MountsTmpEmptyDir(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

	var hasTmp bool
	for _, m := range ce.VolumeMounts {
		if m.Name == sidecarTmpVolumeName && m.MountPath == sidecarTmpMountPath {
			hasTmp = true
			break
		}
	}
	g.Expect(hasTmp).To(BeTrue(), "cosmos-exporter must mount sidecar-tmp at /tmp (ReadOnlyRootFilesystem)")
}

func TestCosmosExporter_SeiArgs(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

	g.Expect(ce.Args).To(HaveLen(1))
	g.Expect(ce.Args[0]).To(ContainSubstring(`"--denom" "usei"`))
	g.Expect(ce.Args[0]).To(ContainSubstring(`"--denom-coefficient" "1000000"`))
	g.Expect(ce.Args[0]).To(ContainSubstring(`"--bech-prefix" "sei"`))
}

func TestCosmosExporter_DefaultResources(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

	// 50m/64Mi requests, 512Mi memory limit, no CPU limit (see
	// defaultCosmosExporterResources — scrape pulls would throttle).
	cpuReq := ce.Resources.Requests[corev1.ResourceCPU]
	memReq := ce.Resources.Requests[corev1.ResourceMemory]
	memLim := ce.Resources.Limits[corev1.ResourceMemory]
	g.Expect(cpuReq.String()).To(Equal("50m"))
	g.Expect(memReq.String()).To(Equal("64Mi"))
	g.Expect(memLim.String()).To(Equal("512Mi"))
	_, hasCPULimit := ce.Resources.Limits[corev1.ResourceCPU]
	g.Expect(hasCPULimit).To(BeFalse())
}

func TestResourceLabels_ChainAndRoleStampedUnconditionally(t *testing.T) {
	g := NewWithT(t)
	tests := []struct {
		name     string
		mutate   func(*seiv1alpha1.SeiNode)
		expected string
	}{
		{"validator", func(n *seiv1alpha1.SeiNode) { n.Spec.Validator = &seiv1alpha1.ValidatorSpec{} }, roleValidator},
		{"archive", func(n *seiv1alpha1.SeiNode) { n.Spec.Archive = &seiv1alpha1.ArchiveSpec{} }, roleArchive},
		{"replayer", func(n *seiv1alpha1.SeiNode) { n.Spec.Replayer = &seiv1alpha1.ReplayerSpec{} }, roleReplayer},
		{"fullNode", func(n *seiv1alpha1.SeiNode) { n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{} }, roleFullNode},
		// No mode sub-spec: deriveRole defaults to full node.
		{"noModeSet", func(_ *seiv1alpha1.SeiNode) {}, roleFullNode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
				Spec:       seiv1alpha1.SeiNodeSpec{ChainID: "pacific-1", Image: testNodeImage},
			}
			tt.mutate(node)

			labels := ResourceLabels(node)

			g.Expect(labels).To(HaveKeyWithValue("sei.io/chain", "pacific-1"))
			g.Expect(labels).To(HaveKeyWithValue("sei.io/role", tt.expected))
		})
	}
}

func TestResourceLabels_ChainOmittedWhenChainIDEmpty(t *testing.T) {
	g := NewWithT(t)
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
		Spec:       seiv1alpha1.SeiNodeSpec{FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	labels := ResourceLabels(node)

	g.Expect(labels).NotTo(HaveKey("sei.io/chain"))
}

func TestResourceLabels_SnapshotPublishLabel(t *testing.T) {
	g := NewWithT(t)
	publish := &seiv1alpha1.TendermintSnapshotPublishConfig{}
	sgWithPublish := &seiv1alpha1.SnapshotGenerationConfig{
		Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: 2, Publish: publish},
	}
	sgNoPublish := &seiv1alpha1.SnapshotGenerationConfig{
		Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: 2},
	}

	tests := []struct {
		name        string
		mutate      func(*seiv1alpha1.SeiNode)
		wantPublish bool
	}{
		{"fullNode publish set", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{SnapshotGeneration: sgWithPublish}
		}, true},
		{"fullNode publish unset", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{SnapshotGeneration: sgNoPublish}
		}, false},
		{"fullNode no snapshotGeneration", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
		}, false},
		{"archive publish set", func(n *seiv1alpha1.SeiNode) {
			n.Spec.Archive = &seiv1alpha1.ArchiveSpec{SnapshotGeneration: sgWithPublish}
		}, true},
		{"archive publish unset", func(n *seiv1alpha1.SeiNode) {
			n.Spec.Archive = &seiv1alpha1.ArchiveSpec{SnapshotGeneration: sgNoPublish}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
				Spec:       seiv1alpha1.SeiNodeSpec{ChainID: "pacific-1", Image: testNodeImage},
			}
			tt.mutate(node)

			labels := ResourceLabels(node)

			if tt.wantPublish {
				g.Expect(labels).To(HaveKeyWithValue("sei.io/snapshot-publish", "true"))
			} else {
				g.Expect(labels).NotTo(HaveKey("sei.io/snapshot-publish"))
			}
		})
	}
}

func TestResourceLabels_SnapshotPublishNotInSelector(t *testing.T) {
	g := NewWithT(t)
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   testNodeImage,
			FullNode: &seiv1alpha1.FullNodeSpec{
				SnapshotGeneration: &seiv1alpha1.SnapshotGenerationConfig{
					Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
						KeepRecent: 2,
						Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
					},
				},
			},
		},
	}

	// The publish label is mutable (toggling publish must not force a
	// StatefulSet recreation), so it lives on the pod template only — never
	// in the immutable selector.
	g.Expect(SelectorLabels(node)).NotTo(HaveKey("sei.io/snapshot-publish"))
	g.Expect(ResourceLabels(node)).To(HaveKeyWithValue("sei.io/snapshot-publish", "true"))
}

func TestResourceLabels_NotInSelector(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	// Chain + role must NOT live in the immutable StatefulSet selector
	// — otherwise renaming the chain (rare) or rolling between modes
	// would require StatefulSet recreation. Only sei.io/node belongs
	// in the selector.
	g.Expect(sts.Spec.Selector.MatchLabels).NotTo(HaveKey("sei.io/chain"))
	g.Expect(sts.Spec.Selector.MatchLabels).NotTo(HaveKey("sei.io/role"))
	g.Expect(sts.Spec.Selector.MatchLabels).To(HaveLen(1))
}

func TestCosmosExporter_NonRootSecurityContext(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("ce-0", "default")

	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	ce := findContainer(sts.Spec.Template.Spec.Containers, containerNameCosmosExporter)

	g.Expect(ce.SecurityContext).NotTo(BeNil())
	g.Expect(ce.SecurityContext.RunAsNonRoot).NotTo(BeNil())
	g.Expect(*ce.SecurityContext.RunAsNonRoot).To(BeTrue())
	g.Expect(*ce.SecurityContext.RunAsUser).To(Equal(int64(65532)))
}
