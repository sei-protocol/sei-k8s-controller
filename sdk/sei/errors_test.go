package sei

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassSentinels(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		isTimeout bool
		isFailed  bool
	}{
		{"timeout", &Error{Class: ClassTimeout}, true, false},
		{"failed", &Error{Class: ClassFailed}, false, true},
		{"usage", &Error{Class: ClassUsage}, false, false},
		{"infra", &Error{Class: ClassInfra}, false, false},
		{"wrapped timeout", fmt.Errorf("ctx: %w", &Error{Class: ClassTimeout}), true, false},
		{"non-sdk error", errors.New("plain"), false, false},
		{"nil", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTimeout(tc.err); got != tc.isTimeout {
				t.Errorf("IsTimeout = %v, want %v", got, tc.isTimeout)
			}
			if got := IsFailed(tc.err); got != tc.isFailed {
				t.Errorf("IsFailed = %v, want %v", got, tc.isFailed)
			}
		})
	}
}

func TestError_UnwrapAndMessage(t *testing.T) {
	cause := errors.New("apiserver down")
	e := &Error{Class: ClassInfra, Resource: "SeiNetwork ns/foo", Phase: "Degraded", Err: cause}
	if !errors.Is(e, cause) {
		t.Fatal("errors.Is should reach the wrapped cause")
	}
	msg := e.Error()
	for _, want := range []string{"Infra", "SeiNetwork ns/foo", "Degraded", "apiserver down"} {
		if !contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
