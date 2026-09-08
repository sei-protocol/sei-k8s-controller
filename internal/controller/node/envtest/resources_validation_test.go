//go:build envtest

package envtest_test

import (
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Admission-level coverage of spec.resources: which footprints the API server
// accepts, and which it rejects by name.
//
// These cases need no controller. The CEL rules on SeidResources are the whole
// subject, and only a real API server evaluates them — the fake client does
// not — so a failure here is a CRD-contract defect and never a reconcile bug.
//
// The rules exist to move two long-standing per-mode couplings from "silently
// normalized during reconcile" to "named at apply time": seid carries no CPU
// limit, and its memory limit equals its memory request.

// The chain ID and image every node in this suite is built with. The sibling
// validation files still inline them; hoisting those is unrelated churn.
const (
	testChainID   = "envtest-1"
	testNodeImage = "sei:latest"
)

// nodeWithResources returns a full node carrying the given resource block. A nil
// block leaves spec.resources unset.
func nodeWithResources(ns, name string, res *seiv1alpha1.SeidResources) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   testChainID,
			Image:     testNodeImage,
			FullNode:  &seiv1alpha1.FullNodeSpec{},
			Resources: res,
		},
	}
}

// TestSeidResources_UnsetAccepted is the no-regression case: the field is
// optional, and every node that predates it must still be admissible.
func TestSeidResources_UnsetAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	g.Expect(testCli.Create(testCtx, nodeWithResources(ns, "res-unset", nil))).To(Succeed())
}

// TestSeidResources_RequestsOnlyAccepted covers the shape the harness renders:
// a CPU and memory request, no limits block. The controller derives the memory
// limit from the request, so this is the complete and preferred spelling.
func TestSeidResources_RequestsOnlyAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithResources(ns, "res-requests", &seiv1alpha1.SeidResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
	})
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
}

// TestSeidResources_CPULimitRejected locks the no-CPU-limit coupling at
// admission. seid is work-conserving and CPU is compressible, so a CPU limit
// only throttles its consensus/replay bursts — the API server must name the
// field rather than let the controller quietly drop it.
func TestSeidResources_CPULimitRejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithResources(ns, "res-cpu-limit", &seiv1alpha1.SeidResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
	})

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "a CPU limit must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("resources.limits accepts only memory"))
}

// TestSeidResources_StrayRequestKeyRejected keeps the request surface to the two
// keys the seid container can actually be sized on. A GPU or an extended
// resource here would schedule against a device plugin that no Sei nodepool runs.
func TestSeidResources_StrayRequestKeyRejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithResources(ns, "res-stray-key", &seiv1alpha1.SeidResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
	})

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "a resource name outside cpu/memory must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("resources.requests accepts only cpu and memory"))
}

// TestSeidResources_UnequalMemoryRejected locks the memory-Guaranteed coupling:
// the footprint is hard-reserved and hard-capped at the same value, so a limit
// above the request is not a permissive setting — it is a different memory model.
func TestSeidResources_UnequalMemoryRejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithResources(ns, "res-unequal-mem", &seiv1alpha1.SeidResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("64Gi"),
		},
	})

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "a memory limit above the request must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("resources.limits.memory must equal resources.requests.memory"))
}

// TestSeidResources_MemoryLimitWithoutRequestRejected closes the hole the
// equality rule would otherwise leave: a limit with no request to compare
// against is not "equal by default", it is unresolvable, and the controller
// would silently size the node off a lower source while the limit read as
// authoritative.
func TestSeidResources_MemoryLimitWithoutRequestRejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithResources(ns, "res-limit-no-req", &seiv1alpha1.SeidResources{
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
	})

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "a memory limit with no memory request must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("resources.limits.memory must equal resources.requests.memory"))
}

// unstructuredNodeWithMemory returns a SeiNode as unstructured JSON, so the
// memory request and limit reach the API server EXACTLY as written.
//
// The typed client cannot express these cases. apimachinery's Quantity
// canonicalizes on parse — resource.MustParse("131072Mi") marshals back as
// "128Gi", and every Quantity marshals as a JSON string even when constructed
// from an int64 — so a typed create can never put two different spellings, or a
// bare JSON number, on the wire. kubectl apply can and does: it ships the YAML
// scalar through the unstructured path untouched. Operators debug with kubectl,
// so that path is the one the rule has to survive.
//
// req and lim are raw JSON values: `"131072Mi"` (quoted) or `34359738368` (bare).
func unstructuredNodeWithMemory(ns, name, req, lim string) *unstructured.Unstructured {
	raw := fmt.Sprintf(`{
	  "apiVersion": "sei.io/v1alpha1",
	  "kind": "SeiNode",
	  "metadata": {"name": %q, "namespace": %q},
	  "spec": {
	    "chainId": %q,
	    "image": %q,
	    "fullNode": {},
	    "resources": {
	      "requests": {"cpu": "4", "memory": %s},
	      "limits": {"memory": %s}
	    }
	  }
	}`, name, ns, testChainID, testNodeImage, req, lim)

	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(raw), &u.Object); err != nil {
		panic(err) // a malformed literal in this file is a test bug, not a failure
	}
	return u
}

// TestSeidResources_EquivalentMemoryUnitsAccepted is the test the equality rule
// exists in its awkward form for. "128Gi" and "131072Mi" are the same quantity
// spelled two ways, and a naive `self.limits.memory == self.requests.memory`
// compares the STRINGS and rejects this valid pair.
//
// It must go through the unstructured client — see unstructuredNodeWithMemory
// for why a typed create cannot reach this case. If this test starts failing,
// someone simplified the rule to ==; restore the quantity().compareTo() form
// rather than relaxing the test.
func TestSeidResources_EquivalentMemoryUnitsAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := unstructuredNodeWithMemory(ns, "res-equiv-units", `"131072Mi"`, `"128Gi"`)

	g.Expect(testCli.Create(testCtx, node)).To(Succeed(),
		"128Gi and 131072Mi are the same quantity; the rule must compare quantities, not strings")
}

// TestSeidResources_IntegerMemorySpellingAccepted covers the other spelling a
// Quantity admits. It generates as x-kubernetes-int-or-string, so a bare JSON
// integer of bytes is legal and reaches CEL as an int — on which quantity() has
// no overload. Without the string() wrapper in the rule this valid pair does not
// merely fail, it fails to EVALUATE, and the operator gets a CEL internal error
// instead of a verdict.
func TestSeidResources_IntegerMemorySpellingAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	// 32Gi in bytes, unquoted: the int branch of int-or-string.
	node := unstructuredNodeWithMemory(ns, "res-int-mem", `34359738368`, `34359738368`)

	g.Expect(testCli.Create(testCtx, node)).To(Succeed(),
		"a bare-integer memory quantity must evaluate, not error out of the CEL rule")
}

// TestSeidResources_UnequalMemoryRejectedOnRawPath re-checks the reject side on
// the same unstructured path, so the rule is known to discriminate there rather
// than merely to admit everything it is handed.
func TestSeidResources_UnequalMemoryRejectedOnRawPath(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := unstructuredNodeWithMemory(ns, "res-raw-unequal", `"32Gi"`, `"64Gi"`)

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resources.limits.memory must equal resources.requests.memory"))
}

// TestSeidResources_RaisedAfterCreateAccepted pins the deliberate absence of an
// immutability rule. Compute is a pod-template field, and the benchmark loop is
// "raise it and re-run", so admission must accept the edit. What the edit does
// NOT do is roll a live pod — the StatefulSets are OnDelete — which is a
// controller-behaviour question, not an admission one.
func TestSeidResources_RaisedAfterCreateAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithResources(ns, "res-raise", &seiv1alpha1.SeidResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
	})
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("32")
	})
	g.Expect(err).NotTo(HaveOccurred(), "raising the shape after create must be accepted")
}
