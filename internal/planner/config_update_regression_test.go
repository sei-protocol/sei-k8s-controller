package planner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

// Model the real start wrapper: restart cannot finish while healthz is 503.
// Mark-ready is fire-and-forget, so approval takes effect only when the restart
// is polled here, rather than assuming synchronous completion on submission.
type startGateSidecar struct {
	mockSidecarClient
	ready                 bool
	loseDuringValidation  bool
	approvalSubmitted     bool
	restartBeforeApproval bool
	restartID             uuid.UUID
}

func (s *startGateSidecar) Healthz(context.Context) (bool, error) { return s.ready, nil }

func (s *startGateSidecar) SubmitTask(ctx context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	switch req.Type {
	case TaskConfigValidate:
		if s.loseDuringValidation {
			s.ready = false
			s.approvalSubmitted = false
		}
	case TaskMarkReady:
		s.approvalSubmitted = true
	case sidecar.TaskTypeRestartSeid:
		s.restartID = *req.Id
		s.restartBeforeApproval = !s.approvalSubmitted
	}
	return s.mockSidecarClient.SubmitTask(ctx, req)
}

func (s *startGateSidecar) GetTask(_ context.Context, id uuid.UUID) (*sidecar.TaskResult, error) {
	status := sidecar.Completed
	if id == s.restartID {
		s.ready = s.approvalSubmitted
		if !s.ready {
			status = sidecar.Running
		}
	}
	return &sidecar.TaskResult{Id: id, Status: status}, nil
}

func TestConfigRestartReleases503StartGateBeforeWaitingForSeid(t *testing.T) {
	for _, midPlan := range []bool{false, true} {
		name := "sidecar-already-not-ready"
		if midPlan {
			name = "sidecar-loses-readiness-during-validation"
		}
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			node := runningFullNode()
			node.Status.CurrentConfigValuesHash = configUpdateOldHash
			sc := &startGateSidecar{ready: midPlan, loseDuringValidation: midPlan}
			resolver := &NodeResolver{BuildSidecarClient: func(*seiv1alpha1.SeiNode) (task.SidecarClient, error) { return sc, nil }}
			g.Expect(resolver.ResolvePlan(ctx, node)).To(Succeed())
			plan := node.Status.Plan
			g.Expect(plan).NotTo(BeNil())
			g.Expect(planTaskTypes(plan)).To(ContainElement(sidecar.TaskTypeRestartSeid))
			if !midPlan {
				g.Expect(sidecarNeedsReapproval(node)).To(BeTrue())
			}
			_, err := executePlan(ctx, node, plan, task.ExecutionConfig{
				BuildSidecarClient: func() (task.SidecarClient, error) { return sc, nil },
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sc.restartBeforeApproval).To(BeFalse())
			g.Expect(sc.ready).To(BeTrue())
			g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
			g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(plan.ConfigValuesHash))
		})
	}
}

func TestFirstConfigObservationRegeneratesRemovedKeysOnImageUpdate(t *testing.T) {
	for _, file := range []string{configUpdateFile, overlayTestAppFile} {
		t.Run(file, func(t *testing.T) {
			g := NewWithT(t)
			node := runningFullNode()
			node.Status.CurrentConfigValuesHash = ""
			node.Spec.Image = testImageV2
			// The piece-1 overlay existed on disk, but the desired set is now empty.
			home := t.TempDir()
			g.Expect(seiconfig.WriteConfigToDir(seiconfig.DefaultForMode(seiconfig.ModeFull), home)).To(Succeed())
			path := filepath.Join(home, "config", file)
			old, err := tomlpatch.ReadTOML(path)
			g.Expect(err).NotTo(HaveOccurred())
			old["removed-piece-one-key"] = true
			g.Expect(tomlpatch.WriteTOML(path, old)).To(Succeed())
			g.Expect(configValuesDrifted(node)).To(BeFalse(), "empty baseline must not itself trigger a rollout")
			plan, err := (&fullNodePlanner{}).BuildPlan(node)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(node.Status.CurrentConfigValuesHash).To(BeEmpty())
			// Roundtrip the persisted plan and execute its materialization payloads
			// through the same sei-config writer and TOML merger the sidecar uses.
			raw, err := json.Marshal(plan)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(json.Unmarshal(raw, &plan)).To(Succeed())
			regenerations := 0
			for i, pt := range plan.Tasks {
				switch pt.Type {
				case TaskConfigApply:
					var intent seiconfig.ConfigIntent
					g.Expect(json.Unmarshal(pt.Params.Raw, &intent)).To(Succeed())
					resolved, err := seiconfig.ResolveIntent(intent)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(resolved.Valid).To(BeTrue())
					g.Expect(seiconfig.WriteConfigToDir(resolved.Config, home)).To(Succeed())
					regenerations++
				case TaskConfigPatch:
					g.Expect(regenerations).To(Equal(1), "base must be regenerated before patches")
					var patch task.ConfigPatchTask
					g.Expect(json.Unmarshal(pt.Params.Raw, &patch)).To(Succeed())
					for name, values := range patch.Files {
						target := filepath.Join(home, "config", name)
						base, err := tomlpatch.ReadTOML(target)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(tomlpatch.WriteTOML(target, tomlpatch.Merge(base, values).(map[string]any))).To(Succeed())
					}
				}
				plan.Tasks[i].Status = seiv1alpha1.TaskComplete
			}
			g.Expect(regenerations).To(Equal(1))
			restored, err := tomlpatch.ReadTOML(path)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(restored).NotTo(HaveKey("removed-piece-one-key"))
			g.Expect(node.Status.CurrentConfigValuesHash).To(BeEmpty())
			_, err = executePlan(context.Background(), node, plan, task.ExecutionConfig{})
			g.Expect(err).NotTo(HaveOccurred())
			emptyHash, err := configValuesHash(nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(node.Status.CurrentConfigValuesHash).To(Equal(emptyHash))
			g.Expect(configValuesDrifted(node)).To(BeFalse())
		})
	}
}

func TestUnobservedConfigNoOpExplainsDeferredNonemptyEdits(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = ""
	node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
	resolver := &NodeResolver{}
	g.Expect(resolver.ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
	condition := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(condition).NotTo(BeNil())
	g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(condition.Reason).To(Equal("ConfigBaselineUnobserved"))
	g.Expect(condition.Message).To(ContainSubstring("image update"))
	g.Expect(node.Status.CurrentConfigValuesHash).To(BeEmpty())
	// An ordinary image update establishes the baseline and clears the
	// deferred explanation through the existing condition lifecycle.
	node.Spec.Image = testImageV2
	g.Expect(resolver.ResolvePlan(context.Background(), node)).To(Succeed())
	for i := range node.Status.Plan.Tasks {
		node.Status.Plan.Tasks[i].Status = seiv1alpha1.TaskComplete
	}
	_, err := executePlan(context.Background(), node, node.Status.Plan, task.ExecutionConfig{})
	g.Expect(err).NotTo(HaveOccurred())
	node.Status.CurrentImage = node.Spec.Image
	g.Expect(resolver.ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
	condition = meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(condition.Reason).To(Equal("UpdateComplete"))
}

func TestUnobservedNodeWithoutConfigValuesNeverGetsBaselineNotice(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = ""
	resolver := &NodeResolver{}
	for range 2 {
		g.Expect(resolver.ResolvePlan(context.Background(), node)).To(Succeed())
		g.Expect(node.Status.Plan).To(BeNil())
		g.Expect(meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)).To(BeNil())
	}
}

func TestFailedUpdateReasonAndMessageSurviveUnobservedConfigReconciles(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.CurrentConfigValuesHash = ""
	node.Spec.ConfigValues = overlayTestNode().Spec.ConfigValues
	node.Spec.Image = testImageV2
	resolver := &NodeResolver{}
	g.Expect(resolver.ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(plan).NotTo(BeNil())
	// observe-image succeeded, but the following start-gate approval failed.
	node.Status.CurrentImage = node.Spec.Image
	plan.Phase = seiv1alpha1.TaskPlanFailed
	plan.FailedTaskDetail = &seiv1alpha1.FailedTaskInfo{Type: TaskMarkReady, Error: "approval denied"}
	wantMessage := "plan " + plan.ID + " failed: task " + TaskMarkReady + ": approval denied"
	for range 2 {
		g.Expect(resolver.ResolvePlan(context.Background(), node)).To(Succeed())
		g.Expect(node.Status.Plan).To(BeNil())
		g.Expect(node.Status.CurrentConfigValuesHash).To(BeEmpty())
		condition := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(condition.Reason).To(Equal("UpdateFailed"))
		g.Expect(condition.Message).To(Equal(wantMessage))
	}
}

func TestClassifyPlanIncludesConfigUpdates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		types []string
		want  string
	}{
		{"config", []string{TaskConfigApply, TaskConfigPatch, TaskConfigValidate, TaskMarkReady, sidecar.TaskTypeRestartSeid}, "config-update"},
		{"image", []string{TaskConfigPatch, TaskConfigValidate, task.TaskTypeObserveImage, TaskMarkReady}, "node-update"},
		{"bootstrap", []string{task.TaskTypeEnsureDataPVC, TaskConfigApply, TaskMarkReady}, "init"},
		{"reapproval", []string{TaskMarkReady}, "mark-ready-reapply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			plan := &seiv1alpha1.TaskPlan{}
			for _, taskType := range tc.types {
				plan.Tasks = append(plan.Tasks, seiv1alpha1.PlannedTask{Type: taskType})
			}
			g.Expect(classifyPlan(plan)).To(Equal(tc.want))
		})
	}
}
