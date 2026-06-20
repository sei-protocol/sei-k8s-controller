package local

import (
	"context"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

func TestStub_RegistersButFailsClassUsage(t *testing.T) {
	p, err := New(context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "local" {
		t.Fatalf("Name = %q, want local", p.Name())
	}
	_, nerr := p.ProvisionNetwork(context.Background(), sei.NetworkSpec{})
	if nerr == nil {
		t.Fatal("ProvisionNetwork should fail in the stub")
	}
	var e *sei.Error
	if !asSDKError(nerr, &e) || e.Class != sei.ClassUsage {
		t.Fatalf("stub error should be ClassUsage, got %v", nerr)
	}
}

func asSDKError(err error, target **sei.Error) bool {
	if e, ok := err.(*sei.Error); ok {
		*target = e
		return true
	}
	return false
}
