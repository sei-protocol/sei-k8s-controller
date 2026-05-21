package taskruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestScenarioYAMLs_CMNameMatchesWorkflowVarsName guards the contract
// surfaced by sei-protocol/sei-k8s-controller#337: scenario YAML's
// envFrom configMapRef.name MUST match what WorkflowVarsName produces
// for the scenario's own Workflow CR name. The first manual fire of
// release-test stuck a Task pod in CreateContainerConfigError for ~8m
// because the scenario referenced workflow-vars-<run-id> but keygen
// created workflow-vars-<workflow-name>.
//
// Scenarios using the seitask binary's typed CM helpers must opt in to
// this test by appearing in `scenariosToCheck` below. Bash-driven
// scenarios (e.g., major-upgrade.yaml today) are excluded — they
// create the CM via kubectl with their own naming convention.
func TestScenarioYAMLs_CMNameMatchesWorkflowVarsName(t *testing.T) {
	scenariosDir, err := filepath.Abs("../../scenarios")
	if err != nil {
		t.Fatal(err)
	}
	scenariosToCheck := []string{"release-test.yaml", "load-test.yaml"}

	for _, name := range scenariosToCheck {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(scenariosDir, name))
			if err != nil {
				t.Fatal(err)
			}
			workflowName := workflowMetaName(t, raw)
			wantCMName := WorkflowVarsName(workflowName)
			cmRefs := configMapRefNames(t, raw)
			if len(cmRefs) == 0 {
				t.Fatalf("no envFrom configMapRef names found — scenario isn't exercising the bridge")
			}
			for i, got := range cmRefs {
				if got != wantCMName {
					t.Errorf("configMapRef[%d].name = %q; want %q (workflow %q)", i, got, wantCMName, workflowName)
				}
			}
		})
	}
}

// workflowMetaName extracts the first `kind: Workflow` document's
// metadata.name from a multi-doc scenario YAML.
func workflowMetaName(t *testing.T, raw []byte) string {
	t.Helper()
	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			continue
		}
		if head.Kind == "Workflow" && head.Metadata.Name != "" {
			return head.Metadata.Name
		}
	}
	t.Fatal("no kind=Workflow document with metadata.name found")
	return ""
}

// configMapRefNames pulls every envFrom configMapRef.name in the scenario.
// Walks the Workflow.spec.templates[].task.container.envFrom path; using a
// regex over the raw YAML keeps the test independent of the chaos-mesh
// Go types.
func configMapRefNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var names []string
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "configMapRef:") {
			continue
		}
		// Next non-blank line should be `  name: <value>` at deeper indent.
		for j := i + 1; j < len(lines) && j < i+4; j++ {
			trimmed := strings.TrimSpace(lines[j])
			if rest, ok := strings.CutPrefix(trimmed, "name:"); ok {
				names = append(names, strings.TrimSpace(rest))
				break
			}
		}
	}
	return names
}
