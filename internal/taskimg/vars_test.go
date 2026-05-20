package taskimg

import (
	"errors"
	"testing"
)

func TestExitReasonFor(t *testing.T) {
	plain := errors.New("plain")
	cases := []struct {
		name string
		err  error
		want ExitReason
	}{
		{"nil", nil, ExitReasonPass},
		{"plain", plain, ExitReasonTaskFail},
		{"task", Task(plain), ExitReasonTaskFail},
		{"infra", Infra(plain), ExitReasonInfraFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitReasonFor(tc.err); got != tc.want {
				t.Fatalf("ExitReasonFor(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}
