package node

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	preInitTerminationGracePeriod = int64(120)
	preInitPodHostname            = "seid"
)

func preInitJobName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("%s-pre-init", node.Name)
}

func generatePreInitJob(node *seiv1alpha1.SeiNode, platform PlatformConfig) *batchv1.Job {
	labels := preInitLabelsForNode(node)
	snap := snapshotSourceFor(node)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      preInitJobName(node),
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
				Spec: buildPreInitPodSpec(node, snap, platform),
			},
		},
	}
}

// generatePreInitService creates a headless Service that enables pod DNS
// resolution for the pre-init Job. The Job pod uses hostname/subdomain to
// register as <hostname>.<service-name>.<namespace>.svc.cluster.local.
func generatePreInitService(node *seiv1alpha1.SeiNode) *corev1.Service {
	labels := preInitLabelsForNode(node)
	port := sidecarPort(node)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      preInitJobName(node),
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

// preInitSidecarURL returns the in-cluster DNS URL for the pre-init Job's sidecar.
func preInitSidecarURL(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("http://%s.%s.%s.svc.cluster.local:%d",
		preInitPodHostname, preInitJobName(node), node.Namespace, sidecarPort(node))
}

// preInitWaitCommand returns a shell command that waits for the sidecar
// healthz to return 200 and then runs "seid start --home /sei".
// Unlike sidecarWaitCommand, it does NOT use the node's custom entrypoint
// because the pre-init Job runs the bootstrap image which may not support
// custom flags (e.g. --skip-app-hash-validation).
//
// seid is run as a child process (not exec'd) so the shell can exit 0
// regardless of how seid terminates. The sidecar's await-condition task
// sends SIGTERM directly to the seid process when the target height is
// reached; the trailing "exit 0" ensures the shell reports success even
// though seid exits with code 143 (SIGTERM). The trap handles the case
// where Kubernetes sends SIGTERM to the shell (PID 1) during pod
// termination.
func preInitWaitCommand(port int32) (command []string, args []string) {
	script := fmt.Sprintf(
		`echo "waiting for sidecar to become ready..."; `+
			`while true; do `+
			`{ exec 3<>/dev/tcp/localhost/%d; } 2>/dev/null && `+
			`printf "GET /v0/healthz HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3 && `+
			`head -1 <&3 | grep -q "200" && break; `+
			`exec 3>&-; sleep 5; done; `+
			`exec 3>&-; `+
			`echo "sidecar ready, starting seid"; `+
			`trap 'exit 0' TERM; `+
			`seid start --home %s & wait $!; exit 0`,
		port, dataDir,
	)
	return []string{"/bin/bash", "-c"}, []string{script}
}

func buildPreInitPodSpec(node *seiv1alpha1.SeiNode, snap *seiv1alpha1.SnapshotSource, platform PlatformConfig) corev1.PodSpec {
	serviceName := preInitJobName(node)

	dataVolume := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: nodeDataPVCClaimName(node),
			},
		},
	}

	port := sidecarPort(node)
	sidecar := corev1.Container{
		Name:          "sei-sidecar",
		Image:         sidecarImage(node),
		Command:       []string{"seictl", "serve"},
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Env: []corev1.EnvVar{
			{Name: "SEI_CHAIN_ID", Value: node.Spec.ChainID},
			{Name: "SEI_SIDECAR_PORT", Value: fmt.Sprintf("%d", port)},
			{Name: "SEI_HOME", Value: dataDir},
		},
		Ports: []corev1.ContainerPort{
			{Name: "sidecar", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataDir},
		},
	}
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Resources != nil {
		sidecar.Resources = *node.Spec.Sidecar.Resources
	}

	bootstrapImage := node.Spec.Image
	if snap != nil && snap.BootstrapImage != "" {
		bootstrapImage = snap.BootstrapImage
	}

	seidCmd, seidArgs := preInitWaitCommand(sidecarPort(node))
	seidContainer := corev1.Container{
		Name:    "seid",
		Image:   bootstrapImage,
		Command: seidCmd,
		Args:    seidArgs,
		Env: []corev1.EnvVar{
			{Name: "TMPDIR", Value: dataDir + "/tmp"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataDir},
		},
		Resources: defaultResourcesForMode(nodeMode(node), platform),
	}

	seidInit := buildSeidInitContainer(node)
	seidInit.Image = bootstrapImage

	return corev1.PodSpec{
		Hostname:                      preInitPodHostname,
		Subdomain:                     serviceName,
		ServiceAccountName:            platform.ServiceAccount,
		ShareProcessNamespace:         ptr.To(true),
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: ptr.To(preInitTerminationGracePeriod),
		Tolerations: []corev1.Toleration{
			{Key: platform.TolerationKey, Value: platform.TolerationVal, Effect: corev1.TaintEffectNoSchedule},
		},
		Volumes:        []corev1.Volume{dataVolume},
		InitContainers: []corev1.Container{seidInit, sidecar},
		Containers:     []corev1.Container{seidContainer},
	}
}
