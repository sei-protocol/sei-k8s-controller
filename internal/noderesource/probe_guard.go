package noderesource

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
)

// ValidateGatedSeidProbes enforces the probe contract the workflow hold leans
// on: a gated seid container (one parked at the sidecar start gate during a
// hold) must carry NO liveness probe and NO startup probe that targets seid's
// own RPC/gRPC port. While seid is held, nothing listens on those ports, so an
// RPC-based liveness or startup probe would make the kubelet kill the held pod
// and release the hold. A startup probe gating on the sidecar's healthz (via
// the RBAC-proxy port) is allowed and expected — that is how the gate blocks.
//
// The readiness probe is intentionally not checked: readiness failing during a
// hold is correct (the pod drops from Service endpoints).
//
// This is the render-time guard for the design's probe invariant; call it on
// the gated container to fail closed rather than ship a pod the hold cannot
// safely park.
func ValidateGatedSeidProbes(c corev1.Container) error {
	if c.LivenessProbe != nil {
		return fmt.Errorf("gated seid container %q must not carry a liveness probe", c.Name)
	}
	if probeTargetsSeidRPC(c.StartupProbe) {
		return fmt.Errorf("gated seid container %q startup probe must not target seid's RPC/gRPC port", c.Name)
	}
	return nil
}

// probeTargetsSeidRPC reports whether a probe polls seid's RPC or gRPC port
// (via HTTPGet or TCPSocket). A nil probe or one targeting any other port
// (e.g. the RBAC-proxy healthz port) returns false.
func probeTargetsSeidRPC(p *corev1.Probe) bool {
	if p == nil {
		return false
	}
	isSeidPort := func(port int32) bool {
		return port == seiconfig.PortRPC || port == seiconfig.PortGRPC
	}
	switch {
	case p.HTTPGet != nil:
		return isSeidPort(p.HTTPGet.Port.IntVal)
	case p.TCPSocket != nil:
		return isSeidPort(p.TCPSocket.Port.IntVal)
	default:
		return false
	}
}
