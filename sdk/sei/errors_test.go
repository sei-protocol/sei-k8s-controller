package sei

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassSentinels(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		isTimeout  bool
		isFailed   bool
		isCanceled bool
	}{
		{"timeout", &Error{Class: ClassTimeout}, true, false, false},
		{"failed", &Error{Class: ClassFailed}, false, true, false},
		{"usage", &Error{Class: ClassUsage}, false, false, false},
		{"infra", &Error{Class: ClassInfra}, false, false, false},
		{"canceled", &Error{Class: ClassCanceled}, false, false, true},
		{"wrapped timeout", fmt.Errorf("ctx: %w", &Error{Class: ClassTimeout}), true, false, false},
		{"wrapped canceled", fmt.Errorf("ctx: %w", &Error{Class: ClassCanceled}), false, false, true},
		{"non-sdk error", errors.New("plain"), false, false, false},
		{"nil", nil, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTimeout(tc.err); got != tc.isTimeout {
				t.Errorf("IsTimeout = %v, want %v", got, tc.isTimeout)
			}
			if got := IsFailed(tc.err); got != tc.isFailed {
				t.Errorf("IsFailed = %v, want %v", got, tc.isFailed)
			}
			if got := IsCanceled(tc.err); got != tc.isCanceled {
				t.Errorf("IsCanceled = %v, want %v", got, tc.isCanceled)
			}
		})
	}
}

func TestClassString(t *testing.T) {
	// ClassInfra's String is covered by TestError_UnwrapAndMessage.
	cases := map[Class]string{
		ClassUsage:    "Usage",
		ClassTimeout:  "Timeout",
		ClassFailed:   "Failed",
		ClassCanceled: "Canceled",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("Class(%d).String() = %q, want %q", int(c), got, want)
		}
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
