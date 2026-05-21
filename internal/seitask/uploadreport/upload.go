// Package uploadreport implements `seitask upload-report`: collect Workflow
// observability artifacts that Loki doesn't index — workflow-vars CM, the
// parent Workflow CR, the WorkflowNode tree — and upload them to S3 under
// a per-run prefix.
//
// Pod stdout/stderr is NOT uploaded; Alloy + Loki on the cluster already
// ingest every Task pod's logs indexed by chaos-mesh.org/workflow.
// upload-report's job is the K8s resource snapshot Loki can't give you:
// the structural record of what fired and how each step terminated.
//
// Runs as the final step of a scenario. The subcommand's exit code mirrors
// the EXIT_REASON workflow-vars value so the Workflow's terminal phase
// reflects scenario outcome rather than upload-step success. Does NOT
// write EXIT_REASON — upload-report is the terminal observer, not a
// producer of the upstream verdict.
//
// +kubebuilder:rbac:groups=chaos-mesh.org,resources=workflows;workflownodes,verbs=get;list
package uploadreport

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

// S3Uploader is the seam tests inject. Production wires *s3.Client.
type S3Uploader interface {
	Put(ctx context.Context, bucket, key string, body []byte) error
}

// Workflow + WorkflowNode are chaos-mesh.org/v1alpha1 CRs. unstructured so
// the binary doesn't depend on the chaos-mesh Go types for read-only
// artifact collection.
var (
	workflowGVK     = schema.GroupVersionKind{Group: "chaos-mesh.org", Version: "v1alpha1", Kind: "Workflow"}
	workflowNodeGVK = schema.GroupVersionKind{Group: "chaos-mesh.org", Version: "v1alpha1", Kind: "WorkflowNode"}
)

// Params carries the typed inputs to Run.
type Params struct {
	Bucket   string
	Prefix   string // S3 key prefix; leading/trailing slashes are trimmed.
	Workflow taskruntime.WorkflowIdentity

	S3 S3Uploader
}

type Result struct {
	UploadedKeys []string
	ExitReason   taskruntime.ExitReason
}

// Run uploads workflow-vars + Workflow CR + WorkflowNode tree under
// s3://<bucket>/<prefix>/. Reads EXIT_REASON from workflow-vars so the
// caller can propagate it into the process's exit code.
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
	case p.S3 == nil:
		return fmt.Errorf("upload-report: S3Uploader seam is required")
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
// WorkflowNode owned by it. The WorkflowNode tree is the canonical record
// of which Task fired, in what order, and how each terminated — the
// structural artifact Loki can't give you from log queries alone.
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
	if err := c.List(ctx, nodes,
		client.InNamespace(p.Workflow.Namespace),
		client.MatchingLabels{"chaos-mesh.org/workflow": p.Workflow.Name},
	); err != nil {
		return taskruntime.Infra(fmt.Errorf("listing WorkflowNodes: %w", err))
	}
	body, err = yaml.Marshal(nodes.Object)
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("marshal WorkflowNodes: %w", err))
	}
	return putAt(ctx, p, prefix+"/workflownodes.yaml", body, res)
}

func putAt(ctx context.Context, p Params, key string, body []byte, res *Result) error {
	if err := p.S3.Put(ctx, p.Bucket, key, body); err != nil {
		return taskruntime.Infra(fmt.Errorf("upload %s: %w", key, err))
	}
	res.UploadedKeys = append(res.UploadedKeys, key)
	return nil
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
