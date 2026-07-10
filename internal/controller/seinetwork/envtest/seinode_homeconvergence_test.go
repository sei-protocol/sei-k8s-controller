//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

// These names are the StatefulSet-spec identities the controller emits for the
// seid data-dir/$HOME convergence (#449). They're unexported in package
// noderesource, so the envtest suite pins its own copies; noderesource_test.go
// covers the generator directly against the same literals.
const (
	containerSeid    = "seid"        // main seid, in .Containers
	containerSidecar = "sei-sidecar" // seictl sidecar, an init container
	containerInit    = "seid-init"   // one-shot `seid init`, an init container
	volumeNameData   = "data"        // the data PVC, mounted at platform.DataDir
	volumeNameHome   = "home"        // emptyDir backing $HOME at platform.HomeDir
	envNameHome      = "HOME"
)

// TestSeiNode_HomeConvergenceMountsApplied is the reconcile-path proof for the
// data-dir/$HOME convergence (#449): the controller must APPLY the nested-mount
// layout to a live StatefulSet through the apiserver, not merely produce it in
// the pure generator (noderesource_test.go already covers GenerateStatefulSet
// directly). On all three seid-touching containers — seid main, sei-sidecar,
// seid-init — the data volume mounts at platform.DataDir, the `home` emptyDir
// mounts at platform.HomeDir, and HOME == platform.HomeDir; the `home` volume
// exists on the pod spec.
//
// SCOPE: envtest runs an apiserver + etcd but NO kubelet. No pod runs and no
// volume is actually mounted. This asserts the StatefulSet SPEC the controller
// emits over SSA — NOT runtime mount behavior (kubelet materializing the nested
// DataDir mountpoint inside the home emptyDir, seid reopening its DB on the
// PVC). That stays the cluster canary's job.
func TestSeiNode_HomeConvergenceMountsApplied(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := newTestSeiNode(ns, "home-convergence", false)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	stsKey := types.NamespacedName{Name: node.Name, Namespace: ns}
	sts := &appsv1.StatefulSet{}
	waitFor(t, func() bool {
		return testCli.Get(testCtx, stsKey, sts) == nil
	}, "controller must reconcile the SeiNode into a live StatefulSet")

	assertHomeConvergedLayout(g, sts)
}

// TestSeiNode_HomeConvergenceSurvivesImageBump is the rollout-path variant, in
// the spirit of image_update_test.go: after an image bump drives the child
// StatefulSet's re-apply, the resulting pod template must still carry the
// nested-mount layout. GenerateStatefulSet re-runs wholesale every reconcile,
// so this is a regression guard against a future update-path special-case
// dropping the converged layout — not a distinct code path from case 1.
//
// SCOPE: same as TestSeiNode_HomeConvergenceMountsApplied — asserts the emitted
// StatefulSet spec, not runtime mount behavior (no kubelet in envtest).
func TestSeiNode_HomeConvergenceSurvivesImageBump(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const newImage = "ghcr.io/sei-protocol/seid:v2.0.0"

	node := newTestSeiNode(ns, "home-convergence-bump", false)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)
	stsKey := types.NamespacedName{Name: node.Name, Namespace: ns}

	// Initial reconcile lands the StatefulSet on the fixture image.
	sts := &appsv1.StatefulSet{}
	waitFor(t, func() bool {
		return testCli.Get(testCtx, stsKey, sts) == nil
	}, "controller must reconcile the SeiNode into a live StatefulSet")

	// Bump spec.image; the controller re-applies the child StatefulSet.
	cur := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Spec.Image = newImage
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	// Wait for the re-applied StatefulSet to carry the new seid image.
	waitFor(t, func() bool {
		if err := testCli.Get(testCtx, stsKey, sts); err != nil {
			return false
		}
		c := findContainer(sts.Spec.Template.Spec.Containers, containerSeid)
		return c != nil && c.Image == newImage
	}, "image bump must propagate to the child StatefulSet's seid container")

	// The converged mount layout must survive the update.
	assertHomeConvergedLayout(g, sts)
}

// assertHomeConvergedLayout verifies the #449 nested-mount contract on the
// StatefulSet pod template: on seid main, sei-sidecar, and seid-init the data
// volume mounts at platform.DataDir, the `home` emptyDir mounts at
// platform.HomeDir, and HOME == platform.HomeDir; and the `home` volume exists
// on the pod spec. It reads the platform constants (not string literals) so the
// assertion tracks any future change to HomeDir/DataDir.
func assertHomeConvergedLayout(g Gomega, sts *appsv1.StatefulSet) {
	podSpec := sts.Spec.Template.Spec

	homeVol := findVolume(podSpec.Volumes, volumeNameHome)
	g.Expect(homeVol).NotTo(BeNil(), "pod spec must carry the `home` volume")
	g.Expect(homeVol.EmptyDir).NotTo(BeNil(), "`home` volume must be an emptyDir")

	// seid main lives in Containers; sei-sidecar and seid-init are init
	// containers. All three touch seid and must share the converged layout.
	seid := findContainer(podSpec.Containers, containerSeid)
	g.Expect(seid).NotTo(BeNil(), "pod spec must carry the seid main container")

	sidecar := findContainer(podSpec.InitContainers, containerSidecar)
	g.Expect(sidecar).NotTo(BeNil(), "pod spec must carry the sei-sidecar init container")

	init := findContainer(podSpec.InitContainers, containerInit)
	g.Expect(init).NotTo(BeNil(), "pod spec must carry the seid-init init container")

	for _, c := range []*corev1.Container{seid, sidecar, init} {
		dataMount := findVolumeMount(c.VolumeMounts, volumeNameData)
		g.Expect(dataMount).NotTo(BeNil(), "%s: data mount must be present", c.Name)
		g.Expect(dataMount.MountPath).To(Equal(platform.DataDir),
			"%s: data volume must mount at platform.DataDir", c.Name)

		homeMount := findVolumeMount(c.VolumeMounts, volumeNameHome)
		g.Expect(homeMount).NotTo(BeNil(), "%s: home mount must be present", c.Name)
		g.Expect(homeMount.MountPath).To(Equal(platform.HomeDir),
			"%s: home volume must mount at platform.HomeDir", c.Name)

		homeEnv := findEnv(c.Env, envNameHome)
		g.Expect(homeEnv).NotTo(BeNil(), "%s: HOME env must be set", c.Name)
		g.Expect(homeEnv.Value).To(Equal(platform.HomeDir),
			"%s: HOME must equal platform.HomeDir", c.Name)
	}
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func findVolumeMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}
