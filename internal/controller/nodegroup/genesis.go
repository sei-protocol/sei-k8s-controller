package nodegroup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

const (
	defaultGenesisBucket       = "sei-genesis-artifacts"
	defaultMaxCeremonyDuration = 15 * time.Minute
	defaultP2PPort             = int32(26656)
)

// SidecarStatusClient abstracts the sidecar HTTP API for testability.
type SidecarStatusClient interface {
	GetNodeID(ctx context.Context) (string, error)
}

// reconcileGenesisAssembly is the top-level genesis coordination entry point.
// It runs only when spec.genesis is set. The flow is:
//  1. Wait for all child SeiNodes to be created.
//  2. Check S3 for each node's gentx artifact.
//  3. Once all artifacts are present, build and drive a group-level InitPlan
//     that submits assemble-and-upload-genesis to the 0th node's sidecar.
//  4. After the plan completes, finalize: collect peers, distribute them, and
//     record the genesis S3 URI.
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

	allArtifactsUploaded, err := r.checkGenesisArtifacts(ctx, group, nodes)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking genesis artifacts: %w", err)
	}
	if !allArtifactsUploaded {
		log.FromContext(ctx).Info("waiting for all genesis artifacts in S3")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if group.Status.InitPlan == nil {
		return r.startAssembly(ctx, group, nodes)
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

// checkGenesisArtifacts verifies that every node has uploaded its gentx
// artifact to S3. Returns true only when all expected keys are present.
func (r *SeiNodeGroupReconciler) checkGenesisArtifacts(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, nodes []seiv1alpha1.SeiNode) (bool, error) {
	if r.ObjectStore == nil {
		return false, fmt.Errorf("ObjectStore not configured on reconciler")
	}

	s3 := genesisS3Config(group)

	for i := range nodes {
		key := fmt.Sprintf("%s%s/gentx.json", s3.Prefix, nodes[i].Name)
		exists, err := r.ObjectStore.HeadObject(ctx, s3.Bucket, key, s3.Region)
		if err != nil {
			return false, fmt.Errorf("checking artifact for %s: %w", nodes[i].Name, err)
		}
		if !exists {
			log.FromContext(ctx).V(1).Info("artifact not yet uploaded",
				"node", nodes[i].Name, "key", key)
			return false, nil
		}
	}
	return true, nil
}

func (r *SeiNodeGroupReconciler) startAssembly(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, nodes []seiv1alpha1.SeiNode) (ctrl.Result, error) {
	plan, err := planner.BuildGroupAssemblyPlan(group, nodes)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building assembly plan: %w", err)
	}

	patch := client.MergeFrom(group.DeepCopy())
	group.Status.InitPlan = plan
	if err := r.Status().Patch(ctx, group, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting init plan: %w", err)
	}

	r.Recorder.Event(group, corev1.EventTypeNormal, "GenesisAssemblyStarted",
		"All artifacts uploaded, starting genesis assembly")
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *SeiNodeGroupReconciler) driveInitPlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, nodes []seiv1alpha1.SeiNode) (ctrl.Result, error) {
	if len(nodes) == 0 {
		return ctrl.Result{}, fmt.Errorf("no nodes available for assembly")
	}

	assemblerNode := &nodes[0]
	return r.PlanExecutor.ExecuteGroupPlan(ctx, group, group.Status.InitPlan, assemblerNode)
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
