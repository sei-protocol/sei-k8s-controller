package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/template"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

const (
	// FieldOwner is the field-manager string used for server-side apply.
	// Distinct from "seinode-task-controller" so apply diffs are attributable
	// to the runner versus the reconciler.
	FieldOwner = "seitask-runner"

	// shortHashLen is the number of hex chars taken from the SHA-256 of the
	// (kind|vars|node) tuple to form the metadata.name suffix.
	shortHashLen = 10
)

// DefaultRenderer renders Go text/template files. The resulting manifest is
// parsed back to assert it is a SeiNodeTask, and metadata.name is
// rewritten to a deterministic value derived from (kind, vars, NODE) so
// re-applies hit the same CR. When OwnerRef is non-nil, it replaces (not
// merges) ownerReferences so the rendered SeiNodeTask cascades on parent
// Workflow deletion.
type DefaultRenderer struct {
	// OwnerRef, when non-nil, is stamped onto the rendered manifest as
	// the sole entry of metadata.ownerReferences. The runner subcommand
	// populates it from taskruntime.LoadWorkflowIdentity at startup.
	OwnerRef *metav1.OwnerReference
}

// Render parses templatePath as a Go text/template and executes it against
// vars. The template author can use {{ .NODE }}, {{ .PROPOSAL_ID }}, etc.
// All keys from vars are exposed as top-level fields with .KEY.
//
// After rendering, the metadata.name is replaced with
// "<kind-kebab>-<NODE>-<short-hash>" (NODE omitted if empty), where the hash
// covers the template content + sorted vars. This guarantees re-applies
// with identical inputs target the same CR (Workflow restart idempotency).
func (r DefaultRenderer) Render(templatePath string, vars map[string]string) ([]byte, string, error) {
	raw, err := os.ReadFile(templatePath) //nolint:gosec // path is operator-controlled CLI arg
	if err != nil {
		return nil, "", fmt.Errorf("read template: %w", err)
	}
	return RenderBytes(templatePath, raw, vars, r.OwnerRef)
}

// RenderBytes is the byte-input variant of Render, exposed for tests.
// When ownerRef is non-nil, it replaces (not merges) ownerReferences on
// the rendered manifest.
func RenderBytes(name string, raw []byte, vars map[string]string, ownerRef *metav1.OwnerReference) ([]byte, string, error) {
	tmpl, err := template.New(name).
		Option("missingkey=error").
		Parse(string(raw))
	if err != nil {
		return nil, "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, "", fmt.Errorf("execute template: %w", err)
	}

	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(buf.Bytes(), &obj.Object); err != nil {
		return nil, "", fmt.Errorf("parse rendered manifest: %w", err)
	}
	if obj.GetKind() != "SeiNodeTask" {
		return nil, "", fmt.Errorf("rendered manifest is %s, want SeiNodeTask", obj.GetKind())
	}

	// Discover the spec.kind so the deterministic name carries the
	// per-kind prefix.
	specKind, _, err := unstructured.NestedString(obj.Object, "spec", "kind")
	if err != nil || specKind == "" {
		return nil, "", fmt.Errorf("spec.kind missing on rendered manifest")
	}

	deterministic := DeterministicName(specKind, vars, raw)
	obj.SetName(deterministic)

	// Replace (not merge) ownerReferences so a template that smuggles a
	// bogus ref can't leak through. Mirrors provisionsnd.stampMetadata.
	if ownerRef != nil {
		obj.SetOwnerReferences([]metav1.OwnerReference{*ownerRef})
	}

	out, err := yaml.Marshal(obj.Object)
	if err != nil {
		return nil, "", fmt.Errorf("re-marshal manifest: %w", err)
	}
	return out, deterministic, nil
}

// DeterministicName produces a stable metadata.name from
// (spec.kind, vars, template-content). Format:
//
//	<kind-kebab>[-<NODE>]-<10-hex>
//
// NODE is included as a human-readable infix when present in vars; the hash
// alone already provides uniqueness (template content + sorted vars), so the
// infix is purely operator ergonomics for `kubectl get snt`.
func DeterministicName(specKind string, vars map[string]string, templateContent []byte) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write(templateContent)
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(vars[k]))
	}
	sum := hex.EncodeToString(h.Sum(nil))[:shortHashLen]

	prefix := kebab(specKind)
	parts := []string{prefix}
	if node := vars["NODE"]; node != "" {
		parts = append(parts, sanitizeForDNS(node))
	}
	parts = append(parts, sum)
	return strings.Join(parts, "-")
}

// kebab converts CamelCase to kebab-case (GovSoftwareUpgrade ->
// gov-software-upgrade). Plain ASCII only.
func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeForDNS replaces characters that aren't DNS-1123 label safe with
// '-'. Truncates to 40 chars to keep the final name under K8s' 253-byte
// resource name limit even with a long kind prefix.
func sanitizeForDNS(s string) string {
	const maxLen = 40
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		default:
			b = append(b, '-')
		}
	}
	if len(b) > maxLen {
		b = b[:maxLen]
	}
	// Trim trailing '-' so the joined name doesn't end with "--<hash>".
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	return string(b)
}

// DynamicApplier implements Applier against the K8s dynamic client.
// Server-side apply is used so re-applies are no-ops at the apiserver level.
type DynamicApplier struct {
	Client dynamic.Interface
}

// SeiNodeTaskGVR is the GroupVersionResource for SeiNodeTask.
var SeiNodeTaskGVR = schema.GroupVersionResource{
	Group:    "sei.io",
	Version:  "v1alpha1",
	Resource: "seinodetasks",
}

// Apply performs server-side apply of the rendered manifest. The manifest
// must already carry metadata.name; the namespace is taken from the runner's
// pod namespace.
func (a DynamicApplier) Apply(ctx context.Context, namespace string, manifest []byte) error {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(manifest, &obj.Object); err != nil {
		return fmt.Errorf("parse manifest for apply: %w", err)
	}
	obj.SetNamespace(namespace)

	data, err := obj.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal manifest for apply: %w", err)
	}
	force := true
	_, err = a.Client.
		Resource(SeiNodeTaskGVR).
		Namespace(namespace).
		Patch(ctx, obj.GetName(), apiTypesApplyPatch, data, metav1.PatchOptions{
			FieldManager: FieldOwner,
			Force:        &force,
		})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("apply SeiNodeTask %q: %w", obj.GetName(), err)
	}
	return nil
}

// apiTypesApplyPatch is a local alias so we don't pull k8s.io/apimachinery/pkg/types
// in the type signature of Apply (kept on a separate line for grep-ability).
var apiTypesApplyPatch = applyPatchTypeMarker()
