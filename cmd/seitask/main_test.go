package main

import (
	"testing"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// TestTaskScheme_RoundTripsSND would have caught the first manual fire's
// `no kind is registered for the type v1alpha1.SeiNodeDeployment in scheme`
// regression at `go test`, not at first cluster fire. Asserts the
// package-level taskScheme has every type provision-snd / keygen /
// upload-report constructs via typed Get/Create.
func TestTaskScheme_RoundTripsSND(t *testing.T) {
	gvks, _, err := taskScheme.ObjectKinds(&seiv1alpha1.SeiNodeDeployment{})
	if err != nil {
		t.Fatalf("SeiNodeDeployment not registered in taskScheme: %v", err)
	}
	if len(gvks) == 0 {
		t.Fatalf("no GVKs returned for SeiNodeDeployment")
	}
	if gvks[0].Group != "sei.io" || gvks[0].Version != "v1alpha1" {
		t.Fatalf("SeiNodeDeployment GVK: %+v; want sei.io/v1alpha1", gvks[0])
	}
}

func TestTaskScheme_RoundTripsSeiNodeTask(t *testing.T) {
	gvks, _, err := taskScheme.ObjectKinds(&seiv1alpha1.SeiNodeTask{})
	if err != nil {
		t.Fatalf("SeiNodeTask not registered in taskScheme: %v", err)
	}
	if len(gvks) == 0 || gvks[0].Group != "sei.io" {
		t.Fatalf("SeiNodeTask GVK wrong: %+v", gvks)
	}
}
