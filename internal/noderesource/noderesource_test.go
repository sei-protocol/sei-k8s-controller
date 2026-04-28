package noderesource

import (
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

func newGenesisNode(name, namespace string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "sei-test",
			Image:   "ghcr.io/sei-protocol/seid:latest",
			Entrypoint: &seiv1alpha1.EntrypointConfig{
				Command: []string{"seid"},
				Args:    []string{"start"},
			},
			Validator: &seiv1alpha1.ValidatorSpec{},
			Sidecar:   &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

func newSnapshotNode(name, namespace string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "sei-test",
			Image:   "ghcr.io/sei-protocol/seid:latest",
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

// --- Pod labels ---

func TestResourceLabelsForNode_DefaultsToNodeOnly(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	labels := ResourceLabels(node)

	g.Expect(labels).To(Equal(map[string]string{NodeLabel: "snap-0"}))
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
		NodeLabel:               "snap-0",
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

	sts := GenerateStatefulSet(node, platformtest.Config())

	g.Expect(sts.Labels).To(HaveKeyWithValue("sei.io/nodedeployment", "my-group"))
	g.Expect(sts.Spec.Template.Labels).To(HaveKeyWithValue("sei.io/nodedeployment", "my-group"))
	g.Expect(sts.Spec.Selector.MatchLabels).To(Equal(map[string]string{"sei.io/node": "snap-0"}))
}

// --- StatefulSet generation ---

func TestGenerateNodeStatefulSet_BasicFields(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())

	g.Expect(sts.Name).To(Equal("mynet-0"))
	g.Expect(sts.Namespace).To(Equal("default"))
	g.Expect(sts.Labels).To(HaveKeyWithValue(NodeLabel, "mynet-0"))
	g.Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(sts.Spec.ServiceName).To(Equal("mynet-0"))
	g.Expect(sts.Spec.Selector.MatchLabels).To(Equal(map[string]string{NodeLabel: "mynet-0"}))
	g.Expect(sts.Spec.VolumeClaimTemplates).To(BeEmpty())
}

func TestGenerateNodeStatefulSet_AlwaysHasSidecar(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(initContainers).To(HaveLen(2))
	g.Expect(initContainers[0].Name).To(Equal("seid-init"))
	g.Expect(initContainers[1].Name).To(Equal("sei-sidecar"))
	g.Expect(findInitContainer(initContainers, "snapshot-restore")).To(BeNil())
}

// --- Pod spec ---

func TestBuildNodePodSpec_Genesis_MountsExistingPVC(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	spec := buildNodePodSpec(node, platformtest.Config())

	g.Expect(spec.ServiceAccountName).To(Equal(platformtest.Config().ServiceAccount))
	g.Expect(spec.Volumes).To(HaveLen(1))
	g.Expect(spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-mynet-0"))
}

func TestBuildNodePodSpec_Snapshot_MountsNodePVC(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	spec := buildNodePodSpec(node, platformtest.Config())

	g.Expect(spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-snap-0"))
}

func TestBuildNodePodSpec_SharedPIDNamespace(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())

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

// --- Main container ---

func TestBuildNodeMainContainer_ImageAndEnv(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	c := buildNodeMainContainer(node)

	g.Expect(c.Name).To(Equal("seid"))
	g.Expect(c.Image).To(Equal("ghcr.io/sei-protocol/seid:latest"))
	g.Expect(c.Command).To(Equal([]string{"seid"}))
	g.Expect(c.Args).To(Equal([]string{"start"}))

	var tmpDir string
	for _, e := range c.Env {
		if e.Name == "TMPDIR" {
			tmpDir = e.Value
		}
	}
	g.Expect(tmpDir).To(Equal(dataDir + "/tmp"))
}

func TestBuildNodeMainContainer_DataVolumeMount(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	c := buildNodeMainContainer(node)

	g.Expect(c.VolumeMounts).To(HaveLen(1))
	g.Expect(c.VolumeMounts[0].Name).To(Equal("data"))
	g.Expect(c.VolumeMounts[0].MountPath).To(Equal(dataDir))
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

func TestBuildNodeMainContainer_NoEntrypoint(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	c := buildNodeMainContainer(node)
	g.Expect(c.Command).To(BeNil())
	g.Expect(c.Args).To(BeNil())
}

// --- Sidecar defaults ---

func TestSidecarImage_DefaultWhenEmpty(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{}

	g.Expect(sidecarImage(node)).To(Equal(defaultSidecarImage))
}

func TestSidecarImage_DefaultWhenNil(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = nil

	g.Expect(sidecarImage(node)).To(Equal(defaultSidecarImage))
}

func TestSidecarImage_OverriddenWhenSet(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Image: "custom/sidecar:v2"}

	g.Expect(sidecarImage(node)).To(Equal("custom/sidecar:v2"))
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

	sts := GenerateStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal(defaultSidecarImage))
}

func TestSidecarContainer_CustomImage(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Image: "custom/seictl:v3", Port: 7777}

	sts := GenerateStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal("custom/seictl:v3"))
}

func TestSidecarContainer_RestartPolicyAlways(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc).NotTo(BeNil())
	g.Expect(sc.RestartPolicy).NotTo(BeNil())
	g.Expect(*sc.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
}

func TestSidecarContainer_EnvVars(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
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

	sts := GenerateStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.VolumeMounts).To(HaveLen(1))
	g.Expect(sc.VolumeMounts[0].MountPath).To(Equal(dataDir))
}

func TestSidecarContainer_CustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := GenerateStatefulSet(node, platformtest.Config())
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

	sts := GenerateStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests.Cpu().String()).To(Equal("250m"))
	g.Expect(sc.Resources.Requests.Memory().String()).To(Equal("128Mi"))
	g.Expect(sc.Resources.Limits.Cpu().String()).To(Equal("500m"))
	g.Expect(sc.Resources.Limits.Memory().String()).To(Equal("256Mi"))
}

func TestSidecarContainer_NoResources_DefaultsToEmpty(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests).To(BeNil())
	g.Expect(sc.Resources.Limits).To(BeNil())
}

// --- Sidecar main container (seid with wait wrapper) ---

func TestSidecarMainContainer_StartupProbeTargetsHealthz(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid).NotTo(BeNil())
	probe := seid.StartupProbe
	g.Expect(probe).NotTo(BeNil())
	g.Expect(probe.HTTPGet).NotTo(BeNil())
	g.Expect(probe.HTTPGet.Path).To(Equal("/v0/healthz"))
	g.Expect(probe.HTTPGet.Port.IntValue()).To(Equal(7777))
	g.Expect(probe.InitialDelaySeconds).To(Equal(int32(5)))
	g.Expect(probe.PeriodSeconds).To(Equal(int32(5)))
	g.Expect(probe.FailureThreshold).To(Equal(int32(86400)))
}

func TestSidecarMainContainer_StartupProbeUsesCustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(9999))
}

func TestSidecarMainContainer_ReadinessProbeTargetsLagStatus(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
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

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Command).To(Equal([]string{"/bin/bash", "-c"}))
	g.Expect(seid.Args).To(HaveLen(1))
	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/7777"))
	g.Expect(seid.Args[0]).To(ContainSubstring("GET /v0/healthz HTTP/1.0"))
	g.Expect(seid.Args[0]).To(ContainSubstring(`grep -q "200"`))
	g.Expect(seid.Args[0]).To(ContainSubstring("exec seid"))
}

func TestSidecarMainContainer_WaitWrapper_IncludesEntrypointArgs(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")
	node.Spec.Entrypoint = &seiv1alpha1.EntrypointConfig{
		Command: []string{"seid"},
		Args:    []string{"start", "--home", "/sei"},
	}

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Args[0]).To(ContainSubstring(`exec seid "start" "--home" "/sei"`))
}

func TestSidecarMainContainer_WaitWrapper_NoEntrypoint_DefaultsSeidStart(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Command).To(Equal([]string{"/bin/bash", "-c"}))
	g.Expect(seid.Args[0]).To(ContainSubstring(`exec seid "start" "--home" "/sei"`))
}

func TestSidecarMainContainer_NilSidecarConfig_UsesDefaults(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = nil

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(int(seiconfig.PortSidecar)))
	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/7777"))

	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sc.Image).To(Equal(defaultSidecarImage))
	g.Expect(sc.Ports[0].ContainerPort).To(Equal(seiconfig.PortSidecar))
}

func TestSidecarMainContainer_WaitWrapper_UsesCustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/9999"))
}

// --- Genesis mode specifics ---

func TestGenesisMode_SidecarPresent(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(initContainers).To(HaveLen(2))
	g.Expect(initContainers[0].Name).To(Equal("seid-init"))
	g.Expect(initContainers[1].Name).To(Equal("sei-sidecar"))
}

func TestGenesisMode_NoSnapshotRestoreInitContainer(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(findInitContainer(initContainers, "snapshot-restore")).To(BeNil())
}

func TestGenesisMode_SharedPIDNamespace(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := GenerateStatefulSet(node, platformtest.Config())

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
	g.Expect(svc.Spec.Ports).To(HaveLen(7))
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

	g.Expect(*pvc.Spec.StorageClassName).To(Equal("io2-archive"))

	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	g.Expect(storage.String()).To(Equal("25000Gi"))
}

func TestBuildNodePodSpec_Archive_SchedulesOnArchiveNodepool(t *testing.T) {
	g := NewWithT(t)
	node := newArchiveNode("archive-0", "pacific-1")

	spec := buildNodePodSpec(node, platformtest.Config())

	g.Expect(spec.Tolerations).To(HaveLen(1))
	g.Expect(spec.Tolerations[0].Key).To(Equal("sei.io/workload"))
	g.Expect(spec.Tolerations[0].Value).To(Equal("sei-archive"))

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	g.Expect(terms).To(HaveLen(1))
	g.Expect(terms[0].MatchExpressions).To(HaveLen(1))
	g.Expect(terms[0].MatchExpressions[0].Key).To(Equal("karpenter.sh/nodepool"))
	g.Expect(terms[0].MatchExpressions[0].Values).To(ConsistOf("sei-archive"))
}

func TestBuildNodePodSpec_FullNode_SchedulesOnDefaultNodepool(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("syncer-0", "pacific-1")

	spec := buildNodePodSpec(node, platformtest.Config())

	g.Expect(spec.Tolerations[0].Value).To(Equal("sei-node"))

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	g.Expect(terms[0].MatchExpressions[0].Values).To(ConsistOf("sei-node"))
}

func TestDefaultStorageForMode_Archive(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()

	sc, size := DefaultStorageForMode(string(seiconfig.ModeArchive), cfg)
	g.Expect(sc).To(Equal("io2-archive"))
	g.Expect(size).To(Equal("25000Gi"))
}

func TestDefaultStorageForMode_FullNode(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()

	sc, size := DefaultStorageForMode(string(seiconfig.ModeFull), cfg)
	g.Expect(sc).To(Equal("gp3-10k-750"))
	g.Expect(size).To(Equal("2000Gi"))
}

func TestDefaultResourcesForMode_Archive(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()

	res := DefaultResourcesForMode(string(seiconfig.ModeArchive), cfg)
	g.Expect(res.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("48")))
	g.Expect(res.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("448Gi")))
}

// --- Signing key (validator) ---

func newValidatorNodeWithSigningKey(name, namespace, secretName string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "ghcr.io/sei-protocol/seid:latest",
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

	sts := GenerateStatefulSet(node, platformtest.Config())

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

	sts := GenerateStatefulSet(node, platformtest.Config())
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

	sts := GenerateStatefulSet(node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil(), "sei-sidecar init container must exist")

	g.Expect(findVolumeMount(sidecar.VolumeMounts, signingKeyVolumeName)).To(BeNil(),
		"sidecar container must NOT mount signing-key — has no business reading consensus material")
}

func TestSigningKey_Unset_NoSigningVolume(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default") // FullNode mode, no SigningKey

	sts := GenerateStatefulSet(node, platformtest.Config())

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

	sts := GenerateStatefulSet(node, platformtest.Config())

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

	sts := GenerateStatefulSet(node, platformtest.Config())
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

	sts := GenerateStatefulSet(node, platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")
	g.Expect(sidecar).NotTo(BeNil(), "sei-sidecar init container must exist")

	g.Expect(findVolumeMount(sidecar.VolumeMounts, nodeKeyVolumeName)).To(BeNil(),
		"sidecar container must NOT mount node-key — has no business reading P2P identity")
}

func TestNodeKey_Unset_NoNodeKeyVolume(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default") // FullNode mode, no NodeKey

	sts := GenerateStatefulSet(node, platformtest.Config())

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

	sts := GenerateStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid).NotTo(BeNil())

	signingMount := findVolumeMount(seid.VolumeMounts, signingKeyVolumeName)
	nodeMount := findVolumeMount(seid.VolumeMounts, nodeKeyVolumeName)
	g.Expect(signingMount).NotTo(BeNil())
	g.Expect(nodeMount).NotTo(BeNil())
	g.Expect(signingMount.MountPath).NotTo(Equal(nodeMount.MountPath),
		"signing-key and node-key mounts must target distinct paths under /sei/config/")
}
