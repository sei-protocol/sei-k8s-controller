package bootstrap

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

const (
	bootstrapTerminationGracePeriod = int64(120)
	bootstrapDataDir                = platform.DataDir
	bootstrapDefaultSidecarImage    = platform.DefaultSidecarImage
	bootstrapNodeLabel              = "sei.io/node"
	bootstrapComponentLabel         = "sei.io/component"
)

// PodInputs is the resolved pod-shape contract for a bootstrap Job
// and its sibling headless Service.
type PodInputs struct {
	// Name is the resource name root used for both the Job and the Service.
	// Pod hostname becomes "<Name>-0" so the in-cluster sidecar URL is
	// "<Name>-0.<Name>.<Namespace>.svc.cluster.local". The data PVC the pod
	// mounts at /sei is "data-<Name>".
	Name string

	Namespace string

	// ChainID is what the seid-init container uses to materialise the data
	// directory and what the sidecar advertises as its SEI_CHAIN_ID.
	ChainID string

	// Image is the seid image used by both the seid-init container and the
	// main "seid" container that runs `seid start --halt-height N`.
	Image string

	SidecarImage     string
	SidecarPort      int32
	SidecarResources *corev1.ResourceRequirements

	// Mode is the sei-config mode string ("full", "archive", "validator").
	// Drives nodepool selection and resource sizing via platform.Config.
	Mode string

	// HaltHeight is the seid --halt-height value.
	HaltHeight int64

	// ForbiddenSecretNames are Secret names that MUST NOT appear as Volume
	// sources on the produced PodSpec. GenerateJob fails closed if
	// any are mounted, so the bootstrap pod cannot carry validator signing
	// material even if a future caller wires a Secret volume by accident.
	// Adapters with a validator in scope populate this with the validator's
	// signing-key and node-key Secret names; adapters with no validator in
	// scope leave it nil.
	ForbiddenSecretNames []string
}

// JobName returns the bootstrap Job name for a given resource root.
func JobName(name string) string {
	return fmt.Sprintf("%s-bootstrap", name)
}

// Labels returns labels for bootstrap Job resources.
func Labels(name string) map[string]string {
	return map[string]string{
		bootstrapNodeLabel:      name,
		bootstrapComponentLabel: "bootstrap",
	}
}

// GenerateJob creates the batch Job that runs seid with --halt-height
// to populate a PVC before the consumer takes over. Fails closed if any
// inputs.ForbiddenSecretNames Secret is mounted on the resulting PodSpec, so
// bootstrap pods cannot carry validator signing material regardless of caller.
func GenerateJob(inputs PodInputs, platformCfg platform.Config) (*batchv1.Job, error) {
	if inputs.Name == "" || inputs.Namespace == "" {
		return nil, fmt.Errorf("bootstrap job requires Name and Namespace (got %q/%q)", inputs.Namespace, inputs.Name)
	}
	if inputs.ChainID == "" {
		return nil, fmt.Errorf("bootstrap job requires ChainID (%s/%s)", inputs.Namespace, inputs.Name)
	}
	if inputs.Image == "" {
		return nil, fmt.Errorf("bootstrap job requires Image (%s/%s)", inputs.Namespace, inputs.Name)
	}
	if inputs.HaltHeight <= 0 {
		return nil, fmt.Errorf("bootstrap job requires HaltHeight > 0 (got %d for %s/%s)", inputs.HaltHeight, inputs.Namespace, inputs.Name)
	}
	labels := Labels(inputs.Name)
	podSpec := buildBootstrapPodSpec(inputs, platformCfg)

	if err := rejectForbiddenSecretMounts(&podSpec, inputs.ForbiddenSecretNames); err != nil {
		return nil, fmt.Errorf("bootstrap pod-spec for %s/%s: %w", inputs.Namespace, inputs.Name, err)
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(inputs.Name),
			Namespace: inputs.Namespace,
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

// GenerateService creates a headless Service so the bootstrap pod
// registers as <Name>-0.<Name>.<Namespace>.svc.cluster.local.
func GenerateService(inputs PodInputs) *corev1.Service {
	labels := Labels(inputs.Name)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inputs.Name,
			Namespace: inputs.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 labels,
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "sidecar", Port: inputs.SidecarPort, TargetPort: intstr.FromInt32(inputs.SidecarPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// rejectForbiddenSecretMounts fails closed if podSpec mounts any Secret whose
// name is in forbidden. Returns nil for an empty forbidden list.
func rejectForbiddenSecretMounts(podSpec *corev1.PodSpec, forbidden []string) error {
	if len(forbidden) == 0 {
		return nil
	}
	forbiddenSet := make(map[string]struct{}, len(forbidden))
	for _, name := range forbidden {
		if name != "" {
			forbiddenSet[name] = struct{}{}
		}
	}
	for _, vol := range podSpec.Volumes {
		if vol.Secret == nil {
			continue
		}
		if _, isForbidden := forbiddenSet[vol.Secret.SecretName]; isForbidden {
			return fmt.Errorf("forbidden Secret %q mounted on volume %q", vol.Secret.SecretName, vol.Name)
		}
	}
	return nil
}

func buildBootstrapPodSpec(inputs PodInputs, platformCfg platform.Config) corev1.PodSpec {
	dataVolume := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: fmt.Sprintf("data-%s", inputs.Name),
			},
		},
	}

	sidecar := corev1.Container{
		Name:          "sei-sidecar",
		Image:         inputs.SidecarImage,
		Command:       []string{"seictl", "serve"},
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Env: []corev1.EnvVar{
			{Name: "SEI_CHAIN_ID", Value: inputs.ChainID},
			{Name: "SEI_SIDECAR_PORT", Value: fmt.Sprintf("%d", inputs.SidecarPort)},
			{Name: "SEI_HOME", Value: bootstrapDataDir},
			{Name: "SEI_GENESIS_BUCKET", Value: platformCfg.GenesisBucket},
			{Name: "SEI_GENESIS_REGION", Value: platformCfg.GenesisRegion},
			{Name: "SEI_SNAPSHOT_BUCKET", Value: platformCfg.SnapshotBucket},
			{Name: "SEI_SNAPSHOT_REGION", Value: platformCfg.SnapshotRegion},
		},
		Ports: []corev1.ContainerPort{
			{Name: "sidecar", ContainerPort: inputs.SidecarPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
	}
	if inputs.SidecarResources != nil {
		sidecar.Resources = *inputs.SidecarResources
	}

	seidCmd, seidArgs := bootstrapWaitCommand(inputs.SidecarPort, inputs.HaltHeight)
	seidContainer := corev1.Container{
		Name:    "seid",
		Image:   inputs.Image,
		Command: seidCmd,
		Args:    seidArgs,
		Env: []corev1.EnvVar{
			{Name: "TMPDIR", Value: bootstrapDataDir + "/tmp"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
		Resources: bootstrapResourcesForMode(inputs.Mode, platformCfg),
	}

	seidInit := bootstrapSeidInitContainer(inputs)

	pool := platformCfg.NodepoolForMode(inputs.Mode)

	return corev1.PodSpec{
		Hostname:                      fmt.Sprintf("%s-0", inputs.Name),
		Subdomain:                     inputs.Name,
		ServiceAccountName:            platformCfg.ServiceAccount,
		ShareProcessNamespace:         ptr.To(true),
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: ptr.To(bootstrapTerminationGracePeriod),
		Tolerations: []corev1.Toleration{
			{Key: platformCfg.TolerationKey, Value: pool, Effect: corev1.TaintEffectNoSchedule},
		},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "karpenter.sh/nodepool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{pool},
						}},
					}},
				},
			},
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
// Uses bash's /dev/tcp to make raw HTTP requests instead of wget/curl, which
// are not available on all sei images.
//
// Cosmos SDK's halt-height sends SIGINT to itself after committing the
// target block, producing exit code 130. The wrapper treats 130 as
// success so the Job completes cleanly.
func bootstrapWaitCommand(port int32, haltHeight int64) (command []string, args []string) {
	script := fmt.Sprintf(
		`echo "waiting for sidecar bootstrap tasks to complete..."; `+
			`while true; do `+
			`if (exec 3<>/dev/tcp/localhost/%d && printf "GET /v0/healthz HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3 && head -1 <&3 | grep -q "200") 2>/dev/null; then `+
			`break; `+
			`fi; `+
			`sleep 5; done; `+
			`echo "sidecar healthy, starting seid with halt-height %d"; `+
			`seid start --home %s --halt-height %d; `+
			`rc=$?; if [ $rc -eq 130 ]; then echo "seid halted at target height (exit 130), treating as success"; exit 0; fi; exit $rc`,
		port, haltHeight, bootstrapDataDir, haltHeight,
	)
	return []string{"/bin/bash", "-c"}, []string{script}
}

func bootstrapSeidInitContainer(inputs PodInputs) corev1.Container {
	script := fmt.Sprintf(
		`if [ -f %s/config/genesis.json ]; then echo "data directory already initialized, skipping seid init"; else seid init %s --chain-id %s --home %s --overwrite; fi && mkdir -p %s/tmp`,
		bootstrapDataDir, inputs.ChainID, inputs.ChainID, bootstrapDataDir, bootstrapDataDir,
	)
	return corev1.Container{
		Name:  "seid-init",
		Image: inputs.Image,
		Command: []string{
			"/bin/sh", "-c", script,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
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
