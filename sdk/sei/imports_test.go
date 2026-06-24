package sei_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestCoreContractStaysDependencyLight enforces the contract-seam invariant: the
// core sei package (Provider/*Handle interfaces, *Spec structs, registry) is the
// dependency-light surface a consumer's in-process harness conforms to by shape,
// so it must not pull controller-runtime / apimachinery / CRD code into its
// transitive imports. Those belong to the k8s provider package alone; the
// concrete object reaches callers only via Object() any. A drifted import here
// fails the build instead of silently coupling the seam to k8s.
func TestCoreContractStaysDependencyLight(t *testing.T) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-deps", "github.com/sei-protocol/sei-k8s-controller/sdk/sei").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	banned := []string{
		"sigs.k8s.io/controller-runtime",
		"k8s.io/apimachinery",
		"k8s.io/client-go",
		"github.com/sei-protocol/sei-k8s-controller/api",
	}
	for dep := range strings.FieldsSeq(string(out)) {
		for _, b := range banned {
			if dep == b || strings.HasPrefix(dep, b+"/") {
				t.Errorf("core sei package transitively imports %q (banned: %q) — the contract seam must stay dependency-light", dep, b)
			}
		}
	}
}
