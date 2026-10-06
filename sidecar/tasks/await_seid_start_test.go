package tasks

import (
	"context"
	"testing"
	"time"
)

// 010 Req 2.3: the start-once step waits until seid runs.
func TestAwaitSeidStart_CompletesWhenSeidRuns(t *testing.T) {
	calls := 0
	a := &SeidStartAwaiter{
		find:         func() bool { calls++; return calls >= 3 },
		pollInterval: time.Millisecond,
	}
	if _, err := a.Handler()(context.Background(), nil); err != nil {
		t.Fatalf("await-seid-start: %v", err)
	}
	if calls != 3 {
		t.Errorf("find called %d times, want 3", calls)
	}
}

func TestAwaitSeidStart_StopsOnContext(t *testing.T) {
	a := &SeidStartAwaiter{find: func() bool { return false }, pollInterval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.Handler()(ctx, nil); err == nil {
		t.Fatal("expected the context error while seid never starts")
	}
}
