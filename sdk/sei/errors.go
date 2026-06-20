package sei

import (
	"context"
	"errors"
	"fmt"
)

// The SDK returns plain wrapped errors. Two seams a caller branches on:
//
//   - IsTimeout — a WaitReady deadline elapsed (the caller's ctx fired). Wraps
//     context.DeadlineExceeded.
//   - apierrors.IsNotFound — pass-through from a Get/Delete; the k8s provider
//     returns the apimachinery error unwrapped, so callers match it directly.
//
// Bad-input (usageErr) and provider faults are plain fmt.Errorf chains; reach
// for errors.Is/As, not a class enum.

// usageErr builds a bad-input error (missing/invalid spec field).
func usageErr(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// IsTimeout reports whether err is (or wraps) a WaitReady deadline —
// context.DeadlineExceeded. A caller distinguishes "not ready in time" from a
// terminal provider error this way.
func IsTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
