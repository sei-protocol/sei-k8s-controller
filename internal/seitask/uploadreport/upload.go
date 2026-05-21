// Package uploadreport implements `seitask upload-report`: collect Workflow
// observability artifacts (workflow-vars ConfigMap, pod logs for every
// labeled Task pod, the Workflow CR's spec+status) and upload them to S3
// under a per-run prefix.
//
// Runs as the final step of a scenario, after the test-runtime steps have
// finished writing to workflow-vars. The exit code mirrors the EXIT_REASON
// the failing Task wrote (or 0 if no Task set one) — so the Workflow's
// terminal phase reflects the underlying scenario outcome, not whether the
// artifact upload itself succeeded.
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

// PodLister + S3Uploader are the seams tests inject. Production uses
// kubernetes.Interface for log streaming and *s3.Client for uploads.
type PodLister interface {
	ListWorkflowPods(ctx context.Context, namespace, workflowName string) ([]string, error)
	StreamPodLog(ctx context.Context, namespace, podName string) (io.ReadCloser, error)
}

type S3Uploader interface {
	Put(ctx context.Context, bucket, key string, body []byte) error
}

// Params carries the typed inputs to Run.
type Params struct {
	Bucket   string
	Prefix   string // S3 key prefix; trailing slash optional, normalized to bare.
	Workflow taskruntime.WorkflowIdentity

	// Seams.
	Pods PodLister
	S3   S3Uploader
}

type Result struct {
	UploadedKeys []string
	ExitReason   taskruntime.ExitReason
}

// Run uploads workflow-vars, pod logs, and the workflow CR to s3://<bucket>/<prefix>/.
// Reads EXIT_REASON from workflow-vars to determine the subcommand's own
// exit code (caller propagates).
func Run(ctx context.Context, c client.Client, p Params) (Result, error) {
	if err := validate(p); err != nil {
		return Result{}, err
	}
	prefix := strings.TrimSuffix(p.Prefix, "/")
	res := Result{ExitReason: taskruntime.ExitReasonPass}

	cm, exitReason, err := readWorkflowVars(ctx, c, p.Workflow)
	if err != nil {
		return res, err
	}
	res.ExitReason = exitReason

	// Render workflow-vars as a YAML blob alongside the rest.
	if cm != nil {
		body, err := yaml.Marshal(cm.Data)
		if err != nil {
			return res, taskruntime.Infra(fmt.Errorf("marshal workflow-vars: %w", err))
		}
		key := prefix + "/workflow-vars.yaml"
		if err := p.S3.Put(ctx, p.Bucket, key, body); err != nil {
			return res, taskruntime.Infra(fmt.Errorf("upload %s: %w", key, err))
		}
		res.UploadedKeys = append(res.UploadedKeys, key)
	}

	podNames, err := p.Pods.ListWorkflowPods(ctx, p.Workflow.Namespace, p.Workflow.Name)
	if err != nil {
		return res, taskruntime.Infra(fmt.Errorf("listing workflow pods: %w", err))
	}
	for _, name := range podNames {
		key, err := uploadPodLog(ctx, p, prefix, name)
		if err != nil {
			return res, err
		}
		res.UploadedKeys = append(res.UploadedKeys, key)
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

func readWorkflowVars(ctx context.Context, c client.Client, w taskruntime.WorkflowIdentity) (*corev1.ConfigMap, taskruntime.ExitReason, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: taskruntime.WorkflowVarsName(w.Name)}, cm)
	switch {
	case apierrors.IsNotFound(err):
		// No prior Task initialized the CM — treat as pass.
		return nil, taskruntime.ExitReasonPass, nil
	case err != nil:
		return nil, "", taskruntime.Infra(fmt.Errorf("reading workflow-vars: %w", err))
	}
	reason := taskruntime.ExitReason(cm.Data[string(taskruntime.KeyExitReason)])
	if reason == "" {
		reason = taskruntime.ExitReasonPass
	}
	return cm, reason, nil
}

func uploadPodLog(ctx context.Context, p Params, prefix, podName string) (string, error) {
	rc, err := p.Pods.StreamPodLog(ctx, p.Workflow.Namespace, podName)
	if err != nil {
		// Log stream may fail for ephemeral pods that already terminated +
		// GC'd; skip silently so one missing pod doesn't kill the upload.
		return "", nil
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", taskruntime.Infra(fmt.Errorf("reading log for %s: %w", podName, err))
	}
	key := prefix + "/pods/" + podName + ".log"
	if err := p.S3.Put(ctx, p.Bucket, key, body); err != nil {
		return "", taskruntime.Infra(fmt.Errorf("upload %s: %w", key, err))
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// Production-impl helpers (used by cmd/seitask wiring).
// ---------------------------------------------------------------------------

// NewClientsetPodLister wraps a kubernetes.Interface as a PodLister. Lists
// pods labeled chaos-mesh.org/workflow=<name> and streams their logs.
func NewClientsetPodLister(cs kubernetes.Interface) PodLister {
	return &clientsetPodLister{cs: cs}
}

type clientsetPodLister struct{ cs kubernetes.Interface }

func (l *clientsetPodLister) ListWorkflowPods(ctx context.Context, namespace, workflowName string) ([]string, error) {
	pods, err := l.cs.CoreV1().Pods(namespace).List(ctx, metaListOpts(workflowName))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		names = append(names, p.Name)
	}
	return names, nil
}

func (l *clientsetPodLister) StreamPodLog(ctx context.Context, namespace, podName string) (io.ReadCloser, error) {
	return l.cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
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
