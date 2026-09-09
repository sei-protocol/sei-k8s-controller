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

// Admission coverage of spec.dataVolume.storage. The size is create-only (the
// PVC is provisioned once): presence rule on the spec, value rule on the sub-type.

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

// unstructuredNodeWithStorage sends size as raw JSON (`"2Ti"` or `2199023255552`);
// a typed client canonicalizes a Quantity, so it cannot spell a bare int.
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

func TestDataVolumeStorage_SizeInClaimShapeAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	g.Expect(testCli.Create(testCtx, nodeWithStorageSize(ns, "dv-size", "500Gi"))).To(Succeed())
}

func TestDataVolumeStorage_UnsetAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	g.Expect(testCli.Create(testCtx, nodeWithStorageSize(ns, "dv-unset", ""))).To(Succeed())
}

// A request pruning to empty (a Helm value templated to `storage:`) would pass
// the rules vacuously; `storage: {}` is the legitimate no-override case.
func TestDataVolumeStorage_EmptyOrNullSizeRejected(t *testing.T) {
	ns := makeNamespace(t)
	for name, requests := range map[string]string{
		"null-value":     `{"storage": null}`,
		"empty-requests": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			raw := fmt.Sprintf(`{
			  "apiVersion": "sei.io/v1alpha1",
			  "kind": "SeiNode",
			  "metadata": {"name": "dv-empty-%s", "namespace": %q},
			  "spec": {
			    "chainId": %q, "image": %q, "fullNode": {},
			    "dataVolume": {"storage": {"resources": {"requests": %s}}}
			  }
			}`, name, ns, testChainID, testNodeImage, requests)
			u := &unstructured.Unstructured{}
			g.Expect(json.Unmarshal([]byte(raw), &u.Object)).To(Succeed())

			err := testCli.Create(testCtx, u)
			g.Expect(err).To(HaveOccurred(), "an empty/null storage request must be rejected, not silently defaulted")
			g.Expect(err.Error()).To(ContainSubstring("must carry resources.requests.storage"))
		})
	}
	t.Run("storage block with no resources is accepted", func(t *testing.T) {
		g := NewWithT(t)
		raw := fmt.Sprintf(`{
		  "apiVersion": "sei.io/v1alpha1",
		  "kind": "SeiNode",
		  "metadata": {"name": "dv-empty-storage", "namespace": %q},
		  "spec": {"chainId": %q, "image": %q, "fullNode": {}, "dataVolume": {"storage": {}}}
		}`, ns, testChainID, testNodeImage)
		u := &unstructured.Unstructured{}
		g.Expect(json.Unmarshal([]byte(raw), &u.Object)).To(Succeed())
		g.Expect(testCli.Create(testCtx, u)).To(Succeed(), "storage:{} is the no-override case, still accepted")
	})
}

func TestDataVolumeStorage_WithImportRejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := nodeWithStorageSize(ns, "dv-both", "500Gi")
	node.Spec.DataVolume.Import = &seiv1alpha1.DataVolumeImport{PVCName: "adopted-pvc"}

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "storage beside import must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
}

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

// A map, not a struct, so a misspelled key is named rather than silently pruned.
func TestDataVolumeStorage_StrayRequestKeyRejected(t *testing.T) {
	t.Run("a misspelled key alone is rejected", func(t *testing.T) {
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

// Why the rule uses quantity(): an equal size spelled differently is no change.
func TestDataVolumeStorage_EquivalentUnitsAccepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := unstructuredNodeWithStorage(ns, "dv-equiv", `"2048Gi"`)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	cur := unstructuredNodeWithStorage(ns, "dv-equiv", `"2Ti"`)
	cur.SetResourceVersion(node.GetResourceVersion())
	g.Expect(testCli.Update(testCtx, cur)).To(Succeed(),
		"2048Gi and 2Ti are the same size; the rule must compare quantities, not strings")
}

// A bare-int size stores an int the finalizer Update re-encodes as a string, so
// == would reject the controller's own write. No manager here, so the typed
// write stands in; the SeiNetwork twin proves the real path.
func TestDataVolumeStorage_BareIntSizeSurvivesControllerReencode(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := unstructuredNodeWithStorage(ns, "dv-reencode", `536870912000`)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Paused = true // any typed write re-marshals the size as a string
	})
	g.Expect(err).NotTo(HaveOccurred(),
		"a typed re-encode of a bare-int size must not trip the create-only rule (the finalizer Update depends on it)")
}

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

	// Shrink too, so the comparison cannot degrade to grow-only.
	t.Run("shrinking the size is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := nodeWithStorageSize(ns, "dv-shrink", "2Ti")
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.DataVolume.Storage.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("500Gi")
		})
		g.Expect(err).To(HaveOccurred(), "shrinking after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("create-only"))
	})

	t.Run("adding a size after create is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		// What a sub-type rule misses: absent in the stored object, it never fires.
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
