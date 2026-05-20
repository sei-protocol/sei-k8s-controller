// Package taskimg is the shared library for seitask subcommands: typed exit
// codes, ownerReference stamping, and workflow-vars ConfigMap helpers.
package taskimg

import (
	"errors"
	"fmt"
)

// Exit codes match qa-testing/release-test.ts: 0=pass, 1=task-fail (the work
// failed), 2=infra-fail (couldn't reach a verdict). Chaos Mesh collapses 1
// and 2 to "Failed"; downstream readers use the EXIT_REASON workflow-vars
// key to recover the distinction.
const (
	ExitPass        = 0
	ExitTaskFailure = 1
	ExitInfraError  = 2
)

// InfraError marks the failure as non-deterministic (API unreachable,
// timeout, malformed input). Bare errors and TaskError map to exit 1.
type InfraError struct{ Err error }

func (e *InfraError) Error() string { return fmt.Sprintf("infra: %v", e.Err) }
func (e *InfraError) Unwrap() error { return e.Err }

// Infra wraps err as an InfraError. nil in → nil out.
func Infra(err error) error {
	if err == nil {
		return nil
	}
	return &InfraError{Err: err}
}

// TaskError marks the failure as work-correctness. Wrapping is optional;
// bare errors map to exit 1 too.
type TaskError struct{ Err error }

func (e *TaskError) Error() string { return fmt.Sprintf("task: %v", e.Err) }
func (e *TaskError) Unwrap() error { return e.Err }

// Task wraps err as a TaskError. nil in → nil out.
func Task(err error) error {
	if err == nil {
		return nil
	}
	return &TaskError{Err: err}
}

// ExitCodeFor: nil → 0, InfraError → 2, everything else → 1.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitPass
	}
	var infra *InfraError
	if errors.As(err, &infra) {
		return ExitInfraError
	}
	return ExitTaskFailure
}
