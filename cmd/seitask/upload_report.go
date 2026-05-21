package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/urfave/cli/v3"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/sei-protocol/sei-k8s-controller/internal/seitask/uploadreport"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

func newUploadReportCommand() *cli.Command {
	return &cli.Command{
		Name: "upload-report",
		Usage: "Collect Workflow artifacts (workflow-vars, Task pod logs) and upload to S3; " +
			"exit code mirrors the EXIT_REASON workflow-vars key",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "bucket",
				Usage:    "S3 bucket to upload to",
				Sources:  cli.EnvVars("S3_BUCKET"),
				Required: true,
			},
			&cli.StringFlag{
				Name:     "prefix",
				Usage:    "S3 key prefix (typically ${NAMESPACE}/${SCENARIO}/${RUN_ID})",
				Sources:  cli.EnvVars("S3_PREFIX"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "region",
				Usage:   "AWS region",
				Sources: cli.EnvVars("AWS_REGION"),
				Value:   "us-east-2",
			},
		},
		Action: runUploadReport,
	}
}

func runUploadReport(ctx context.Context, cmd *cli.Command) error {
	wf, err := taskruntime.LoadWorkflowIdentity()
	if err != nil {
		return err
	}
	c, err := kubeClientFromEnv()
	if err != nil {
		return err
	}

	// Separate clientset for pods/log streaming — controller-runtime's
	// typed client doesn't expose subresources like /log directly, so we
	// build a vanilla clientset alongside.
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("loading kubeconfig for clientset: %w", err))
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("building clientset: %w", err))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cmd.String("region")))
	if err != nil {
		return taskruntime.Infra(fmt.Errorf("loading AWS config: %w", err))
	}
	s3client := s3.NewFromConfig(awsCfg)

	// upload-report is the terminal observer — never writes EXIT_REASON
	// itself. An infra-fail in the upload would otherwise overwrite a
	// genuine upstream task-fail and lose the underlying classification.
	res, err := uploadreport.Run(ctx, c, uploadreport.Params{
		Bucket:   cmd.String("bucket"),
		Prefix:   cmd.String("prefix"),
		Workflow: wf,
		Pods:     uploadreport.NewClientsetPodLister(cs),
		S3:       uploadreport.NewS3Uploader(s3client),
	})
	if err != nil {
		return err
	}
	log.Printf("upload-report: uploaded %d artifacts (%d pods skipped); upstream exit-reason=%s",
		len(res.UploadedKeys), len(res.SkippedPods), res.ExitReason)

	// Mirror upstream verdict so the Workflow's terminal phase reflects
	// scenario outcome rather than upload-step success.
	switch res.ExitReason {
	case taskruntime.ExitReasonInfraFail:
		return taskruntime.Infra(fmt.Errorf("upstream task reported infra-fail"))
	case taskruntime.ExitReasonTaskFail:
		return taskruntime.Task(fmt.Errorf("upstream task reported task-fail"))
	}
	return nil
}
