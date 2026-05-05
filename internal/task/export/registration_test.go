package export

import (
	"encoding/json"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// TestAwaitJobDeserializerParity asserts that the two registered task types
// (await-bootstrap-job and await-export-job) resolve to the same underlying
// awaitJobExecution shape. The two tasks intentionally share an
// implementation (parameterized by JobName); a refactor that diverges them
// silently would only surface in production.
func TestAwaitJobDeserializerParity(t *testing.T) {
	body := json.RawMessage(`{"jobName":"foo","namespace":"bar"}`)
	cfg := task.ExecutionConfig{}

	bootstrapExec, err := task.Deserialize(TaskTypeAwaitBootstrapJob, "id-1", body, cfg)
	if err != nil {
		t.Fatalf("Deserialize await-bootstrap-job: %v", err)
	}
	exportExec, err := task.Deserialize(TaskTypeAwaitExportJob, "id-2", body, cfg)
	if err != nil {
		t.Fatalf("Deserialize await-export-job: %v", err)
	}

	if _, ok := bootstrapExec.(*awaitJobExecution); !ok {
		t.Errorf("await-bootstrap-job resolved to %T, want *awaitJobExecution", bootstrapExec)
	}
	if _, ok := exportExec.(*awaitJobExecution); !ok {
		t.Errorf("await-export-job resolved to %T, want *awaitJobExecution", exportExec)
	}
}
