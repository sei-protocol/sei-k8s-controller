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

	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

const (
	bootstrapTerminationGracePeriod = int64(120)
	bootstrapDataDir                = platform.DataDir
	bootstrapDefaultSidecarImage    = platform.DefaultSidecarImage
	bootstrapNodeLabel              = "sei.io/node"
	bootstrapComponentLabel         = "sei.io/component"
)

// BootstrapPodInputs is the resolved pod-shape contract for a bootstrap Job
// and its sibling headless Service. Built once per call by an adapter that
// has the upstream context (today: a SeiNode for per-node bootstrap).
// Helpers below operate on this struct only — they no longer reach into a
// SeiNode.
//
// The Service path uses only Name, Namespace, and SidecarPort and tolerates
// the rest being zero-valued; GenerateBootstrapJob enforces the full set.
type BootstrapPodInputs struct {
	// Name is the resource name root used for both the Job and the Service.
	// Pod hostname becomes "<Name>-0" so the in-cluster sidecar URL is
	// "<Name>-0.<Name>.<Namespace>.svc.cluster.local". The data PVC the pod
	// mounts at /sei is "data-<Name>".
	Name string

	Namespace string

	// ChainID is what the seid-init container uses to materialise the data
	// directory and what the sidecar advertises as its SEI_CHAIN_ID.
	ChainID string

	// SeidImage is the image for the seid-init container (runs `seid init`
	// once before the main containers).
	SeidImage string

	// BootstrapImage is the image for the main "seid" container that runs
	// `seid start --halt-height N`. The per-SeiNode adapter resolves this
	// to snap.BootstrapImage when set, else node.Spec.Image, and assigns
	// the same value to SeidImage.
	BootstrapImage string

	// SidecarImage / SidecarPort / SidecarResources are resolved at the
	// adapter (with platform defaults applied) so the helpers below can
	// stay value-driven.
	SidecarImage     string
	SidecarPort      int32
	SidecarResources *corev1.ResourceRequirements

	// Mode is the sei-config mode string ("full", "archive", "validator").
	// Drives nodepool selection and resource sizing via platform.Config.
	Mode string

	// HaltHeight is the seid --halt-height value. Required for Job; ignored
	// for Service.
	HaltHeight int64
}

// BootstrapJobName returns the bootstrap Job name for a given resource root.
// Pure string formatter — no SeiNode required.
func BootstrapJobName(name string) string {
	return fmt.Sprintf("%s-bootstrap", name)
}

// BootstrapLabels returns labels for bootstrap Job resources.
func BootstrapLabels(name string) map[string]string {
	return map[string]string{
		bootstrapNodeLabel:      name,
		bootstrapComponentLabel: "bootstrap",
	}
}

// GenerateBootstrapJob creates the batch Job that runs seid with --halt-height
// to populate a PVC before the consumer (a StatefulSet today, an export Job
// tomorrow) takes over.
//
// The pod-spec deliberately omits validator signing material. The per-SeiNode
// adapter calls assertNoSigningKeyOnBootstrapPod after this returns to enforce
// that invariant; the SND fork-genesis adapter has no SeiNode and physically
// cannot leak signing material.
func GenerateBootstrapJob(in BootstrapPodInputs, platformCfg platform.Config) (*batchv1.Job, error) {
	if in.Name == "" || in.Namespace == "" {
		return nil, fmt.Errorf("bootstrap job requires Name and Namespace (got %q/%q)", in.Namespace, in.Name)
	}
	if in.ChainID == "" {
		return nil, fmt.Errorf("bootstrap job requires ChainID (%s/%s)", in.Namespace, in.Name)
	}
	if in.SeidImage == "" || in.BootstrapImage == "" {
		return nil, fmt.Errorf("bootstrap job requires SeidImage and BootstrapImage (%s/%s)", in.Namespace, in.Name)
	}
	if in.HaltHeight <= 0 {
		return nil, fmt.Errorf("bootstrap job requires HaltHeight > 0 (got %d for %s/%s)", in.HaltHeight, in.Namespace, in.Name)
	}
	labels := BootstrapLabels(in.Name)
	podSpec := buildBootstrapPodSpec(in, platformCfg)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BootstrapJobName(in.Name),
			Namespace: in.Namespace,
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

// GenerateBootstrapService creates a headless Service that provides stable
// DNS for the bootstrap Job pod. The pod registers as
// <Name>-0.<Name>.<Namespace>.svc.cluster.local. On the per-SeiNode bootstrap
// path the Service name matches the eventual StatefulSet's headless Service
// name so the sidecar URL is consistent across both phases. On the SND fork
// path the Service is owned by the SeiNodeDeployment and named after the
// exporter resource root.
func GenerateBootstrapService(in BootstrapPodInputs) *corev1.Service {
	labels := BootstrapLabels(in.Name)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 labels,
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "sidecar", Port: in.SidecarPort, TargetPort: intstr.FromInt32(in.SidecarPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func buildBootstrapPodSpec(in BootstrapPodInputs, platformCfg platform.Config) corev1.PodSpec {
	dataVolume := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: fmt.Sprintf("data-%s", in.Name),
			},
		},
	}

	sidecar := corev1.Container{
		Name:          "sei-sidecar",
		Image:         in.SidecarImage,
		Command:       []string{"seictl", "serve"},
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Env: []corev1.EnvVar{
			{Name: "SEI_CHAIN_ID", Value: in.ChainID},
			{Name: "SEI_SIDECAR_PORT", Value: fmt.Sprintf("%d", in.SidecarPort)},
			{Name: "SEI_HOME", Value: bootstrapDataDir},
			{Name: "SEI_GENESIS_BUCKET", Value: platformCfg.GenesisBucket},
			{Name: "SEI_GENESIS_REGION", Value: platformCfg.GenesisRegion},
			{Name: "SEI_SNAPSHOT_BUCKET", Value: platformCfg.SnapshotBucket},
			{Name: "SEI_SNAPSHOT_REGION", Value: platformCfg.SnapshotRegion},
		},
		Ports: []corev1.ContainerPort{
			{Name: "sidecar", ContainerPort: in.SidecarPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
	}
	if in.SidecarResources != nil {
		sidecar.Resources = *in.SidecarResources
	}

	seidCmd, seidArgs := bootstrapWaitCommand(in.SidecarPort, in.HaltHeight)
	seidContainer := corev1.Container{
		Name:    "seid",
		Image:   in.BootstrapImage,
		Command: seidCmd,
		Args:    seidArgs,
		Env: []corev1.EnvVar{
			{Name: "TMPDIR", Value: bootstrapDataDir + "/tmp"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: bootstrapDataDir},
		},
		Resources: bootstrapResourcesForMode(in.Mode, platformCfg),
	}

	seidInit := bootstrapSeidInitContainer(in)

	pool := platformCfg.NodepoolForMode(in.Mode)

	return corev1.PodSpec{
		Hostname:                      fmt.Sprintf("%s-0", in.Name),
		Subdomain:                     in.Name,
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

func bootstrapSeidInitContainer(in BootstrapPodInputs) corev1.Container {
	script := fmt.Sprintf(
		`if [ -f %s/config/genesis.json ]; then echo "data directory already initialized, skipping seid init"; else seid init %s --chain-id %s --home %s --overwrite; fi && mkdir -p %s/tmp`,
		bootstrapDataDir, in.ChainID, in.ChainID, bootstrapDataDir, bootstrapDataDir,
	)
	return corev1.Container{
		Name:  "seid-init",
		Image: in.SeidImage,
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
