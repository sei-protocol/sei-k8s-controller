//go:build integration

package integration

import (
	"context"
	_ "embed"
	"strconv"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

//go:embed testdata/post_load_rpc.mjs
var postLoadRPCScript string

// Use Node from the existing release-test image. The harness carries the
// benchmark itself, so its implementation and the chain test roll together.
func runPostLoadRPC(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ch *chain, image string, first, last int64) {
	t.Helper()
	id := ch.network.Name()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rpc-" + id, Namespace: ch.network.Namespace(),
			Labels: map[string]string{runLabelKey: id},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](0), ActiveDeadlineSeconds: ptr.To[int64](35 * 60),
			TTLSecondsAfterFinished: ptr.To[int32](86400),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{runLabelKey: id}},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](10001),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name: "rpc-benchmark", Image: image,
						Command: []string{"node", "--input-type=module", "--eval"},
						Args: []string{
							postLoadRPCScript + "\nawait runPostLoad(process.argv[1], Number(process.argv[2]), Number(process.argv[3]), undefined, Number(process.argv[4]), undefined, process.argv[5]);",
							"--", ch.rpcNodes[1].EVMRPC(), strconv.FormatInt(first, 10), strconv.FormatInt(last, 10), strconv.Itoa(historyRPCMaxLogBlocks), ch.rpcNodes[0].EVMRPC(),
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
						},
					}},
				},
			},
		},
	}
	runJob(ctx, t, cs, job)
	t.Logf("post-load RPC results:\n%s", podLogTail(ctx, cs, job.Namespace, job.Name))
}
