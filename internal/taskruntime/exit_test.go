package taskruntime

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCodeFor(t *testing.T) {
	plain := errors.New("plain")
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitPass},
		{"plain", plain, ExitTaskFailure},
		{"task", Task(plain), ExitTaskFailure},
		{"infra", Infra(plain), ExitInfraError},
		{"infra wrapped twice", fmt.Errorf("outer: %w", Infra(plain)), ExitInfraError},
		{"task wrapped twice", fmt.Errorf("outer: %w", Task(plain)), ExitTaskFailure},
		{"Task(nil)", Task(nil), ExitPass},
		{"Infra(nil)", Infra(nil), ExitPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Fatalf("ExitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
