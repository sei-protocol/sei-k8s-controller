package sei

import (
	"errors"
	"fmt"
)

// Class buckets every SDK error so a harness branches on cause instead of
// matching on the `jq -r .reason` off a metav1.Status that bash consumers use
// today (LLD §3.5). One-way door D4: adding a class is additive; renaming or
// removing one breaks every consumer's switch.
type Class int

const (
	// ClassUsage is a bad spec or missing required field — terminal, caller's
	// fault (the cliutil.UsageError analogue).
	ClassUsage Class = iota

	// ClassTimeout is a ReadyTimeout/RunningTimeout/FirstBlockTimeout exceeded.
	// A chaos harness may retry the provision.
	ClassTimeout

	// ClassFailed is a resource that reached a terminal Failed phase. The test
	// fails; retrying the same spec won't help.
	ClassFailed

	// ClassInfra is an apiserver/network/RPC transient or unexpected fault (the
	// taskruntime.Infra analogue).
	ClassInfra
)

func (c Class) String() string {
	switch c {
	case ClassUsage:
		return "Usage"
	case ClassTimeout:
		return "Timeout"
	case ClassFailed:
		return "Failed"
	case ClassInfra:
		return "Infra"
	default:
		return fmt.Sprintf("Class(%d)", int(c))
	}
}

// Error is the structured SDK error. Consumers branch on Class (via the
// IsTimeout/IsFailed sentinels) and read Resource/Phase for diagnostics.
type Error struct {
	Class    Class
	Resource string // "SeiNetwork foo/bar", "SeiNode foo/bar-2"; "" when not resource-scoped
	Phase    string // last observed .status.phase, when relevant
	Err      error  // wrapped cause; supports errors.Is/As
}

func (e *Error) Error() string {
	var b []byte
	b = append(b, e.Class.String()...)
	if e.Resource != "" {
		b = append(b, ": "...)
		b = append(b, e.Resource...)
	}
	if e.Phase != "" {
		b = append(b, " (phase="...)
		b = append(b, e.Phase...)
		b = append(b, ')')
	}
	if e.Err != nil {
		b = append(b, ": "...)
		b = append(b, e.Err.Error()...)
	}
	return string(b)
}

func (e *Error) Unwrap() error { return e.Err }

// classOf reports the Class of err if it is (or wraps) an *Error.
func classOf(err error) (Class, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Class, true
	}
	return 0, false
}

// IsTimeout reports whether err is (or wraps) a ClassTimeout SDK error.
func IsTimeout(err error) bool {
	c, ok := classOf(err)
	return ok && c == ClassTimeout
}

// IsFailed reports whether err is (or wraps) a ClassFailed SDK error.
func IsFailed(err error) bool {
	c, ok := classOf(err)
	return ok && c == ClassFailed
}

// usageErr builds a ClassUsage error not scoped to a resource.
func usageErr(format string, args ...any) *Error {
	return &Error{Class: ClassUsage, Err: fmt.Errorf(format, args...)}
}
