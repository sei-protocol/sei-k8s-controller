package local

import (
	"context"
	"errors"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

func TestStub_RegistersButVerbsNotImplemented(t *testing.T) {
	p, err := New(context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "local" {
		t.Fatalf("Name = %q, want local", p.Name())
	}

	if _, err := p.CreateNetwork(context.Background(), sei.NetworkSpec{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("CreateNetwork err = %v, want ErrNotImplemented", err)
	}
	if _, err := p.CreateNode(context.Background(), sei.NodeSpec{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("CreateNode err = %v, want ErrNotImplemented", err)
	}
	if _, err := p.GetNetwork(context.Background(), "n", "ns"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("GetNetwork err = %v, want ErrNotImplemented", err)
	}
	if _, err := p.GetNode(context.Background(), "n", "ns"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("GetNode err = %v, want ErrNotImplemented", err)
	}
}
