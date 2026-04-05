package task

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

const (
	bootstrapTerminationGracePeriod = int64(120)
	bootstrapDataDir                = platform.DataDir
	bootstrapDefaultSidecarImage    = platform.DefaultSidecarImage
	bootstrapNodeLabel              = "sei.io/node"
	bootstrapComponentLabel         = "sei.io/component"
)

// BootstrapJobName returns the bootstrap Job name for a node.
func BootstrapJobName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("%s-bootstrap", node.Name)
}

// BootstrapLabels returns labels for bootstrap Job resources.
func BootstrapLabels(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		bootstrapNodeLabel:      node.Name,
		bootstrapComponentLabel: "bootstrap",
	}
}

// GenerateBootstrapJob creates the batch Job that runs seid with --halt-height
// to populate a PVC before the StatefulSet takes over.
func GenerateBootstrapJob(
	node *seiv1alpha1.SeiNode,
	snap *seiv1alpha1.SnapshotSource,
	platformCfg platform.Config,
) (*batchv1.Job, error) {
	if snap == nil || snap.S3 == nil {
		return nil, fmt.Errorf("bootstrap job requires an S3 snapshot source (node %s/%s)", node.Namespace, node.Name)
	}
	labels := BootstrapLabels(node)
	podSpec := buildBootstrapPodSpec(node, snap, platformCfg)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BootstrapJobName(node),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"karpenter.sh/do-not-disrupt": "true",
					},
				},
				Spec: podSpec,
			},
		},
	}, nil
}

// GenerateBootstrapService creates a headless Service that provides stable DNS
// for the bootstrap Job pod. The pod registers as
// <hostname>.<service-name>.<namespace>.svc.cluster.local.
//
// The Service deliberately uses node.Name — the same name as the production
// headless Service created by the StatefulSet reconciler. The bootstrap
// teardown task deletes this Service before the StatefulSet is created,
// so the sidecar URL (node-0.node.ns.svc.cluster.local) is consistent
// across both phases without overlap.
func GenerateBootstrapService(node *seiv1alpha1.SeiNode) *corev1.Service {
	labels := BootstrapLabels(node)
	port := bootstrapSidecarPort(node)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 labels,
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "sidecar", Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func buildBootstrapPodSpec(node *seiv1alpha1.SeiNode, snap *seiv1alpha1.SnapshotSource, platformCfg platform.Config) corev1.PodSpec {
	serviceName := node.Name

	dataVolume := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: bootstrapPVCClaimName(node),
			},
		},
	}

	port := bootstrapSidecarPort(node)
	sidecar := corev1.Container{
		Name:          "sei-sidecar",
		Image:         bootstrapSidecarImage(node),
		Command:       []string{"seictl", "serve"},
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Env: []corev1.EnvVar{
			{Name: "SEI_CHAIN_ID", Value: node.Spec.ChainID},
			{Name: "SEI_SIDECAR_PORT", Value: fmt.Sprintf("%d", port)},
			{Name: "SEI_HOME", Value: bootstrapDataDir},
			{Name: "SEI_GENESIS_BUCKET", Value: platformCfg.GenesisBucket},
			{Name: "SEI_GENESIS_REGION", Value: platformCfg.GenesisRegion},
			{Name: "SEI_SNAPSHOT_BUCKET", Value: platformCfg.SnapshotBucket},
			{Name: "SEI_SNAPSHOT_REGION", Value: platformCfg.SnapshotRegion},
		},
		Ports: []corev1.ContainerPort{
			{Name: "sidecar", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
	}
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Resources != nil {
		sidecar.Resources = *node.Spec.Sidecar.Resources
	}

	bootstrapImage := node.Spec.Image
	if snap != nil && snap.BootstrapImage != "" {
		bootstrapImage = snap.BootstrapImage
	}

	haltHeight := snap.S3.TargetHeight
	seidCmd, seidArgs := bootstrapWaitCommand(bootstrapSidecarPort(node), haltHeight)
	seidContainer := corev1.Container{
		Name:    "seid",
		Image:   bootstrapImage,
		Command: seidCmd,
		Args:    seidArgs,
		Env: []corev1.EnvVar{
			{Name: "TMPDIR", Value: bootstrapDataDir + "/tmp"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
		Resources: bootstrapResourcesForMode(bootstrapNodeMode(node), platformCfg),
	}

	seidInit := bootstrapSeidInitContainer(node)
	seidInit.Image = bootstrapImage

	return corev1.PodSpec{
		Hostname:                      fmt.Sprintf("%s-0", node.Name),
		Subdomain:                     serviceName,
		ServiceAccountName:            platformCfg.ServiceAccount,
		ShareProcessNamespace:         ptr.To(true),
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: ptr.To(bootstrapTerminationGracePeriod),
		Tolerations: []corev1.Toleration{
			{Key: platformCfg.TolerationKey, Value: platformCfg.TolerationVal, Effect: corev1.TaintEffectNoSchedule},
		},
		Volumes:        []corev1.Volume{dataVolume},
		InitContainers: []corev1.Container{seidInit, sidecar},
		Containers:     []corev1.Container{seidContainer},
	}
}

// bootstrapWaitCommand returns a shell command that waits for the sidecar
// to report healthy and then runs seid with --halt-height. Polls /v0/healthz
// which returns 503 until the mark-ready task completes, ensuring all
// bootstrap sidecar tasks (snapshot-restore, configure-genesis, config-apply,
// discover-peers, config-validate) have finished before seid starts.
//
// Cosmos SDK's halt-height sends SIGINT to itself after committing the
// target block, producing exit code 130. The wrapper treats 130 as
// success so the Job completes cleanly.
func bootstrapWaitCommand(port int32, haltHeight int64) (command []string, args []string) {
	script := fmt.Sprintf(
		`echo "waiting for sidecar bootstrap tasks to complete..."; `+
			`while true; do `+
			`wget -q -O /dev/null http://localhost:%d/v0/healthz && break; `+
			`sleep 5; done; `+
			`echo "sidecar healthy, starting seid with halt-height %d"; `+
			`seid start --home %s --halt-height %d; `+
			`rc=$?; if [ $rc -eq 130 ]; then echo "seid halted at target height (exit 130), treating as success"; exit 0; fi; exit $rc`,
		port, haltHeight, bootstrapDataDir, haltHeight,
	)
	return []string{"/bin/sh", "-c"}, []string{script}
}

func bootstrapSeidInitContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	script := fmt.Sprintf(
		`if [ -f %s/config/genesis.json ]; then echo "data directory already initialized, skipping seid init"; else seid init %s --chain-id %s --home %s --overwrite; fi && mkdir -p %s/tmp`,
		bootstrapDataDir, node.Spec.ChainID, node.Spec.ChainID, bootstrapDataDir, bootstrapDataDir,
	)
	return corev1.Container{
		Name:  "seid-init",
		Image: node.Spec.Image,
		Command: []string{
			"/bin/sh", "-c", script,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
	}
}

func bootstrapSidecarImage(node *seiv1alpha1.SeiNode) string {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Image != "" {
		return node.Spec.Sidecar.Image
	}
	return bootstrapDefaultSidecarImage
}

func bootstrapSidecarPort(node *seiv1alpha1.SeiNode) int32 {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Port != 0 {
		return node.Spec.Sidecar.Port
	}
	return seiconfig.PortSidecar
}

func bootstrapPVCClaimName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}

// bootstrapNodeMode determines the sei-config mode string from the node spec.
func bootstrapNodeMode(node *seiv1alpha1.SeiNode) string {
	switch {
	case node.Spec.Archive != nil:
		return string(seiconfig.ModeArchive)
	case node.Spec.Validator != nil:
		return string(seiconfig.ModeValidator)
	case node.Spec.Replayer != nil:
		return string(seiconfig.ModeFull)
	default:
		return string(seiconfig.ModeFull)
	}
}

func bootstrapResourcesForMode(mode string, platformCfg platform.Config) corev1.ResourceRequirements {
	switch mode {
	case string(seiconfig.ModeArchive):
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(platformCfg.ResourceCPUArchive),
				corev1.ResourceMemory: resource.MustParse(platformCfg.ResourceMemArchive),
			},
		}
	default:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(platformCfg.ResourceCPUDefault),
				corev1.ResourceMemory: resource.MustParse(platformCfg.ResourceMemDefault),
			},
		}
	}
}

// IsJobFailed returns true if a Job has a Failed condition.
func IsJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// IsJobComplete returns true if a Job has a Complete condition.
func IsJobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// JobFailureReason extracts a human-readable failure reason from a Job's conditions.
func JobFailureReason(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Message != "" {
			return c.Message
		}
	}
	return "bootstrap job failed"
}
