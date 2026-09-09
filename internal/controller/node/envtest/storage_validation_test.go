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

// Admission-level coverage of spec.dataVolume.storage: which data-volume sizes
// the API server accepts, and which it rejects by name.
//
// The size is create-only because the PVC is provisioned once — ensure-data-pvc
// is Get-then-Create with no update path — so a later edit could never reach the
// volume. The check is split between a presence rule on SeiNodeSpec and a value
// rule on DataVolumeStorage; these cases cover both halves and the seam.

// nodeWithStorageSize returns a full node whose data volume asks for size. An
// empty size leaves the storage block off entirely.
func nodeWithStorageSize(ns, name, size string) *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  testChainID,
			Image:    testNodeImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	if size != "" {
		node.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
			Storage: &seiv1alpha1.DataVolumeStorage{
				Resources: &seiv1alpha1.VolumeClaimResources{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(size),
					},
				},
			},
		}
	}
	return node
}

// unstructuredNodeWithStorage returns a SeiNode as unstructured JSON so the size
// reaches the API server EXACTLY as written. The typed client cannot express
// these cases: apimachinery canonicalizes a Quantity on parse and always
// marshals it as a string, so a typed create can never put a bare JSON number,
// or two different spellings of one value, on the wire. kubectl apply can and
// does. See unstructuredNodeWithMemory for the same problem on the compute side.
//
// size is a raw JSON value: `"2Ti"` (quoted) or `2199023255552` (bare).
func unstructuredNodeWithStorage(ns, name, size string) *unstructured.Unstructured {
	raw := fmt.Sprintf(`{
	  "apiVersion": "sei.io/v1alpha1",
	  "kind": "SeiNode",
	  "metadata": {"name": %q, "namespace": %q},
	  "spec": {
	    "chainId": %q,
	    "image": %q,
	    "fullNode": {},
	    "dataVolume": {"storage": {"resources": {"requests": {"storage": %s}}}}
	  }
	}`, name, ns, testChainID, testNodeImage, size)

	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(raw), &u.Object); err != nil {
		panic(err) // a malformed literal in this file is a test bug, not a failure
	}
	return u
}

// TestDataVolumeStorage_SizeInClaimShapeAccepted covers the shape the harness
// renders: a size in the volume-claim shape, which is the whole surface of this
// iteration.
func TestDataVolumeStorage_SizeInClaimShapeAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	g.Expect(testCli.Create(testCtx, nodeWithStorageSize(ns, "dv-size", "500Gi"))).To(Succeed())
}

// TestDataVolumeStorage_UnsetAccepted is the no-regression case: the field is
// optional, and every node predating it must still be admissible.
func TestDataVolumeStorage_UnsetAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	g.Expect(testCli.Create(testCtx, nodeWithStorageSize(ns, "dv-unset", ""))).To(Succeed())
}

// TestDataVolumeStorage_WithImportRejected locks the mutual exclusion. An
// imported PVC keeps the importer's class and size and the controller never
// mutates it, so a size beside an import would read as applied while being
// ignored.
func TestDataVolumeStorage_WithImportRejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithStorageSize(ns, "dv-both", "500Gi")
	node.Spec.DataVolume.Import = &seiv1alpha1.DataVolumeImport{PVCName: "adopted-pvc"}

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "storage beside import must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
}

// TestDataVolumeStorage_LimitsRejected: a volume claim carries a request, not a
// limit. The limit is absent from the schema, so the API server rejects it as an
// unknown field rather than by a rule — either way it is named, which is what
// Req 2.4 asks for.
func TestDataVolumeStorage_LimitsRejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	raw := fmt.Sprintf(`{
	  "apiVersion": "sei.io/v1alpha1",
	  "kind": "SeiNode",
	  "metadata": {"name": "dv-limits", "namespace": %q},
	  "spec": {
	    "chainId": %q, "image": %q, "fullNode": {},
	    "dataVolume": {"storage": {"resources": {
	      "requests": {"storage": "500Gi"},
	      "limits": {"storage": "1Ti"}
	    }}}
	  }
	}`, ns, testChainID, testNodeImage)
	u := &unstructured.Unstructured{}
	g.Expect(json.Unmarshal([]byte(raw), &u.Object)).To(Succeed())

	err := testCli.Create(testCtx, u, client.FieldValidation(metav1.FieldValidationStrict))
	g.Expect(err).To(HaveOccurred(), "a limits block under a volume claim must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("limits"))
}

// TestDataVolumeStorage_StrayRequestKeyRejected keeps the claim to the one key
// that means something. The claim is a map rather than a struct precisely so
// this is a NAMED rejection rather than a silent prune — a pruned `storag` would
// provision the per-mode default size while the manifest read as though it asked
// for 2Ti, which is the failure this whole field is meant to avoid.
func TestDataVolumeStorage_StrayRequestKeyRejected(t *testing.T) {
	t.Run("a misspelled key alone is rejected", func(t *testing.T) {
		// The realistic typo: the operator writes one key and gets it wrong.
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := nodeWithStorageSize(ns, "dv-typo", "500Gi")
		node.Spec.DataVolume.Storage.Resources.Requests = corev1.ResourceList{
			"storag": resource.MustParse("2Ti"),
		}

		err := testCli.Create(testCtx, node)
		g.Expect(err).To(HaveOccurred(), "a key outside storage must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("accepts only storage"))
	})

	t.Run("a stray key beside the size is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := nodeWithStorageSize(ns, "dv-stray", "500Gi")
		node.Spec.DataVolume.Storage.Resources.Requests["iops"] = resource.MustParse("16000")

		err := testCli.Create(testCtx, node)
		g.Expect(err).To(HaveOccurred(), "an extra key must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("accepts only storage"))
	})
}

// TestDataVolumeStorage_NonPositiveSizeRejected: a zero or negative claim is not
// a smaller volume, it is an unprovisionable one.
func TestDataVolumeStorage_NonPositiveSizeRejected(t *testing.T) {
	for _, size := range []string{"0", "-1Gi"} {
		t.Run("size="+size, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)

			err := testCli.Create(testCtx, nodeWithStorageSize(ns, "dv-nonpos", size))
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("must be positive"))
		})
	}
}

// TestDataVolumeStorage_EquivalentUnitsAccepted is the case the create-only rule
// exists in quantity() form for. A node created with one spelling and re-applied
// with an equal one must be admitted, because it asks for no change at all.
//
// It has to go through the unstructured client — see unstructuredNodeWithStorage.
// If this starts failing, someone replaced compareTo with ==; restore the
// quantity() form rather than relaxing the test.
func TestDataVolumeStorage_EquivalentUnitsAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := unstructuredNodeWithStorage(ns, "dv-equiv", `"2048Gi"`)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	// Re-apply the same size spelled as 2Ti. Same quantity, different string.
	cur := unstructuredNodeWithStorage(ns, "dv-equiv", `"2Ti"`)
	cur.SetResourceVersion(node.GetResourceVersion())
	g.Expect(testCli.Update(testCtx, cur)).To(Succeed(),
		"2048Gi and 2Ti are the same size; the rule must compare quantities, not strings")
}

// TestDataVolumeStorage_BareIntSizeSurvivesControllerReencode reproduces the
// finalizer wedge a structural create-only rule would cause, for the size.
//
// A node applied with a bare-integer size stores an int in etcd. The
// controller's first reconcile installs its finalizer with a typed Update, which
// re-encodes the size as a string. A structural == reads int != string, rejects
// the controller's own write, and the node never gets a finalizer — it wedges
// before it ever provisions. The quantity()-based rule compares values, so the
// re-encode is admitted. This is the case that fails against a structural rule;
// it is mutation-checked.
func TestDataVolumeStorage_BareIntSizeSurvivesControllerReencode(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	// 500Gi in bytes as a bare JSON integer — the spelling that stores an int.
	node := unstructuredNodeWithStorage(ns, "dv-reencode", `536870912000`)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Paused = true // any typed write re-marshals the size as a string
	})
	g.Expect(err).NotTo(HaveOccurred(),
		"a typed re-encode of a bare-int size must not trip the create-only rule (the finalizer Update depends on it)")
}

// TestDataVolumeStorage_CreateOnlyGate covers the three edits the create-only
// pair must reject, and the unrelated edit it must not.
func TestDataVolumeStorage_CreateOnlyGate(t *testing.T) {
	t.Run("changing the size is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := nodeWithStorageSize(ns, "dv-change", "500Gi")
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.DataVolume.Storage.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Ti")
		})
		g.Expect(err).To(HaveOccurred(), "resizing after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("create-only"))
	})

	t.Run("adding a size after create is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		// The case a rule on the sub-type alone would miss: with no storage in
		// the stored object, a transition rule there never fires. By this point
		// the PVC is already provisioned at the per-mode size, so an added size
		// is inert — which is exactly why it must not be accepted.
		node := nodeWithStorageSize(ns, "dv-add", "")
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
				Storage: &seiv1alpha1.DataVolumeStorage{
					Resources: &seiv1alpha1.VolumeClaimResources{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("500Gi"),
						},
					},
				},
			}
		})
		g.Expect(err).To(HaveOccurred(), "adding a size after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("create-only"))
	})

	t.Run("removing the size is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := nodeWithStorageSize(ns, "dv-remove", "500Gi")
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.DataVolume = nil
		})
		g.Expect(err).To(HaveOccurred(), "removing the size after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("create-only"))
	})

	t.Run("an unrelated edit is still accepted", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := nodeWithStorageSize(ns, "dv-unrelated", "500Gi")
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.Paused = true
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"an edit leaving the size unchanged must be accepted")
	})

	t.Run("adding dataVolume.import after create is still allowed", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		// The size gate is scoped to the size. Import adoption on a node that
		// never had a dataVolume is pre-existing behaviour and must survive.
		node := nodeWithStorageSize(ns, "dv-import-add", "")
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
				Import: &seiv1alpha1.DataVolumeImport{PVCName: "adopted-pvc"},
			}
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"adding an import after create must stay allowed — the gate covers the size only")
	})
}
