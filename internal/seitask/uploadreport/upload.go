// Package uploadreport implements `seitask upload-report`: collect Workflow
// observability artifacts (workflow-vars CM, the Workflow CR + its
// WorkflowNode tree, per-container Task pod logs) and upload them to S3
// under a per-run prefix.
//
// Runs as the final step of a scenario. The subcommand's exit code mirrors
// the EXIT_REASON workflow-vars value so the Workflow's terminal phase
// reflects scenario outcome rather than upload-step success. Does NOT write
// EXIT_REASON itself — upload-report is the terminal observer, never a
// producer of the upstream verdict.
//
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=chaos-mesh.org,resources=workflows;workflownodes,verbs=get;list
package uploadreport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

// PodLister + S3Uploader are the seams tests inject. Production wires
// real K8s clientset + AWS S3 client.
type PodLister interface {
	// ListWorkflowPods returns every pod labeled
	// chaos-mesh.org/workflow=<workflowName>. The returned objects must
	// carry .Spec.InitContainers and .Spec.Containers so the caller can
	// enumerate per-container log streams.
	ListWorkflowPods(ctx context.Context, namespace, workflowName string) ([]corev1.Pod, error)
	// StreamContainerLog streams logs for one container of one pod. Empty
	// containerName selects the pod's default container, matching kubectl.
	StreamContainerLog(ctx context.Context, namespace, podName, containerName string) (io.ReadCloser, error)
}

type S3Uploader interface {
	Put(ctx context.Context, bucket, key string, body []byte) error
}

// Workflow + WorkflowNode are chaos-mesh.org/v1alpha1 CRs. We use
// unstructured.Unstructured so the binary doesn't depend on the chaos-mesh
// Go types — adding that dependency for read-only artifact collection
// isn't worth the module weight.
var (
	workflowGVK     = schema.GroupVersionKind{Group: "chaos-mesh.org", Version: "v1alpha1", Kind: "Workflow"}
	workflowNodeGVK = schema.GroupVersionKind{Group: "chaos-mesh.org", Version: "v1alpha1", Kind: "WorkflowNode"}
)

// Params carries the typed inputs to Run.
type Params struct {
	Bucket   string
	Prefix   string // S3 key prefix; leading/trailing slashes are trimmed.
	Workflow taskruntime.WorkflowIdentity

	// Seams.
	Pods PodLister
	S3   S3Uploader
}

type Result struct {
	UploadedKeys []string
	SkippedPods  []SkippedPod
	ExitReason   taskruntime.ExitReason
}

// SkippedPod records pods whose logs were not uploaded, along with the
// classification of why. Surfaced in the Result so callers + the run record
// can distinguish "pod was GC'd" (benign) from "RBAC drop" (infra-fail).
type SkippedPod struct {
	Pod    string
	Reason string
	Err    error
}

// Run uploads workflow-vars + Workflow CR + WorkflowNode tree + per-
// container pod logs under s3://<bucket>/<prefix>/. Reads EXIT_REASON from
// workflow-vars to set Result.ExitReason; the caller propagates that into
// the process's own exit code.
func Run(ctx context.Context, c client.Client, p Params) (Result, error) {
	if err := validate(p); err != nil {
		return Result{}, err
	}
	prefix := strings.Trim(p.Prefix, "/")
	res := Result{ExitReason: taskruntime.ExitReasonPass}

	if err := uploadWorkflowVars(ctx, c, p, prefix, &res); err != nil {
		return res, err
	}
	if err := uploadWorkflowResources(ctx, c, p, prefix, &res); err != nil {
		return res, err
	}
	if err := uploadPodLogs(ctx, p, prefix, &res); err != nil {
		return res, err
	}
	return res, nil
}

func validate(p Params) error {
	switch {
	case p.Bucket == "":
		return fmt.Errorf("upload-report: --bucket is required")
	case p.Prefix == "":
		return fmt.Errorf("upload-report: --prefix is required")
	case p.Workflow.Name == "" || p.Workflow.Namespace == "":
		return fmt.Errorf("upload-report: workflow identity not loaded")
	case p.Pods == nil || p.S3 == nil:
		return fmt.Errorf("upload-report: PodLister and S3Uploader seams are required")
	}
	return nil
}

func uploadWorkflowVars(ctx context.Context, c client.Client, p Params, prefix string, res *Result) error {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: p.Workflow.Namespace, Name: taskruntime.WorkflowVarsName(p.Workflow.Name)}, cm)
	switch {
	case apierrors.IsNotFound(err):
		// No prior Task initialized the CM — pass, nothing to upload.
		return nil
	case err != nil:
		return taskruntime.Infra(fmt.Errorf("reading workflow-vars: %w", err))
	}
	if reason := taskruntime.ExitReason(cm.Data[string(taskruntime.KeyExitReason)]); reason != "" {
		res.ExitReason = reason
	}
	body, err := yaml.Marshal(cm.Data)
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("marshal workflow-vars: %w", err))
	}
	return putAt(ctx, p, prefix+"/workflow-vars.yaml", body, res)
}

// uploadWorkflowResources serializes the parent Workflow CR + every
// WorkflowNode owned by it to YAML. WorkflowNode is the canonical record
// of which Task fired, in what order, and how each one terminated — the
// highest-value post-mortem artifact for a Chaos Mesh-driven harness.
func uploadWorkflowResources(ctx context.Context, c client.Client, p Params, prefix string, res *Result) error {
	wf := &unstructured.Unstructured{}
	wf.SetGroupVersionKind(workflowGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: p.Workflow.Namespace, Name: p.Workflow.Name}, wf); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // workflow was deleted before our turn; nothing to upload
		}
		return taskruntime.Infra(fmt.Errorf("reading Workflow CR: %w", err))
	}
	body, err := yaml.Marshal(wf.Object)
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("marshal Workflow CR: %w", err))
	}
	if err := putAt(ctx, p, prefix+"/workflow.yaml", body, res); err != nil {
		return err
	}

	nodes := &unstructured.UnstructuredList{}
	nodes.SetGroupVersionKind(workflowNodeGVK)
	listOpts := []client.ListOption{
		client.InNamespace(p.Workflow.Namespace),
		client.MatchingLabels{"chaos-mesh.org/workflow": p.Workflow.Name},
	}
	if err := c.List(ctx, nodes, listOpts...); err != nil {
		return taskruntime.Infra(fmt.Errorf("listing WorkflowNodes: %w", err))
	}
	body, err = yaml.Marshal(nodes.Object)
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("marshal WorkflowNodes: %w", err))
	}
	return putAt(ctx, p, prefix+"/workflownodes.yaml", body, res)
}

// uploadPodLogs streams every container in every Task pod and uploads each
// to pods/<pod>/<container>.log. Pod GC race + per-container coverage are
// the two reasons this is per-container, not per-pod. Log-stream failures
// are classified (NotFound = benign, anything else = infra-fail recorded
// in res.SkippedPods + propagated as an error).
func uploadPodLogs(ctx context.Context, p Params, prefix string, res *Result) error {
	pods, err := p.Pods.ListWorkflowPods(ctx, p.Workflow.Namespace, p.Workflow.Name)
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("listing workflow pods: %w", err))
	}
	for _, pod := range pods {
		for _, ctr := range allContainers(pod) {
			key := fmt.Sprintf("%s/pods/%s/%s.log", prefix, pod.Name, ctr)
			rc, err := p.Pods.StreamContainerLog(ctx, p.Workflow.Namespace, pod.Name, ctr)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// Pod GC'd between List and Stream — benign late-race.
					res.SkippedPods = append(res.SkippedPods, SkippedPod{Pod: pod.Name, Reason: "pod-gone", Err: err})
					continue
				}
				return taskruntime.Infra(fmt.Errorf("streaming log for %s/%s: %w", pod.Name, ctr, err))
			}
			body, readErr := io.ReadAll(rc)
			_ = rc.Close()
			if readErr != nil {
				return taskruntime.Infra(fmt.Errorf("reading log for %s/%s: %w", pod.Name, ctr, readErr))
			}
			if err := putAt(ctx, p, key, body, res); err != nil {
				return err
			}
		}
	}
	return nil
}

func allContainers(p corev1.Pod) []string {
	out := make([]string, 0, len(p.Spec.InitContainers)+len(p.Spec.Containers))
	for _, c := range p.Spec.InitContainers {
		out = append(out, c.Name)
	}
	for _, c := range p.Spec.Containers {
		out = append(out, c.Name)
	}
	return out
}

func putAt(ctx context.Context, p Params, key string, body []byte, res *Result) error {
	if err := p.S3.Put(ctx, p.Bucket, key, body); err != nil {
		return taskruntime.Infra(fmt.Errorf("upload %s: %w", key, err))
	}
	res.UploadedKeys = append(res.UploadedKeys, key)
	return nil
}

// ---------------------------------------------------------------------------
// Production-impl helpers (used by cmd/seitask wiring).
// ---------------------------------------------------------------------------

// NewClientsetPodLister wraps a kubernetes.Interface as a PodLister.
func NewClientsetPodLister(cs kubernetes.Interface) PodLister {
	return &clientsetPodLister{cs: cs}
}

type clientsetPodLister struct{ cs kubernetes.Interface }

func (l *clientsetPodLister) ListWorkflowPods(ctx context.Context, namespace, workflowName string) ([]corev1.Pod, error) {
	pods, err := l.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "chaos-mesh.org/workflow=" + workflowName,
	})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (l *clientsetPodLister) StreamContainerLog(ctx context.Context, namespace, podName, containerName string) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{}
	if containerName != "" {
		opts.Container = containerName
	}
	return l.cs.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
}

// NewS3Uploader wraps an *s3.Client as an S3Uploader.
func NewS3Uploader(client *s3.Client) S3Uploader {
	return &s3Uploader{client: client}
}

type s3Uploader struct{ client *s3.Client }

func (u *s3Uploader) Put(ctx context.Context, bucket, key string, body []byte) error {
	_, err := u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	return err
}
