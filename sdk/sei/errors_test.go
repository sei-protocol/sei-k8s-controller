package sei

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("waiting: %w", context.DeadlineExceeded), true},
		{"canceled is not timeout", context.Canceled, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTimeout(tc.err); got != tc.want {
				t.Errorf("IsTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}
