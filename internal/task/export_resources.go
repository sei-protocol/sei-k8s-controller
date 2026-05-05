package task

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

const (
	// terminationGracePeriod is intentionally longer than the bootstrap Job's
	// 120s so a cascade-GC mid-upload (operator deletes the SND) gives the
	// sidecar's transfermanager time to finalize the in-flight S3 multipart
	// upload before SIGKILL.
	terminationGracePeriod = int64(600)

	dataDir = platform.DataDir

	nodeLabel      = "sei.io/node"
	componentLabel = "sei.io/component"

	dataVolumeName        = "data"
	seidContainerName     = "seid"
	seidInitContainerName = "seid-init"
)

// ExportJobInputs is the resolved pod-shape contract for an export Job. Built once
// per call by the SND fork-genesis adapter. No SeiNode-shaped fields — the
// fork path has only an SND in scope.
type ExportJobInputs struct {
	Name      string
	Namespace string

	// ChainID is the source chain whose state seid exports.
	ChainID string

	// SeidImage is the seid binary used by both the seid-init container and
	// the main seid container that runs `seid export`.
	SeidImage string

	SidecarImage     string
	SidecarPort      int32
	SidecarResources *corev1.ResourceRequirements

	// Mode drives nodepool selection and resource sizing via platform.Config.
	// "full" suffices for the export workload.
	Mode string

	// PVCClaimName is the bootstrap-stage PVC the export Job mounts read-write.
	// The bootstrap Job populated it with state up to ExportHeight; the export
	// Job runs `seid export --height ExportHeight` against that data.
	PVCClaimName string

	// ExportHeight is the height passed to `seid export --height`.
	ExportHeight int64

	// GenesisBucket / GenesisRegion / GenesisKey are the destination of the
	// uploaded exported-state artifact. Passed to the seid container via env
	// and posted to the sidecar's upload-file task.
	GenesisBucket string
	GenesisRegion string
	GenesisKey    string
}

// ExportJobName returns the export Job name for a given resource root.
func ExportJobName(name string) string { return fmt.Sprintf("%s-export", name) }

// ExportLabels returns the labels stamped on the export Job and its pod template.
func ExportLabels(name string) map[string]string {
	return map[string]string{
		nodeLabel:      name,
		componentLabel: "exporter",
	}
}

// GenerateExportJob produces the batch Job that runs `seid export`, POSTs the
// resulting artifact path to the sidecar's upload-file task, polls until
// terminal, and exits with a code derived from the task's outcome (0
// success, 1 failure with the classified message echoed to stderr).
//
// Pod shape mirrors the bootstrap Job: seid-init + sei-sidecar (native
// sidecar) + seid main. Differences from bootstrap are:
//   - Sidecar env omits SEI_SNAPSHOT_*; only the genesis bucket is in scope.
//   - Seid container command is the export+trigger+poll script below, not
//     the halt-height start script.
//   - TerminationGracePeriodSeconds is 600s (vs 120s) to cover a cascade-GC
//     mid-upload of multi-GB exported state.
//   - No ForbiddenSecretNames invariant — no validator in scope, the SND
//     adapter never wires signing-key Secrets.
func GenerateExportJob(inputs ExportJobInputs, platformCfg platform.Config) (*batchv1.Job, error) {
	if inputs.Name == "" || inputs.Namespace == "" {
		return nil, fmt.Errorf("export job requires Name and Namespace (got %q/%q)", inputs.Namespace, inputs.Name)
	}
	if inputs.ChainID == "" {
		return nil, fmt.Errorf("export job requires ChainID (%s/%s)", inputs.Namespace, inputs.Name)
	}
	if inputs.SeidImage == "" {
		return nil, fmt.Errorf("export job requires SeidImage (%s/%s)", inputs.Namespace, inputs.Name)
	}
	if inputs.PVCClaimName == "" {
		return nil, fmt.Errorf("export job requires PVCClaimName (%s/%s)", inputs.Namespace, inputs.Name)
	}
	if inputs.ExportHeight <= 0 {
		return nil, fmt.Errorf("export job requires ExportHeight > 0 (got %d for %s/%s)", inputs.ExportHeight, inputs.Namespace, inputs.Name)
	}
	if inputs.GenesisBucket == "" {
		return nil, fmt.Errorf("export job requires GenesisBucket (%s/%s)", inputs.Namespace, inputs.Name)
	}
	if inputs.GenesisRegion == "" {
		return nil, fmt.Errorf("export job requires GenesisRegion (%s/%s)", inputs.Namespace, inputs.Name)
	}
	if inputs.GenesisKey == "" {
		return nil, fmt.Errorf("export job requires GenesisKey (%s/%s)", inputs.Namespace, inputs.Name)
	}

	labels := ExportLabels(inputs.Name)
	podSpec := buildPodSpec(inputs, platformCfg)

	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ExportJobName(inputs.Name),
			Namespace: inputs.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32),
			TTLSecondsAfterFinished: &ttl,
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

func buildPodSpec(inputs ExportJobInputs, platformCfg platform.Config) corev1.PodSpec {
	dataVolume := corev1.Volume{
		Name: dataVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: inputs.PVCClaimName,
			},
		},
	}

	sidecar := BuildSidecarContainer(
		inputs.SidecarImage,
		inputs.SidecarPort,
		[]corev1.EnvVar{
			{Name: "SEI_CHAIN_ID", Value: inputs.ChainID},
			{Name: "SEI_SIDECAR_PORT", Value: fmt.Sprintf("%d", inputs.SidecarPort)},
			{Name: "SEI_HOME", Value: dataDir},
			{Name: "SEI_GENESIS_BUCKET", Value: platformCfg.GenesisBucket},
			{Name: "SEI_GENESIS_REGION", Value: platformCfg.GenesisRegion},
		},
		inputs.SidecarResources,
		dataDir,
	)

	seidContainer := corev1.Container{
		Name:    seidContainerName,
		Image:   inputs.SeidImage,
		Command: []string{"/bin/bash", "-c"},
		Args:    []string{exportTriggerScript},
		Env: []corev1.EnvVar{
			{Name: "TMPDIR", Value: dataDir + "/tmp"},
			{Name: "EXPORT_HEIGHT", Value: fmt.Sprintf("%d", inputs.ExportHeight)},
			{Name: "GENESIS_BUCKET", Value: inputs.GenesisBucket},
			{Name: "GENESIS_REGION", Value: inputs.GenesisRegion},
			{Name: "GENESIS_KEY", Value: inputs.GenesisKey},
			{Name: "SIDECAR_PORT", Value: fmt.Sprintf("%d", inputs.SidecarPort)},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataDir},
		},
		Resources: resourcesForMode(inputs.Mode, platformCfg),
	}

	seidInit := seidInitContainer(inputs)
	pool := platformCfg.NodepoolForMode(inputs.Mode)

	shareProc := true
	gracePeriod := terminationGracePeriod
	return corev1.PodSpec{
		Hostname:                      fmt.Sprintf("%s-0", inputs.Name),
		Subdomain:                     inputs.Name,
		ServiceAccountName:            platformCfg.ServiceAccount,
		ShareProcessNamespace:         &shareProc,
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: &gracePeriod,
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

func seidInitContainer(inputs ExportJobInputs) corev1.Container {
	script := fmt.Sprintf(
		`if [ -f %s/config/genesis.json ]; then echo "data directory already initialized, skipping seid init"; else seid init %s --chain-id %s --home %s --overwrite; fi && mkdir -p %s/tmp`,
		dataDir, inputs.ChainID, inputs.ChainID, dataDir, dataDir,
	)
	return corev1.Container{
		Name:    seidInitContainerName,
		Image:   inputs.SeidImage,
		Command: []string{"/bin/sh", "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataDir},
		},
	}
}

func resourcesForMode(mode string, platformCfg platform.Config) corev1.ResourceRequirements {
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

// exportTriggerScript is the seid container's bash command. It runs
// `seid export`, POSTs the resulting artifact to the sidecar's upload-file
// task over localhost, polls task status, and exits with the task outcome.
//
// Wire shape pinned to the sidecar's actual surface:
//
//   - POST /v0/tasks with body {"type":"upload-file","params":{...}} — single
//     endpoint, type-in-envelope. Returns {"id":"<uuid>"}.
//   - GET /v0/tasks/<id> for status polling. Status values "completed" /
//     "failed" match sidecar/engine.TaskStatusCompleted / TaskStatusFailed.
//
// HTTP/1.0 over /dev/tcp because the seid distroless image has no curl/wget
// and no jq. Same /dev/tcp pattern as bootstrap.bootstrapWaitCommand uses
// for healthz polling.
const exportTriggerScript = `set -euo pipefail

# post_task POSTs JSON to the sidecar and echoes the response body.
post_task() {
    local host="$1" port="$2" path="$3" body="$4"
    exec 3<>/dev/tcp/"$host"/"$port"
    printf 'POST %s HTTP/1.0\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s' \
        "$path" "$host" "${#body}" "$body" >&3
    cat <&3
}

# get_task GETs a task by id and echoes the response body.
get_task() {
    local host="$1" port="$2" id="$3"
    exec 3<>/dev/tcp/"$host"/"$port"
    printf 'GET /v0/tasks/%s HTTP/1.0\r\nHost: %s\r\n\r\n' "$id" "$host" >&3
    cat <&3
}

# Wait for the sidecar's healthz to reach 200, bounded by 5min wall-clock.
SECONDS=0
until (exec 3<>/dev/tcp/localhost/${SIDECAR_PORT} \
       && printf 'GET /v0/healthz HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3 \
       && head -1 <&3 | grep -q ' 200 ') 2>/dev/null; do
    if [ "$SECONDS" -ge 300 ]; then
        echo "sidecar healthz never reached 200 after ${SECONDS}s" >&2
        exit 1
    fi
    sleep 5
done

# Run the export. --streaming writes directly to the file in chunks so the
# seid process doesn't buffer multi-GB exports in memory (sei-chain has
# historical OOM scars on the non-streaming path). Stderr stays on the
# container's stderr stream.
seid export --home /sei --height "${EXPORT_HEIGHT}" \
    --streaming --streaming-file /sei/tmp/exported-state.json

# POST upload-file task. Body is the envelope {"type","params"} the sidecar's
# /v0/tasks handler decodes; capture task id from the response.
BODY=$(printf '{"type":"upload-file","params":{"file":"/sei/tmp/exported-state.json","bucket":"%s","key":"%s","region":"%s"}}' \
       "${GENESIS_BUCKET}" "${GENESIS_KEY}" "${GENESIS_REGION}")
RESP=$(post_task localhost "${SIDECAR_PORT}" /v0/tasks "${BODY}")
TASK_ID=$(echo "$RESP" | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)
if [ -z "$TASK_ID" ]; then
    echo "failed to extract task id from sidecar response" >&2
    echo "$RESP" >&2
    exit 1
fi

# Poll until terminal. On failure, echo the classified error to stderr so
# the operator sees the upload-file message via kubectl logs.
while true; do
    RESP=$(get_task localhost "${SIDECAR_PORT}" "${TASK_ID}")
    STATUS=$(echo "$RESP" | grep -oE '"status":"[^"]+"' | head -1 | cut -d'"' -f4)
    case "$STATUS" in
        completed) exit 0 ;;
        failed)
            echo "$RESP" | grep -oE '"error":"[^"]+"' | head -1 | cut -d'"' -f4 >&2
            exit 1 ;;
        *) sleep 5 ;;
    esac
done
`
