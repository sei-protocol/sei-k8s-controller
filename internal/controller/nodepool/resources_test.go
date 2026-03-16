package nodepool

import (
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const genesisDataVolumeName = "genesis-data"

func newSeiNodePool(name, namespace string, nodeCount int32) *seiv1alpha1.SeiNodePool {
	return &seiv1alpha1.SeiNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodePoolSpec{
			ChainID: "sei-test",
			NodeConfiguration: seiv1alpha1.NodeConfiguration{
				NodeCount: nodeCount,
				Image:     "ghcr.io/sei-protocol/seid:latest",
				Entrypoint: seiv1alpha1.EntrypointConfig{
					Command: []string{"seid"},
					Args:    []string{"start"},
				},
			},
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

func TestGenerateSeiNode(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("my-net", "default", 3)

	node := generateSeiNode(sn, 1)

	g.Expect(node.Name).To(Equal("my-net-1"))
	g.Expect(node.Namespace).To(Equal("default"))
	g.Expect(node.Labels).To(HaveKeyWithValue(nodepoolLabel, "my-net"))
	g.Expect(node.Spec.ChainID).To(Equal("sei-test"))
	g.Expect(node.Spec.Image).To(Equal("ghcr.io/sei-protocol/seid:latest"))
	g.Expect(node.Spec.Entrypoint).NotTo(BeNil())
	g.Expect(node.Spec.Entrypoint.Command).To(Equal([]string{"seid"}))
	g.Expect(node.Spec.Entrypoint.Args).To(Equal([]string{"start"}))
	g.Expect(node.Spec.Genesis.PVC).NotTo(BeNil())
	g.Expect(node.Spec.Genesis.PVC.DataPVC).To(Equal("data-my-net-1"))
	g.Expect(node.Spec.FullNode).To(BeNil())
}

func TestGenerateSeiNode_RetainOnDeletePropagated(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("my-net", "default", 1)
	sn.Spec.Storage.RetainOnDelete = true

	node := generateSeiNode(sn, 0)
	g.Expect(node.Spec.Storage.RetainOnDelete).To(BeTrue())
}

func TestGenerateDataPVC(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "ns1", 2)

	pvc := generateDataPVC(sn, 0)

	g.Expect(pvc.Name).To(Equal("data-testnet-0"))
	g.Expect(pvc.Namespace).To(Equal("ns1"))
	g.Expect(pvc.Labels).To(HaveKeyWithValue(nodepoolLabel, "testnet"))
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))

	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	g.Expect(storage.String()).To(Equal("1000Gi"))
}

func TestGenerateDataPVC_OrdinalInName(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("net", "default", 3)

	for _, ordinal := range []int{0, 1, 2} {
		pvc := generateDataPVC(sn, ordinal)
		g.Expect(pvc.Name).To(Equal(dataPVCName(sn, ordinal)))
	}
}

func TestGenerateGenesisPVC(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "ns1", 1)

	pvc := generateGenesisPVC(sn)

	g.Expect(pvc.Name).To(Equal("testnet-genesis-data"))
	g.Expect(pvc.Namespace).To(Equal("ns1"))
	g.Expect(pvc.Labels).To(HaveKeyWithValue(nodepoolLabel, "testnet"))
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteMany))
	g.Expect(pvc.Spec.StorageClassName).NotTo(BeNil())
	g.Expect(*pvc.Spec.StorageClassName).To(Equal(efsStorageClass))
}

func TestGenerateGenesisJob(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "default", 3)

	job := generateGenesisJob(sn)

	g.Expect(job.Name).To(Equal("testnet-genesis"))
	g.Expect(job.Namespace).To(Equal("default"))
	g.Expect(job.Labels).To(HaveKeyWithValue(nodepoolLabel, "testnet"))
	g.Expect(*job.Spec.BackoffLimit).To(Equal(int32(3)))

	containers := job.Spec.Template.Spec.Containers
	g.Expect(containers).To(HaveLen(1))
	c := containers[0]
	g.Expect(c.Name).To(Equal("genesis"))
	g.Expect(c.Image).To(Equal("ghcr.io/sei-protocol/seid:latest"))
	g.Expect(c.Command).To(Equal([]string{"sh", "/scripts/generate.sh"}))

	g.Expect(envValue(c.Env, "NUM_NODES")).To(Equal("3"))
	g.Expect(envValue(c.Env, "CHAIN_ID")).To(Equal("sei-test"))
	g.Expect(envValue(c.Env, "NODES_DIR")).To(Equal(genesisNodesDir(sn)))
}

func TestGenerateGenesisJob_MountsGenesisAndScripts(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "default", 1)
	job := generateGenesisJob(sn)

	c := job.Spec.Template.Spec.Containers[0]

	var genesisMount, scriptsMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		switch c.VolumeMounts[i].Name {
		case genesisDataVolumeName:
			genesisMount = &c.VolumeMounts[i]
		case "scripts":
			scriptsMount = &c.VolumeMounts[i]
		}
	}
	g.Expect(genesisMount).NotTo(BeNil())
	g.Expect(scriptsMount).NotTo(BeNil())
	g.Expect(scriptsMount.ReadOnly).To(BeTrue())
	g.Expect(scriptsMount.MountPath).To(Equal("/scripts"))

	var scriptVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "scripts" {
			scriptVol = &job.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	g.Expect(scriptVol).NotTo(BeNil())
	g.Expect(scriptVol.ConfigMap.Name).To(Equal("testnet-genesis-script"))
}

func TestGeneratePrepJob(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "default", 1)

	job := generatePrepJob(sn, 2)

	g.Expect(job.Name).To(Equal("testnet-prep-2"))
	g.Expect(job.Namespace).To(Equal("default"))
	g.Expect(*job.Spec.BackoffLimit).To(Equal(int32(3)))

	c := job.Spec.Template.Spec.Containers[0]
	g.Expect(c.Name).To(Equal("genesis-copy"))
	g.Expect(c.Image).To(Equal("ghcr.io/sei-protocol/seid:latest"))
	g.Expect(envValue(c.Env, "NODE_INDEX")).To(Equal("2"))
}

func TestGeneratePrepJob_GenesisDataMountReadOnly(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "default", 1)
	job := generatePrepJob(sn, 0)

	c := job.Spec.Template.Spec.Containers[0]

	var genesisMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == genesisDataVolumeName {
			genesisMount = &c.VolumeMounts[i]
			break
		}
	}
	g.Expect(genesisMount).NotTo(BeNil())
	g.Expect(genesisMount.ReadOnly).To(BeTrue())
}

func TestGeneratePrepJob_VolumesCorrect(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "default", 1)
	job := generatePrepJob(sn, 0)

	volumes := job.Spec.Template.Spec.Volumes
	var datavol, genesisvol *corev1.Volume
	for i := range volumes {
		switch volumes[i].Name {
		case "data":
			datavol = &volumes[i]
		case genesisDataVolumeName:
			genesisvol = &volumes[i]
		}
	}
	g.Expect(datavol).NotTo(BeNil())
	g.Expect(datavol.PersistentVolumeClaim.ClaimName).To(Equal(dataPVCName(sn, 0)))

	g.Expect(genesisvol).NotTo(BeNil())
	g.Expect(genesisvol.PersistentVolumeClaim.ClaimName).To(Equal(genesisPVCName(sn)))
	g.Expect(genesisvol.PersistentVolumeClaim.ReadOnly).To(BeTrue())
}

func TestGenerateGenesisScriptConfigMap(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("my-net", "ns1", 1)

	cm := generateGenesisScriptConfigMap(sn)

	g.Expect(cm.Name).To(Equal("my-net-genesis-script"))
	g.Expect(cm.Namespace).To(Equal("ns1"))
	g.Expect(cm.Labels).To(HaveKeyWithValue(nodepoolLabel, "my-net"))
	g.Expect(cm.Data).To(HaveKey("generate.sh"))
	g.Expect(cm.Data["generate.sh"]).NotTo(BeEmpty())
}

func TestGenesisScript_ContainsExpectedVariables(t *testing.T) {
	g := NewWithT(t)
	g.Expect(genesisScript).To(ContainSubstring("NUM_NODES"))
	g.Expect(genesisScript).To(ContainSubstring("CHAIN_ID"))
}

func TestGenerateNetworkPolicy(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "ns1", 1)

	np := generateNetworkPolicy(sn)

	g.Expect(np.Name).To(Equal("testnet"))
	g.Expect(np.Namespace).To(Equal("ns1"))
	g.Expect(np.Labels).To(HaveKeyWithValue(nodepoolLabel, "testnet"))
}

func TestGenerateNetworkPolicy_IngressOnlyFromSamePool(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("testnet", "default", 1)

	np := generateNetworkPolicy(sn)

	g.Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(
		HaveKeyWithValue(nodepoolLabel, "testnet"),
	)
}

func TestNetworkPolicyPorts_FivePorts(t *testing.T) {
	g := NewWithT(t)
	ports := networkPolicyPorts(corev1.ProtocolTCP)
	g.Expect(ports).To(HaveLen(5))

	nums := make([]int32, len(ports))
	for i, p := range ports {
		nums[i] = p.Port.IntVal
	}
	g.Expect(nums).To(ConsistOf(int32(8545), int32(8546), int32(26656), int32(26657), int32(26660)))
}

func TestIsJobComplete(t *testing.T) {
	g := NewWithT(t)

	pending := &batchv1.Job{}
	g.Expect(isJobComplete(pending)).To(BeFalse())

	complete := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(isJobComplete(complete)).To(BeTrue())
}

func TestIsJobFailed(t *testing.T) {
	g := NewWithT(t)

	pending := &batchv1.Job{}
	g.Expect(isJobFailed(pending)).To(BeFalse())

	failed := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "backoff limit exceeded"},
			},
		},
	}
	g.Expect(isJobFailed(failed)).To(BeTrue())
}

func TestNodepoolPhase(t *testing.T) {
	tests := []struct {
		name       string
		ready      int32
		total      int32
		nodePhases []string
		wantPhase  string
	}{
		{
			name:      "no nodes configured",
			total:     0,
			wantPhase: "Pending",
		},
		{
			name:       "all nodes ready",
			ready:      2,
			total:      2,
			nodePhases: []string{"Running", "Running"},
			wantPhase:  "Running",
		},
		{
			name:       "partial ready — no failures",
			ready:      1,
			total:      2,
			nodePhases: []string{"Running", "Pending"},
			wantPhase:  "Pending",
		},
		{
			name:       "any node failed",
			ready:      1,
			total:      2,
			nodePhases: []string{"Running", "Failed"},
			wantPhase:  "Failed",
		},
		{
			name:       "no nodes ready",
			ready:      0,
			total:      2,
			nodePhases: []string{"Pending", "Pending"},
			wantPhase:  "Pending",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			nodeList := &seiv1alpha1.SeiNodeList{}
			for _, phase := range tc.nodePhases {
				nodeList.Items = append(nodeList.Items, seiv1alpha1.SeiNode{
					Status: seiv1alpha1.SeiNodeStatus{Phase: phase},
				})
			}
			g.Expect(nodepoolPhase(tc.ready, tc.total, nodeList)).To(Equal(tc.wantPhase))
		})
	}
}

func TestApplyStatusConditions_AllReady(t *testing.T) {
	g := NewWithT(t)
	sn := &seiv1alpha1.SeiNodePool{}

	applyStatusConditions(sn, 3, 3)

	ready := findCondition(sn.Status.Conditions, ConditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal(ReasonAllPodsReady))

	prog := findCondition(sn.Status.Conditions, ConditionTypeProgressing)
	g.Expect(prog).NotTo(BeNil())
	g.Expect(prog.Status).To(Equal(metav1.ConditionFalse))
}

func TestApplyStatusConditions_PartialReady(t *testing.T) {
	g := NewWithT(t)
	sn := &seiv1alpha1.SeiNodePool{}

	applyStatusConditions(sn, 1, 3)

	ready := findCondition(sn.Status.Conditions, ConditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))

	degraded := findCondition(sn.Status.Conditions, ConditionTypeDegraded)
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(degraded.Message).To(ContainSubstring("1/3"))
}

func TestApplyStatusConditions_NoneReady(t *testing.T) {
	g := NewWithT(t)
	sn := &seiv1alpha1.SeiNodePool{}

	applyStatusConditions(sn, 0, 2)

	ready := findCondition(sn.Status.Conditions, ConditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))

	prog := findCondition(sn.Status.Conditions, ConditionTypeProgressing)
	g.Expect(prog).NotTo(BeNil())
	g.Expect(prog.Status).To(Equal(metav1.ConditionTrue))
}

func TestSetCondition_SetsObservedGeneration(t *testing.T) {
	g := NewWithT(t)
	sn := &seiv1alpha1.SeiNodePool{}
	sn.Generation = 42

	setCondition(sn, ConditionTypeReady, metav1.ConditionTrue, ReasonAllPodsReady, "ok")

	cond := findCondition(sn.Status.Conditions, ConditionTypeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(42)))
}

func TestSetCondition_UpdatesExistingInPlace(t *testing.T) {
	g := NewWithT(t)
	sn := &seiv1alpha1.SeiNodePool{}

	setCondition(sn, ConditionTypeReady, metav1.ConditionFalse, ReasonPodsNotReady, "waiting")
	setCondition(sn, ConditionTypeReady, metav1.ConditionTrue, ReasonAllPodsReady, "done")

	count := 0
	for _, c := range sn.Status.Conditions {
		if c.Type == ConditionTypeReady {
			count++
		}
	}
	g.Expect(count).To(Equal(1))

	cond := findCondition(sn.Status.Conditions, ConditionTypeReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonAllPodsReady))
}

func TestNamingHelpers(t *testing.T) {
	g := NewWithT(t)
	sn := newSeiNodePool("mynet", "default", 1)

	g.Expect(dataPVCName(sn, 0)).To(Equal("data-mynet-0"))
	g.Expect(dataPVCName(sn, 3)).To(Equal("data-mynet-3"))
	g.Expect(seiNodeName(sn, 0)).To(Equal("mynet-0"))
	g.Expect(prepJobName(sn, 1)).To(Equal("mynet-prep-1"))
	g.Expect(genesisPVCName(sn)).To(Equal("mynet-genesis-data"))
}

func envValue(envs []corev1.EnvVar, name string) string {
	for _, e := range envs {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
