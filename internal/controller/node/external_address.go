package node

import (
	"context"
	"fmt"
	"net"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	// p2pServiceSuffix is appended to the SeiNode name to form the
	// per-pod LoadBalancer Service name. Format: "<seinode>-p2p".
	p2pServiceSuffix = "-p2p"

	// p2pTCPPort is the CometBFT P2P TCP port. Pinned by sei-config /
	// CometBFT; mirrored here to keep the Service generator self-contained.
	p2pTCPPort int32 = 26656

	// envVPCCIDR carries the VPC CIDR used for pod-IP routability checks.
	// Unset → fail-closed (refuse to stamp), the safety net for harbor's
	// CGNAT pods where NLB target-type=ip would silently break.
	envVPCCIDR = "SEI_VPC_CIDR"

	// podNameLabel is the StatefulSet pod-name label. Selecting on this
	// targets a single ordinal pod, surviving any future scale-up.
	podNameLabel = "statefulset.kubernetes.io/pod-name"
)

// p2pServiceName returns the per-pod LB Service name for a SeiNode.
func p2pServiceName(node *seiv1alpha1.SeiNode) string {
	return node.Name + p2pServiceSuffix
}

// runExternalAddressGate runs reconcileExternalAddress and decides
// whether the outer Reconcile should short-circuit. done=true means
// return immediately (error, or gate-held); done=false means proceed
// to StatefulSet reconcile.
func (r *SeiNodeReconciler) runExternalAddressGate(ctx context.Context, node *seiv1alpha1.SeiNode, flushStatus func() error) (bool, error) {
	ready, err := r.reconcileExternalAddress(ctx, node)
	if err != nil {
		return true, fmt.Errorf("reconciling external address: %w", err)
	}
	if ready {
		return false, nil
	}
	if err := flushStatus(); err != nil {
		return true, fmt.Errorf("flushing status before service-ready gate: %w", err)
	}
	return true, nil
}

// reconcileExternalAddress provisions the per-pod L4 NLB Service when
// the parent SND has Networking.TCP set, writes the NLB hostname into
// Status.ExternalAddress when populated, and reports whether the node
// is ready to proceed to StatefulSet creation.
//
// ready=true when TCP is not requested (opt-out, Service cleaned up)
// or when TCP is requested AND ExternalAddress is populated.
// ready=false otherwise (VPC CIDR unset, pod outside CIDR, or NLB
// hostname not yet populated). PublishableReady condition carries the
// reason. ExternalAddress is cleared only on intentional opt-out.
func (r *SeiNodeReconciler) reconcileExternalAddress(ctx context.Context, node *seiv1alpha1.SeiNode) (bool, error) {
	logger := log.FromContext(ctx)

	snd, err := r.fetchParentSND(ctx, node)
	if err != nil {
		return false, fmt.Errorf("fetching parent SND: %w", err)
	}

	if snd == nil || snd.Spec.Networking == nil || snd.Spec.Networking.TCP == nil {
		if err := r.deleteP2PService(ctx, node); err != nil {
			return false, fmt.Errorf("deleting p2p service on opt-out: %w", err)
		}
		if node.Status.ExternalAddress != "" {
			node.Status.ExternalAddress = ""
		}
		setPublishableCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonPublishableDisabled,
			"parent SeiNodeDeployment has no Networking.TCP; per-pod LB not provisioned")
		return true, nil
	}

	// Fail-closed when SEI_VPC_CIDR is unset — without it we can't
	// verify pod-IP routability (harbor's Cilium CGNAT is the case).
	vpcCIDR := os.Getenv(envVPCCIDR)
	if vpcCIDR == "" {
		logger.Info("publishable P2P requested but SEI_VPC_CIDR is not set; refusing to stamp Service",
			"seinode", node.Name)
		setPublishableCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonVPCCIDRNotConfigured,
			"SEI_VPC_CIDR is not configured on the controller; publishable Service stamping is fail-closed")
		return false, nil
	}

	// Pod-IP routability: fail closed if the pod exists but is outside
	// the CIDR. First reconcile (no pod yet) stamps the Service anyway
	// so NLB provisioning can run in parallel with STS bringup.
	podInside, podPresent, err := r.podInsideVPCCIDR(ctx, node, vpcCIDR)
	if err != nil {
		return false, fmt.Errorf("checking pod-IP routability: %w", err)
	}
	if podPresent && !podInside {
		setPublishableCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonPodOutsideVPCCIDR,
			fmt.Sprintf("pod %s-0 IP is outside SEI_VPC_CIDR %s; NLB target-type=ip requires VPC-routable pod IPs", node.Name, vpcCIDR))
		return false, nil
	}

	if err := r.ensureP2PService(ctx, node); err != nil {
		return false, fmt.Errorf("ensuring p2p service: %w", err)
	}

	hostname, err := r.readP2PServiceHostname(ctx, node)
	if err != nil {
		return false, fmt.Errorf("reading p2p service hostname: %w", err)
	}
	if hostname == "" {
		setPublishableCondition(node, metav1.ConditionFalse, seiv1alpha1.ReasonServiceProvisioning,
			"per-pod LB Service exists; waiting for AWS NLB hostname to populate")
		return false, nil
	}

	desired := fmt.Sprintf("%s:%d", hostname, p2pTCPPort)
	if node.Status.ExternalAddress != desired {
		node.Status.ExternalAddress = desired
	}
	setPublishableCondition(node, metav1.ConditionTrue, seiv1alpha1.ReasonExternalAddressReady,
		fmt.Sprintf("Status.ExternalAddress = %s", desired))
	return true, nil
}

// fetchParentSND resolves the SeiNode's controller owner reference back
// to the parent SeiNodeDeployment. Returns (nil, nil) when the SeiNode
// has no controller owner of kind SeiNodeDeployment (standalone case).
func (r *SeiNodeReconciler) fetchParentSND(ctx context.Context, node *seiv1alpha1.SeiNode) (*seiv1alpha1.SeiNodeDeployment, error) {
	ctrlRef := metav1.GetControllerOf(node)
	if ctrlRef == nil {
		return nil, nil
	}
	if ctrlRef.Kind != "SeiNodeDeployment" {
		return nil, nil
	}
	if ctrlRef.APIVersion != seiv1alpha1.GroupVersion.String() {
		return nil, nil
	}
	snd := &seiv1alpha1.SeiNodeDeployment{}
	err := r.Get(ctx, types.NamespacedName{Name: ctrlRef.Name, Namespace: node.Namespace}, snd)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return snd, nil
}

// podInsideVPCCIDR reports whether the SeiNode's first pod has an IP
// inside the given VPC CIDR. podPresent is false when the pod (or its
// IP) does not yet exist — the first-reconcile shape, before the STS
// has been created. In that case, callers should proceed to stamp the
// Service; the next reconcile after the pod boots will re-check.
func (r *SeiNodeReconciler) podInsideVPCCIDR(ctx context.Context, node *seiv1alpha1.SeiNode, vpcCIDR string) (inside, podPresent bool, err error) {
	_, cidr, parseErr := net.ParseCIDR(vpcCIDR)
	if parseErr != nil {
		return false, false, fmt.Errorf("parsing SEI_VPC_CIDR %q: %w", vpcCIDR, parseErr)
	}
	pod := &corev1.Pod{}
	getErr := r.Get(ctx, types.NamespacedName{Name: node.Name + "-0", Namespace: node.Namespace}, pod)
	if apierrors.IsNotFound(getErr) {
		return false, false, nil
	}
	if getErr != nil {
		return false, false, fmt.Errorf("fetching pod %s-0: %w", node.Name, getErr)
	}
	if pod.Status.PodIP == "" {
		return false, false, nil
	}
	ip := net.ParseIP(pod.Status.PodIP)
	if ip == nil {
		return false, true, fmt.Errorf("pod %s-0 has unparseable IP %q", node.Name, pod.Status.PodIP)
	}
	return cidr.Contains(ip), true, nil
}

// ensureP2PService idempotently applies the per-pod LoadBalancer
// Service via Server-Side Apply, with the SeiNode as the controller
// owner so the Owns(&Service{}) watch already wired on the controller
// fires on Service updates and the Service is GC'd on SeiNode deletion.
func (r *SeiNodeReconciler) ensureP2PService(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	desired := generateP2PService(node)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner("seinode-controller"), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying p2p service: %w", err)
	}
	return nil
}

// readP2PServiceHostname returns the AWS NLB hostname from the live
// Service's status.loadBalancer.ingress[0].hostname, or "" if not yet
// populated. NotFound is treated as empty (caller may have just
// stamped the Service; cache propagation can lag by one reconcile).
func (r *SeiNodeReconciler) readP2PServiceHostname(ctx context.Context, node *seiv1alpha1.SeiNode) (string, error) {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: node.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return "", nil
	}
	return svc.Status.LoadBalancer.Ingress[0].Hostname, nil
}

// deleteP2PService removes the per-pod LB Service if present. Used on
// the opt-out path (parent SND has no Networking.TCP). NotFound is the
// no-op steady state.
func (r *SeiNodeReconciler) deleteP2PService(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: node.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// generateP2PService renders the per-pod L4 NLB Service. Pod-name
// selector targets ordinal-0 even under future scale-up. Annotations
// are the AWS LBC contract for internet-facing IP-target NLB with
// cross-zone enabled; externalTrafficPolicy=Local preserves source IP.
func generateP2PService(node *seiv1alpha1.SeiNode) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p2pServiceName(node),
			Namespace: node.Namespace,
			Labels:    p2pServiceLabels(node),
			Annotations: map[string]string{
				"service.beta.kubernetes.io/aws-load-balancer-type":                              "nlb",
				"service.beta.kubernetes.io/aws-load-balancer-scheme":                            "internet-facing",
				"service.beta.kubernetes.io/aws-load-balancer-nlb-target-type":                   "ip",
				"service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled": "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			Selector: map[string]string{
				podNameLabel: node.Name + "-0",
			},
			Ports: []corev1.ServicePort{{
				Name:       "p2p",
				Port:       p2pTCPPort,
				TargetPort: intstr.FromInt32(p2pTCPPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// p2pServiceLabels are observability labels on the per-pod LB Service.
// Lookup is by name, not label — keep these small and stable.
func p2pServiceLabels(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		"sei.io/node":      node.Name,
		"sei.io/component": "p2p-lb",
	}
}

// setPublishableCondition is a thin wrapper that always populates
// ObservedGeneration — required by the project's condition discipline.
func setPublishableCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPublishableReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
