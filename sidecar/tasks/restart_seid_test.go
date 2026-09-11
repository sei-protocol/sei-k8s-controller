package tasks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

// fakeSignaler implements actions.ProcessSignaler for restart-seid tests.
type fakeSignaler struct {
	findPID  int
	findErr  error
	alive    atomic.Bool
	signals  []syscall.Signal
	signalFn func(pid int, sig syscall.Signal) error
}

func (f *fakeSignaler) FindPID(string) (int, error) { return f.findPID, f.findErr }

func (f *fakeSignaler) Signal(pid int, sig syscall.Signal) error {
	f.signals = append(f.signals, sig)
	if f.signalFn != nil {
		return f.signalFn(pid, sig)
	}
	return nil
}

func (f *fakeSignaler) Alive(int) bool { return f.alive.Load() }

// upAfter returns a probe that reports down for the first n calls, then up.
func upAfter(n int) func(context.Context) bool {
	var calls atomic.Int32
	return func(context.Context) bool {
		return calls.Add(1) > int32(n)
	}
}

func neverUp(context.Context) bool { return false }

func TestRestartSeider_HappyPath(t *testing.T) {
	sig := &fakeSignaler{findPID: 42}
	sig.alive.Store(false) // exits immediately after SIGTERM

	r := &RestartSeider{
		signaler:    sig,
		probeUp:     upAfter(0), // up on first probe
		gracePeriod: time.Second,
		upTimeout:   time.Second,
		upInterval:  time.Millisecond,
	}

	if _, err := r.Handler()(context.Background(), nil); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(sig.signals) != 1 || sig.signals[0] != syscall.SIGTERM {
		t.Errorf("expected single SIGTERM, got %v", sig.signals)
	}
}

func TestRestartSeider_GraceTimeoutFailsWithoutSIGKILL(t *testing.T) {
	sig := &fakeSignaler{findPID: 42}
	sig.alive.Store(true) // never exits on SIGTERM

	r := &RestartSeider{
		signaler:    sig,
		probeUp:     upAfter(0),
		gracePeriod: 50 * time.Millisecond,
		upTimeout:   time.Second,
		upInterval:  time.Millisecond,
	}

	_, err := r.Handler()(context.Background(), nil)
	if err == nil {
		t.Fatal("expected grace-timeout failure, got nil")
	}
	if !strings.Contains(err.Error(), "still alive") {
		t.Errorf("expected still-alive error, got %v", err)
	}
	if len(sig.signals) != 1 || sig.signals[0] != syscall.SIGTERM {
		t.Errorf("expected single SIGTERM and no SIGKILL, got %v", sig.signals)
	}
}

func TestRestartSeider_NotFoundRPCDownWaitsForUp(t *testing.T) {
	sig := &fakeSignaler{findErr: fmt.Errorf("process \"seid\" not found in /proc")}

	r := &RestartSeider{
		signaler:    sig,
		probeUp:     upAfter(2), // RPC down (not-found guard + first poll), then up
		gracePeriod: time.Second,
		upTimeout:   time.Second,
		upInterval:  time.Millisecond,
	}

	if _, err := r.Handler()(context.Background(), nil); err != nil {
		t.Fatalf("expected success when seid not found and RPC down, got %v", err)
	}
	if len(sig.signals) != 0 {
		t.Errorf("expected no signals when seid not running, got %v", sig.signals)
	}
}

func TestRestartSeider_NotFoundRPCUpFails(t *testing.T) {
	sig := &fakeSignaler{findErr: fmt.Errorf("process \"seid\" not found in /proc")}

	r := &RestartSeider{
		signaler:    sig,
		probeUp:     upAfter(0), // RPC serving despite no /proc match
		gracePeriod: time.Second,
		upTimeout:   time.Second,
		upInterval:  time.Millisecond,
	}

	_, err := r.Handler()(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when RPC up but process not found, got nil")
	}
	if !strings.Contains(err.Error(), "not found in /proc") {
		t.Errorf("expected not-found-in-/proc error, got %v", err)
	}
	if len(sig.signals) != 0 {
		t.Errorf("expected no signals, got %v", sig.signals)
	}
}

func TestRestartSeider_WaitForUpContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &RestartSeider{
		signaler:    &fakeSignaler{findErr: fmt.Errorf("process \"seid\" not found in /proc")},
		probeUp:     neverUp,
		gracePeriod: time.Second,
		upTimeout:   time.Minute,
		upInterval:  10 * time.Millisecond,
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := r.waitForUp(ctx, r.probeUp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRestartSeider_RPCNeverUpTimesOut(t *testing.T) {
	sig := &fakeSignaler{findPID: 42}
	sig.alive.Store(false)

	r := &RestartSeider{
		signaler:    sig,
		probeUp:     neverUp,
		gracePeriod: time.Second,
		upTimeout:   50 * time.Millisecond,
		upInterval:  time.Millisecond,
	}

	_, err := r.Handler()(context.Background(), nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestIsSeidStart(t *testing.T) {
	tests := []struct {
		name    string
		cmdline []byte
		want    bool
	}{
		{"bare seid start", []byte("seid\x00start\x00--home\x00/.sei"), true},
		{"absolute path seid start", []byte("/usr/bin/seid\x00start"), true},
		{"trailing null", []byte("seid\x00start\x00"), true},
		{"seid non-start subcommand", []byte("seid\x00version"), false},
		{"seid-init", []byte("seid-init\x00start"), false},
		{"bash wrapper", []byte("bash\x00-c\x00seid start"), false},
		{"seid no args", []byte("seid"), false},
		{"empty", []byte{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSeidStart(tt.cmdline); got != tt.want {
				t.Errorf("isSeidStart(%q) = %v, want %v", tt.cmdline, got, tt.want)
			}
		})
	}
}

// A request that names an up-check waits on that check, not the default
// /status probe, and the stop phase's honesty check uses the same probe.
func TestRestartSeider_UpCheckFromParams(t *testing.T) {
	sig := &fakeSignaler{findPID: 42}
	sig.alive.Store(false)

	var got wire.UpCheck
	r := &RestartSeider{
		signaler: sig,
		probeUp:  neverUp,
		probeFor: func(c wire.UpCheck) func(context.Context) bool {
			got = c
			return upAfter(0)
		},
		gracePeriod: time.Second,
		upTimeout:   time.Second,
		upInterval:  time.Millisecond,
	}

	params := map[string]any{"upCheck": map[string]any{"scheme": "tcp", "port": 26656}}
	if _, err := r.Handler()(context.Background(), params); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got != (wire.UpCheck{Scheme: wire.UpCheckTCP, Port: 26656}) {
		t.Fatalf("unexpected up-check %+v", got)
	}
}

func TestRestartSeider_RejectsMalformedUpCheck(t *testing.T) {
	sig := &fakeSignaler{findPID: 42}
	r := &RestartSeider{signaler: sig, probeUp: upAfter(0), probeFor: upCheckProbe,
		gracePeriod: time.Second, upTimeout: time.Second, upInterval: time.Millisecond}

	params := map[string]any{"upCheck": map[string]any{"scheme": "http", "port": 8545}}
	if _, err := r.Handler()(context.Background(), params); err == nil {
		t.Fatal("expected validation error for http up-check without path")
	}
	if len(sig.signals) != 0 {
		t.Fatalf("seid must not be signalled on a rejected request, got %v", sig.signals)
	}
}

// upCheckProbe reads real loopback listeners: a TCP connect and an HTTP GET
// answered 2xx count as up; a closed port and a 5xx do not.
func TestUpCheckProbe_Loopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, closedStr, _ := net.SplitHostPort(closed.Addr().String())
	closedPort, _ := strconv.Atoi(closedStr)
	_ = closed.Close()

	ctx := context.Background()
	cases := []struct {
		name  string
		check wire.UpCheck
		want  bool
	}{
		{"tcp open", wire.UpCheck{Scheme: wire.UpCheckTCP, Port: int32(port)}, true},
		{"tcp closed", wire.UpCheck{Scheme: wire.UpCheckTCP, Port: int32(closedPort)}, false},
		{"http 200", wire.UpCheck{Scheme: wire.UpCheckHTTP, Port: int32(port), Path: "/"}, true},
		{"http 503", wire.UpCheck{Scheme: wire.UpCheckHTTP, Port: int32(port), Path: "/down"}, false},
		{"http closed", wire.UpCheck{Scheme: wire.UpCheckHTTP, Port: int32(closedPort), Path: "/"}, false},
	}
	for _, tc := range cases {
		if got := upCheckProbe(tc.check)(ctx); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
