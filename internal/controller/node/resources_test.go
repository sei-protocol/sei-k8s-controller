package node

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
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
			Genesis: seiv1alpha1.GenesisConfiguration{
				ChainID: "sei-test",
				PVC:     &seiv1alpha1.GenesisPVCSource{DataPVC: "data-mynet-0"},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

func newSnapshotNode(name, namespace string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "sei-test",
			Image:   "ghcr.io/sei-protocol/seid:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{ChainID: "sei-test"},
			Snapshot: &seiv1alpha1.SnapshotSource{
				Region: "eu-central-1",
				Bucket: seiv1alpha1.BucketSnapshot{
					URI: "s3://sei-snapshots/pacific-1/",
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
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

// --- StatefulSet generation ---

func TestGenerateNodeStatefulSet_BasicFields(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")

	sts := generateNodeStatefulSet(node)

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

	sts := generateNodeStatefulSet(node)
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

	spec := buildNodePodSpec(node)

	g.Expect(spec.ServiceAccountName).To(Equal(nodeServiceAccount))
	g.Expect(spec.Volumes).To(HaveLen(1))
	g.Expect(spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-mynet-0"))
}

func TestBuildNodePodSpec_Snapshot_MountsNodePVC(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	spec := buildNodePodSpec(node)

	g.Expect(spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-snap-0"))
}

func TestBuildNodePodSpec_SharedPIDNamespace(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")

	sts := generateNodeStatefulSet(node)

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

	g.Expect(sidecarPort(node)).To(Equal(defaultSidecarPort))
}

func TestSidecarPort_DefaultWhenNil(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "default")
	node.Spec.Sidecar = nil

	g.Expect(sidecarPort(node)).To(Equal(defaultSidecarPort))
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

	sts := generateNodeStatefulSet(node)
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal(defaultSidecarImage))
}

func TestSidecarContainer_CustomImage(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Image: "custom/seictl:v3", Port: 7777}

	sts := generateNodeStatefulSet(node)
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Image).To(Equal("custom/seictl:v3"))
}

func TestSidecarContainer_RestartPolicyAlways(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node)
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc).NotTo(BeNil())
	g.Expect(sc.RestartPolicy).NotTo(BeNil())
	g.Expect(*sc.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
}

func TestSidecarContainer_EnvVars(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node)
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(envValue(sc.Env, "SEI_SIDECAR_PORT")).To(Equal("7777"))
	g.Expect(envValue(sc.Env, "SEI_HOME")).To(Equal(dataDir))
}

func TestSidecarContainer_DataVolumeMount(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node)
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.VolumeMounts).To(HaveLen(1))
	g.Expect(sc.VolumeMounts[0].MountPath).To(Equal(dataDir))
}

func TestSidecarContainer_CustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := generateNodeStatefulSet(node)
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

	sts := generateNodeStatefulSet(node)
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests.Cpu().String()).To(Equal("250m"))
	g.Expect(sc.Resources.Requests.Memory().String()).To(Equal("128Mi"))
	g.Expect(sc.Resources.Limits.Cpu().String()).To(Equal("500m"))
	g.Expect(sc.Resources.Limits.Memory().String()).To(Equal("256Mi"))
}

func TestSidecarContainer_NoResources_DefaultsToEmpty(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node)
	sc := findInitContainer(sts.Spec.Template.Spec.InitContainers, "sei-sidecar")

	g.Expect(sc.Resources.Requests).To(BeNil())
	g.Expect(sc.Resources.Limits).To(BeNil())
}

// --- Sidecar main container (seid with wait wrapper) ---

func TestSidecarMainContainer_StartupProbeTargetsHealthz(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node)
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid).NotTo(BeNil())
	probe := seid.StartupProbe
	g.Expect(probe).NotTo(BeNil())
	g.Expect(probe.HTTPGet).NotTo(BeNil())
	g.Expect(probe.HTTPGet.Path).To(Equal("/healthz"))
	g.Expect(probe.HTTPGet.Port.IntValue()).To(Equal(7777))
	g.Expect(probe.InitialDelaySeconds).To(Equal(int32(5)))
	g.Expect(probe.PeriodSeconds).To(Equal(int32(5)))
	g.Expect(probe.FailureThreshold).To(Equal(int32(86400)))
}

func TestSidecarMainContainer_StartupProbeUsesCustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := generateNodeStatefulSet(node)
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(9999))
}

func TestSidecarMainContainer_WaitWrapper_PollsHealthzBeforeExec(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := generateNodeStatefulSet(node)
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Command).To(Equal([]string{"/bin/bash", "-c"}))
	g.Expect(seid.Args).To(HaveLen(1))
	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/7777"))
	g.Expect(seid.Args[0]).To(ContainSubstring("exec seid"))
}

func TestSidecarMainContainer_WaitWrapper_IncludesEntrypointArgs(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")
	node.Spec.Entrypoint = &seiv1alpha1.EntrypointConfig{
		Command: []string{"seid"},
		Args:    []string{"start", "--home", "/sei"},
	}

	sts := generateNodeStatefulSet(node)
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Args[0]).To(ContainSubstring(`exec seid "start" "--home" "/sei"`))
}

func TestSidecarMainContainer_WaitWrapper_NoEntrypoint_DefaultsSeid(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")

	sts := generateNodeStatefulSet(node)
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Command).To(Equal([]string{"/bin/bash", "-c"}))
	g.Expect(seid.Args[0]).To(HaveSuffix("exec seid"))
}

func TestSidecarMainContainer_WaitWrapper_UsesCustomPort(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("sc-0", "default")
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Port: 9999}

	sts := generateNodeStatefulSet(node)
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")

	g.Expect(seid.Args[0]).To(ContainSubstring("/dev/tcp/localhost/9999"))
}

// --- Genesis mode specifics ---

func TestGenesisMode_SidecarPresent(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := generateNodeStatefulSet(node)
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(initContainers).To(HaveLen(2))
	g.Expect(initContainers[0].Name).To(Equal("seid-init"))
	g.Expect(initContainers[1].Name).To(Equal("sei-sidecar"))
}

func TestGenesisMode_NoSnapshotRestoreInitContainer(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := generateNodeStatefulSet(node)
	initContainers := sts.Spec.Template.Spec.InitContainers

	g.Expect(findInitContainer(initContainers, "snapshot-restore")).To(BeNil())
}

func TestGenesisMode_SharedPIDNamespace(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("gen-0", "default")

	sts := generateNodeStatefulSet(node)

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
	g.Expect(svc.Spec.Ports).To(HaveLen(6))
}

func TestServicePorts_SixExpectedPorts(t *testing.T) {
	g := NewWithT(t)
	ports := servicePorts()
	g.Expect(ports).To(HaveLen(6))
	portNums := make([]int32, len(ports))
	for i, p := range ports {
		portNums[i] = p.Port
	}
	g.Expect(portNums).To(ConsistOf(int32(8545), int32(8546), int32(9090), int32(26656), int32(26657), int32(26660)))
}

func TestContainerPorts_SixExpectedPorts(t *testing.T) {
	g := NewWithT(t)
	ports := containerPorts()
	g.Expect(ports).To(HaveLen(6))
}

// --- PVC generation ---

func TestGenerateNodeDataPVC(t *testing.T) {
	g := NewWithT(t)
	node := newSnapshotNode("snap-0", "ns1")

	pvc := generateNodeDataPVC(node)

	g.Expect(pvc.Name).To(Equal("data-snap-0"))
	g.Expect(pvc.Namespace).To(Equal("ns1"))
	g.Expect(pvc.Labels).To(HaveKeyWithValue(nodeLabel, "snap-0"))
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))

	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	g.Expect(storage.String()).To(Equal("1000Gi"))
}

// --- S3 URI parsing ---

func TestParseS3URI(t *testing.T) {
	tests := []struct {
		uri        string
		wantBucket string
		wantPrefix string
	}{
		{"s3://my-bucket/path/to/prefix", "my-bucket", "path/to/prefix"},
		{"s3://my-bucket/", "my-bucket", ""},
		{"s3://my-bucket", "my-bucket", ""},
		{"not-a-uri", "not-a-uri", ""},
	}

	for _, tc := range tests {
		t.Run(tc.uri, func(t *testing.T) {
			g := NewWithT(t)
			bucket, prefix := parseS3URI(tc.uri)
			g.Expect(bucket).To(Equal(tc.wantBucket))
			g.Expect(prefix).To(Equal(tc.wantPrefix))
		})
	}
}

// --- Conditions ---

func TestSetNodeCondition_ObservedGeneration(t *testing.T) {
	g := NewWithT(t)
	node := &seiv1alpha1.SeiNode{}
	node.Generation = 7

	setNodeCondition(node, ConditionTypeReady, metav1.ConditionTrue, ReasonAllPodsReady, "ok")

	cond := findCondition(node.Status.Conditions, ConditionTypeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))
}

func TestSetNodeCondition_UpdatesExistingInPlace(t *testing.T) {
	g := NewWithT(t)
	node := &seiv1alpha1.SeiNode{}

	setNodeCondition(node, ConditionTypeReady, metav1.ConditionFalse, ReasonPodsNotReady, "waiting")
	setNodeCondition(node, ConditionTypeReady, metav1.ConditionTrue, ReasonAllPodsReady, "done")

	count := 0
	for _, c := range node.Status.Conditions {
		if c.Type == ConditionTypeReady {
			count++
		}
	}
	g.Expect(count).To(Equal(1))
	cond := findCondition(node.Status.Conditions, ConditionTypeReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// --- Genesis configuration ---

func TestGenesisConfiguration_PVCOnly(t *testing.T) {
	g := NewWithT(t)
	gc := seiv1alpha1.GenesisConfiguration{
		ChainID: "sei-test",
		PVC:     &seiv1alpha1.GenesisPVCSource{DataPVC: "data-0"},
	}
	count := genesisSourceCount(gc)
	g.Expect(count).To(Equal(1), "PVC-only should have exactly one source set")
}

func TestGenesisConfiguration_S3Only(t *testing.T) {
	g := NewWithT(t)
	gc := seiv1alpha1.GenesisConfiguration{
		ChainID: "sei-test",
		S3:      &seiv1alpha1.GenesisS3Source{URI: "s3://bucket/genesis.json", Region: "us-east-1"},
	}
	count := genesisSourceCount(gc)
	g.Expect(count).To(Equal(1), "S3-only should have exactly one source set")
}

func TestGenesisConfiguration_RejectsBoth(t *testing.T) {
	g := NewWithT(t)
	gc := seiv1alpha1.GenesisConfiguration{
		ChainID: "sei-test",
		PVC:     &seiv1alpha1.GenesisPVCSource{DataPVC: "data-0"},
		S3:      &seiv1alpha1.GenesisS3Source{URI: "s3://bucket/genesis.json", Region: "us-east-1"},
	}
	count := genesisSourceCount(gc)
	g.Expect(count).To(Equal(2), "both PVC and S3 set should violate at-most-one-of")
}

func TestGenesisConfiguration_AllowsNeither(t *testing.T) {
	g := NewWithT(t)
	gc := seiv1alpha1.GenesisConfiguration{ChainID: "sei-test"}
	count := genesisSourceCount(gc)
	g.Expect(count).To(Equal(0), "neither PVC nor S3 is valid (uses default genesis)")
}

// genesisSourceCount counts how many genesis source fields are set,
// mirroring the CEL rule: (has(pvc)?1:0) + (has(s3)?1:0) <= 1
func genesisSourceCount(gc seiv1alpha1.GenesisConfiguration) int {
	count := 0
	if gc.PVC != nil {
		count++
	}
	if gc.S3 != nil {
		count++
	}
	return count
}
