package k8s

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// buildClient resolves a controller-runtime client from the ambient kubeconfig
// chain (--kubeconfig/$KUBECONFIG/in-cluster/SA-namespace), re-deriving the
// loader behavior of seictl's internal cliutil rather than importing it (LLD
// §5.1). No caller-supplied connection string (§3.1). It also resolves the
// default namespace from $POD_NAMESPACE / the SA-namespace file, falling back to
// "default".
func buildClient() (ctrlclient.Client, string, error) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, "", fmt.Errorf("resolving kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := seiv1alpha1.AddToScheme(scheme); err != nil {
		return nil, "", fmt.Errorf("registering sei scheme: %w", err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, "", fmt.Errorf("building client: %w", err)
	}
	return c, defaultNamespace(saNamespaceFile), nil
}

// saNamespaceFile is the in-cluster service-account namespace projection.
const saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// defaultNamespace resolves the namespace a spec with Namespace=="" lands in:
// $POD_NAMESPACE, then the SA-namespace file at saFile, then "default".
func defaultNamespace(saFile string) string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile(saFile); err == nil {
		// The SA-namespace file carries a trailing newline; trim it so the
		// namespace doesn't pick up a stray "\n".
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}
