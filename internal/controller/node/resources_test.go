package node

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
	labels := resourceLabelsForNode(node)

	g.Expect(labels).To(Equal(map[string]string{nodeLabel: "snap-0"}))
}

func TestResourceLabelsForNode_MergesPodLabels(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.PodLabels = map[string]string{
		"sei.io/nodedeployment": "my-group",
		"team":             "platform",
	}
	labels := resourceLabelsForNode(node)

	g.Expect(labels).To(Equal(map[string]string{
		nodeLabel:          "snap-0",
		"sei.io/nodedeployment": "my-group",
		"team":             "platform",
	}))
}

func TestResourceLabelsForNode_SystemLabelWins(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.PodLabels = map[string]string{
		nodeLabel: "should-be-overridden",
	}
	labels := resourceLabelsForNode(node)

	g.Expect(labels).To(HaveKeyWithValue(nodeLabel, "snap-0"))
}

func TestGenerateNodeStatefulSet_PodLabelsPropagate(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.PodLabels = map[string]string{
		"sei.io/nodedeployment": "my-group",
	}

	sts := generateNodeStatefulSet(node, platformtest.Config())

	g.Expect(sts.Labels).To(HaveKeyWithValue("sei.io/nodedeployment", "my-group"))
	g.Expect(sts.Spec.Template.Labels).To(HaveKeyWithValue("sei.io/nodedeployment", "my-group"))
	g.Expect(sts.Spec.Selector.MatchLabels).To(HaveKeyWithValue("sei.io/nodedeployment", "my-group"))
}

// --- StatefulSet generation ---

func TestGenerateNodeStatefulSet_BasicFields(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())

	g.Expect(sts.Name).To(Equal("mynet-0"))
	g.Expect(sts.Namespace).To(Equal("default"))
	g.Expect(sts.Labels).To(HaveKeyWithValue(nodeLabel, "mynet-0"))
	g.Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(sts.Spec.ServiceName).To(Equal("mynet-0"))
	g.Expect(sts.Spec.Selector.MatchLabels).To(Equal(sts.Spec.Template.Labels))
	g.Expect(sts.Spec.VolumeClaimTemplates).To(BeEmpty())
}

func TestGenerateNodeStatefulSet_AlwaysHasSidecar(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
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

	sts := generateNodeStatefulSet(node, platformtest.Config())

	g.Expect(sts.Spec.Template.Spec.ShareProcessNamespace).NotTo(BeNil())
	g.Expect(*sts.Spec.Template.Spec.ShareProcessNamespace).To(BeTrue())
}

// --- PVC ---

func TestNodeDataPVCClaimName_Genesis(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	g.Expect(nodeDataPVCClaimName(node)).To(Equal("data-mynet-0"))
}

func TestNodeDataPVCClaimName_Snapshot(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	g.Expect(nodeDataPVCClaimName(node)).To(Equal("data-snap-0"))
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

	g.Expect(sidecarPort(node)).To(Equal(seiconfig.PortSidecar))
}

func TestSidecarPort_DefaultWhenNil(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = nil

	g.Expect(sidecarPort(node)).To(Equal(seiconfig.PortSidecar))
}

func TestSidecarPort_OverriddenWhenSet(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	g.Expect(sidecarPort(node)).To(Equal(int32(9999)))
}

// --- Sidecar container ---

func TestSidecarContainer_DefaultImage(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 7777}

	sts := generateNodeStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal(defaultSidecarImage))
}

func TestSidecarContainer_CustomImage(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Image: "custom/seictl:v3", Port: 7777}

	sts := generateNodeStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal("custom/seictl:v3"))
}

func TestSidecarContainer_RestartPolicyAlways(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc).NotTo(BeNil())
	g.Expect(sc.RestartPolicy).NotTo(BeNil())
	g.Expect(*sc.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
}

func TestSidecarContainer_EnvVars(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
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

	sts := generateNodeStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.VolumeMounts).To(HaveLen(1))
	g.Expect(sc.VolumeMounts[0].MountPath).To(Equal(dataDir))
}

func TestSidecarContainer_CustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := generateNodeStatefulSet(node, platformtest.Config())
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

	sts := generateNodeStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests.Cpu().String()).To(Equal("250m"))
	g.Expect(sc.Resources.Requests.Memory().String()).To(Equal("128Mi"))
	g.Expect(sc.Resources.Limits.Cpu().String()).To(Equal("500m"))
	g.Expect(sc.Resources.Limits.Memory().String()).To(Equal("256Mi"))
}

func TestSidecarContainer_NoResources_DefaultsToEmpty(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests).To(BeNil())
	g.Expect(sc.Resources.Limits).To(BeNil())
}

// --- Sidecar main container (seid with wait wrapper) ---

func TestSidecarMainContainer_StartupProbeTargetsHealthz(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
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

	sts := generateNodeStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(9999))
}

func TestSidecarMainContainer_ReadinessProbeTargetsLagStatus(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
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

	sts := generateNodeStatefulSet(node, platformtest.Config())
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

	sts := generateNodeStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Args[0]).To(ContainSubstring(`exec seid "start" "--home" "/sei"`))
}

func TestSidecarMainContainer_WaitWrapper_NoEntrypoint_DefaultsSeidStart(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Command).To(Equal([]string{"/bin/bash", "-c"}))
	g.Expect(seid.Args[0]).To(ContainSubstring(`exec seid "start" "--home" "/sei"`))
}

func TestSidecarMainContainer_NilSidecarConfig_UsesDefaults(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = nil

	sts := generateNodeStatefulSet(node, platformtest.Config())
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

	sts := generateNodeStatefulSet(node, platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/9999"))
}

// --- Genesis mode specifics ---

func TestGenesisMode_SidecarPresent(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(initContainers).To(HaveLen(2))
	g.Expect(initContainers[0].Name).To(Equal("seid-init"))
	g.Expect(initContainers[1].Name).To(Equal("sei-sidecar"))
}

func TestGenesisMode_NoSnapshotRestoreInitContainer(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(findInitContainer(initContainers, "snapshot-restore")).To(BeNil())
}

func TestGenesisMode_SharedPIDNamespace(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := generateNodeStatefulSet(node, platformtest.Config())

	g.Expect(sts.Spec.Template.Spec.ShareProcessNamespace).NotTo(BeNil())
	g.Expect(*sts.Spec.Template.Spec.ShareProcessNamespace).To(BeTrue())
}

// --- Service ---

func TestGenerateNodeHeadlessService(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "ns1")

	svc := generateNodeHeadlessService(node)

	g.Expect(svc.Name).To(Equal("mynet-0"))
	g.Expect(svc.Namespace).To(Equal("ns1"))
	g.Expect(svc.Labels).To(HaveKeyWithValue(nodeLabel, "mynet-0"))
	g.Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
	g.Expect(svc.Spec.PublishNotReadyAddresses).To(BeTrue())
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(nodeLabel, "mynet-0"))
	g.Expect(svc.Spec.Ports).To(HaveLen(7))
}

func TestServicePorts_SevenExpectedPorts(t *testing.T) {
	g := NewWithT(t)
	ports := servicePorts()
	g.Expect(ports).To(HaveLen(7))
	portNums := make([]int32, len(ports))
	for i, p := range ports {
		portNums[i] = p.Port
	}
	g.Expect(portNums).To(ConsistOf(int32(1317), int32(8545), int32(8546), int32(9090), int32(26656), int32(26657), int32(26660)))
}

func TestContainerPorts_SevenExpectedPorts(t *testing.T) {
	g := NewWithT(t)
	ports := containerPorts()
	g.Expect(ports).To(HaveLen(7))
}

// --- PVC generation ---

func TestGenerateNodeDataPVC(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "ns1")

	pvc := generateNodeDataPVC(node, platformtest.Config())

	g.Expect(pvc.Name).To(Equal("data-snap-0"))
	g.Expect(pvc.Namespace).To(Equal("ns1"))
	g.Expect(pvc.Labels).To(HaveKeyWithValue(nodeLabel, "snap-0"))
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))

	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	g.Expect(storage.String()).To(Equal("2000Gi"))
}
