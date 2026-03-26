package nodegroup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	defaultGenesisBucket       = "sei-genesis-artifacts"
	defaultMaxCeremonyDuration = 15 * time.Minute
	defaultSidecarPort         = int32(7777)
	defaultP2PPort             = int32(26656)
)

// SidecarStatusClient abstracts the sidecar HTTP API for testability.
type SidecarStatusClient interface {
	GetNodeID(ctx context.Context) (string, error)
}

// sidecarSubmitter is the subset of sidecar.SidecarClient that can submit and poll tasks.
type sidecarSubmitter interface {
	SubmitTask(ctx context.Context, task sidecar.TaskRequest) (interface{ String() string }, error)
	GetTask(ctx context.Context, id interface{ String() string }) (*sidecar.TaskResult, error)
}

// reconcileGenesisAssembly is the top-level genesis coordination entry point.
// It runs only when spec.genesis is set.
func (r *SeiNodeGroupReconciler) reconcileGenesisAssembly(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) (ctrl.Result, error) {
	if group.Spec.Genesis == nil {
		return ctrl.Result{}, nil
	}

	nodes, err := r.listChildSeiNodes(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	if int32(len(nodes)) < group.Spec.Replicas {
		log.FromContext(ctx).Info("waiting for all SeiNodes to be created",
			"have", len(nodes), "want", group.Spec.Replicas)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	maxDuration := defaultMaxCeremonyDuration
	if group.Spec.Genesis.MaxCeremonyDuration != nil {
		maxDuration = group.Spec.Genesis.MaxCeremonyDuration.Duration
	}
	if !group.CreationTimestamp.IsZero() && time.Since(group.CreationTimestamp.Time) > maxDuration {
		r.Recorder.Event(group, corev1.EventTypeWarning, "GenesisCeremonyTimeout",
			"Genesis ceremony exceeded maximum duration")
		return r.setGenesisCondition(ctx, group, metav1.ConditionFalse,
			"Timeout", "genesis ceremony exceeded maximum duration")
	}

	for i := range nodes {
		n := &nodes[i]
		if n.Status.Phase == seiv1alpha1.PhaseFailed {
			msg := fmt.Sprintf("SeiNode %s is in Failed phase", n.Name)
			r.Recorder.Event(group, corev1.EventTypeWarning, "GenesisBootstrapFailed", msg)
			return r.setGenesisCondition(ctx, group, metav1.ConditionFalse, "BootstrapFailed", msg)
		}
	}

	allGenesisTasksReady := true
	for i := range nodes {
		plan := nodes[i].Status.InitPlan
		if plan == nil {
			allGenesisTasksReady = false
			break
		}
		ct := planner.CurrentTask(plan)
		if ct == nil || ct.Type != task.TaskTypeAwaitGenesisAssembly {
			allGenesisTasksReady = false
			break
		}
	}
	if !allGenesisTasksReady {
		log.FromContext(ctx).Info("waiting for all SeiNodes genesis tasks to complete")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if group.Status.InitPlan == nil {
		return r.startAssembly(ctx, group)
	}

	if group.Status.InitPlan.Phase == seiv1alpha1.TaskPlanComplete {
		if group.Status.GenesisS3URI != "" {
			return ctrl.Result{}, nil
		}
		return r.finalizeGenesis(ctx, group, nodes)
	}

	if group.Status.InitPlan.Phase == seiv1alpha1.TaskPlanFailed {
		return r.setGenesisCondition(ctx, group, metav1.ConditionFalse,
			"AssemblyFailed", "genesis assembly task failed")
	}

	return r.driveInitPlan(ctx, group, nodes)
}

func (r *SeiNodeGroupReconciler) startAssembly(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) (ctrl.Result, error) {
	plan := &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{Type: sidecar.TaskTypeAssembleGenesis, Status: seiv1alpha1.PlannedTaskPending},
		},
	}

	patch := client.MergeFrom(group.DeepCopy())
	group.Status.InitPlan = plan
	if err := r.Status().Patch(ctx, group, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting init plan: %w", err)
	}

	r.Recorder.Event(group, corev1.EventTypeNormal, "GenesisAssemblyStarted",
		"All bootstrap tasks complete, starting genesis assembly")
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *SeiNodeGroupReconciler) driveInitPlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, nodes []seiv1alpha1.SeiNode) (ctrl.Result, error) {
	if len(nodes) == 0 {
		return ctrl.Result{}, fmt.Errorf("no nodes available for assembly")
	}

	assemblerNode := &nodes[0]
	sc := r.buildSidecarClient(assemblerNode)
	if sc == nil {
		log.FromContext(ctx).Info("assembler sidecar not reachable yet")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	plan := group.Status.InitPlan
	task := &plan.Tasks[0]

	switch task.Status {
	case seiv1alpha1.PlannedTaskPending:
		s3 := genesisS3Config(group)
		nodeParams := make([]sidecar.GenesisNodeParam, len(nodes))
		for i := range nodes {
			nodeParams[i] = sidecar.GenesisNodeParam{Name: nodes[i].Name}
		}
		builder := sidecar.AssembleAndUploadGenesisTask{
			S3Bucket: s3.Bucket,
			S3Prefix: s3.Prefix,
			S3Region: s3.Region,
			ChainID:  group.Spec.Genesis.ChainID,
			Nodes:    nodeParams,
		}
		req := builder.ToTaskRequest()
		id, err := sc.(sidecarSubmitter).SubmitTask(ctx, req)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("submitting assembly task: %w", err)
		}
		patch := client.MergeFrom(group.DeepCopy())
		task.Status = seiv1alpha1.PlannedTaskComplete
		task.ID = id.String()
		if err := r.Status().Patch(ctx, group, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording assembly task submission: %w", err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *SeiNodeGroupReconciler) finalizeGenesis(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, nodes []seiv1alpha1.SeiNode) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	peerList, err := r.collectPeers(ctx, nodes)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("collecting peers: %w", err)
	}

	for i := range nodes {
		if err := r.setStaticPeers(ctx, &nodes[i], peerList); err != nil {
			return ctrl.Result{}, fmt.Errorf("pushing peers to %s: %w", nodes[i].Name, err)
		}
	}

	s3 := genesisS3Config(group)
	genesisURI := fmt.Sprintf("s3://%s/%sgenesis.json", s3.Bucket, s3.Prefix)

	patch := client.MergeFrom(group.DeepCopy())
	group.Status.GenesisS3URI = genesisURI
	if err := r.Status().Patch(ctx, group, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording genesis S3 URI: %w", err)
	}

	logger.Info("genesis ceremony complete", "genesisURI", genesisURI, "peers", len(peerList))
	r.Recorder.Eventf(group, corev1.EventTypeNormal, "GenesisCeremonyComplete",
		"Genesis assembled and distributed to %d nodes", len(nodes))

	return r.setGenesisCondition(ctx, group, metav1.ConditionTrue,
		"CeremonyComplete", "genesis assembled and distributed")
}

func (r *SeiNodeGroupReconciler) collectPeers(ctx context.Context, nodes []seiv1alpha1.SeiNode) ([]string, error) {
	var peers []string
	for i := range nodes {
		n := &nodes[i]
		sc := r.buildSidecarClient(n)
		if sc == nil {
			return nil, fmt.Errorf("cannot reach sidecar on %s", n.Name)
		}

		getter, ok := sc.(SidecarStatusClient)
		if !ok {
			return nil, fmt.Errorf("sidecar client for %s does not support GetNodeID", n.Name)
		}
		nodeID, err := getter.GetNodeID(ctx)
		if err != nil {
			return nil, fmt.Errorf("getting node ID for %s: %w", n.Name, err)
		}

		port := defaultP2PPort
		dns := fmt.Sprintf("%s.%s.svc.cluster.local", n.Name, n.Namespace)
		peers = append(peers, fmt.Sprintf("%s@%s:%d", nodeID, dns, port))
	}
	return peers, nil
}

func (r *SeiNodeGroupReconciler) setStaticPeers(ctx context.Context, node *seiv1alpha1.SeiNode, peers []string) error {
	if node.Spec.Validator == nil {
		return fmt.Errorf("node %s has no validator spec", node.Name)
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Validator.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: peers}},
	}
	return r.Patch(ctx, node, patch)
}

func (r *SeiNodeGroupReconciler) buildSidecarClient(node *seiv1alpha1.SeiNode) any {
	if r.BuildSidecarClientFn != nil {
		return r.BuildSidecarClientFn(node)
	}
	url := planner.SidecarURLForNode(node)
	c, err := sidecar.NewSidecarClient(url)
	if err != nil {
		return nil
	}
	return c
}

func (r *SeiNodeGroupReconciler) setGenesisCondition(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, status metav1.ConditionStatus, reason, message string) (ctrl.Result, error) {
	patch := client.MergeFrom(group.DeepCopy())

	found := false
	for i := range group.Status.Conditions {
		if group.Status.Conditions[i].Type == seiv1alpha1.ConditionGenesisCeremonyComplete {
			group.Status.Conditions[i].Status = status
			group.Status.Conditions[i].Reason = reason
			group.Status.Conditions[i].Message = message
			group.Status.Conditions[i].LastTransitionTime = metav1.Now()
			found = true
			break
		}
	}
	if !found {
		group.Status.Conditions = append(group.Status.Conditions, metav1.Condition{
			Type:               seiv1alpha1.ConditionGenesisCeremonyComplete,
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
	}

	if err := r.Status().Patch(ctx, group, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting genesis condition: %w", err)
	}
	return ctrl.Result{}, nil
}

// genesisS3Config returns the S3 destination for genesis artifacts, applying
// defaults when the user omits the field.
func genesisS3Config(group *seiv1alpha1.SeiNodeGroup) seiv1alpha1.GenesisS3Destination {
	gc := group.Spec.Genesis
	if gc.GenesisS3 != nil {
		dest := *gc.GenesisS3
		if dest.Prefix == "" {
			dest.Prefix = fmt.Sprintf("%s/%s/", gc.ChainID, group.Name)
		}
		return dest
	}
	return seiv1alpha1.GenesisS3Destination{
		Bucket: defaultGenesisBucket,
		Prefix: fmt.Sprintf("%s/%s/", gc.ChainID, group.Name),
		Region: "us-east-2",
	}
}

func marshalOverrides(overrides map[string]string) string {
	if len(overrides) == 0 {
		return ""
	}
	data, err := json.Marshal(overrides)
	if err != nil {
		return ""
	}
	return string(data)
}
