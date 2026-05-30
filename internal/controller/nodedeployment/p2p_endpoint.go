// p2p_endpoint.go provisions per-pod P2P endpoint Services (LoadBalancer)
// and the deterministic vanity hostname for each. external-dns CNAMEs the
// hostname to the NLB; the child SeiNode reads Spec.ExternalAddress.
package nodedeployment

import (
	"context"
	"fmt"
	"strconv"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const (
	p2pEndpointFieldOwner = client.FieldOwner("nodedeployment-controller-p2p-endpoint")

	p2pEndpointComponentLabel = "sei.io/component"
	p2pEndpointComponentValue = "p2p-endpoint"

	externalDNSHostnameAnnotation = "external-dns.alpha.kubernetes.io/hostname"

	awsLBTypeAnnotation        = "service.beta.kubernetes.io/aws-load-balancer-type"
	awsLBSchemeAnnotation      = "service.beta.kubernetes.io/aws-load-balancer-scheme"
	awsLBTargetTypeAnnotation  = "service.beta.kubernetes.io/aws-load-balancer-nlb-target-type"
	awsLBCrossZoneAnnotation   = "service.beta.kubernetes.io/aws-load-balancer-attributes"
	awsLBTypeValue             = "external"
	awsLBSchemeValue           = "internet-facing"
	awsLBCrossZoneAttributeStr = "load_balancing.cross_zone.enabled=true"

	// NLBTargetTypeIP registers pod IPs directly with the NLB target group.
	// Correct on VPC-CNI clusters where pod IPs are VPC-routable.
	NLBTargetTypeIP = "ip"
	// NLBTargetTypeInstance routes NLB traffic via the node's NodePort and
	// relies on kube-proxy / Cilium socket-LB to forward to the pod.
	// Required on clusters where pod IPs aren't VPC-routable.
	NLBTargetTypeInstance = "instance"
	// DefaultNLBTargetType matches the prior hardcoded behaviour.
	DefaultNLBTargetType = NLBTargetTypeIP

	p2pEndpointServiceSuffix = "-p2p"
)

// effectiveChainID returns the chain ID for child SeiNodes, mirroring
// generateSeiNode: template wins, Genesis is the validator fallback.
func effectiveChainID(group *seiv1alpha1.SeiNodeDeployment) string {
	if id := group.Spec.Template.Spec.ChainID; id != "" {
		return id
	}
	if group.Spec.Genesis != nil {
		return group.Spec.Genesis.ChainID
	}
	return ""
}

// p2pEndpointHostname returns "<seinode>-p2p.<chainID>.<domain>" or "" when
// the P2P endpoint path is not configured.
func (r *SeiNodeDeploymentReconciler) p2pEndpointHostname(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	if r.P2PEndpointDomain == "" {
		return ""
	}
	chainID := effectiveChainID(group)
	if chainID == "" {
		return ""
	}
	return fmt.Sprintf("%s-p2p.%s.%s", seiNodeName(group, ordinal), chainID, r.P2PEndpointDomain)
}

// p2pEndpointAddress returns "<hostname>:<port>" or "" when the
// hostname is unavailable.
func (r *SeiNodeDeploymentReconciler) p2pEndpointAddress(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	host := r.p2pEndpointHostname(group, ordinal)
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, seiconfig.PortP2P)
}

func p2pEndpointServiceName(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	return seiNodeName(group, ordinal) + p2pEndpointServiceSuffix
}

// reconcileP2PEndpoints server-side-applies one LoadBalancer Service
// per desired ordinal and trims orphans.
func (r *SeiNodeDeploymentReconciler) reconcileP2PEndpoints(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	desiredNames := make(map[string]struct{}, group.Spec.Replicas)
	for i := range int(group.Spec.Replicas) {
		desired := generateP2PEndpointService(group, i, r.p2pEndpointHostname(group, i), r.NLBTargetType)
		desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
		if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference on P2P endpoint Service %s: %w", desired.Name, err)
		}
		//nolint:staticcheck // SSA apply via untyped object mirrors the rest of this controller
		if err := r.Patch(ctx, desired, client.Apply, p2pEndpointFieldOwner, client.ForceOwnership); err != nil {
			return fmt.Errorf("applying P2P endpoint Service %s: %w", desired.Name, err)
		}
		desiredNames[desired.Name] = struct{}{}
	}

	if err := r.deleteOrphanP2PEndpoints(ctx, group, desiredNames); err != nil {
		return fmt.Errorf("trimming orphan P2P endpoint Services: %w", err)
	}

	setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionTrue,
		"P2PEndpointsApplied",
		fmt.Sprintf("%d P2P endpoint Service(s) reconciled", len(desiredNames)))
	return nil
}

// deleteOrphanP2PEndpoints removes P2P endpoint Services owned by the
// SND whose names are not in desiredNames. Pass nil to delete all.
func (r *SeiNodeDeploymentReconciler) deleteOrphanP2PEndpoints(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, desiredNames map[string]struct{}) error {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list,
		client.InNamespace(group.Namespace),
		client.MatchingLabels{
			groupLabel:                group.Name,
			p2pEndpointComponentLabel: p2pEndpointComponentValue,
		},
	); err != nil {
		return fmt.Errorf("listing P2P endpoint Services: %w", err)
	}
	for i := range list.Items {
		svc := &list.Items[i]
		if _, ok := desiredNames[svc.Name]; ok {
			continue
		}
		if !metav1.IsControlledBy(svc, group) {
			continue
		}
		if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting orphan P2P endpoint Service %s: %w", svc.Name, err)
		}
	}
	return nil
}

func (r *SeiNodeDeploymentReconciler) deleteP2PEndpoints(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	return r.deleteOrphanP2PEndpoints(ctx, group, nil)
}

// orphanP2PEndpoints drops the SND owner ref so the Services survive
// SND deletion under DeletionPolicyRetain.
func (r *SeiNodeDeploymentReconciler) orphanP2PEndpoints(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list,
		client.InNamespace(group.Namespace),
		client.MatchingLabels{
			groupLabel:                group.Name,
			p2pEndpointComponentLabel: p2pEndpointComponentValue,
		},
	); err != nil {
		return fmt.Errorf("listing P2P endpoint Services for orphan: %w", err)
	}
	for i := range list.Items {
		if err := r.removeOwnerRef(ctx, &list.Items[i], group); err != nil {
			return fmt.Errorf("orphaning P2P endpoint Service %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

// deleteP2PEndpointForOrdinal removes the per-ordinal Service before
// scaleDown deletes the child. Idempotent.
func (r *SeiNodeDeploymentReconciler) deleteP2PEndpointForOrdinal(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, ordinal int) error {
	name := p2pEndpointServiceName(group, ordinal)
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: group.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching P2P endpoint Service %s for delete: %w", name, err)
	}
	if !metav1.IsControlledBy(svc, group) {
		return nil
	}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting P2P endpoint Service %s: %w", name, err)
	}
	return nil
}

func generateP2PEndpointService(group *seiv1alpha1.SeiNodeDeployment, ordinal int, hostname, targetType string) *corev1.Service {
	name := p2pEndpointServiceName(group, ordinal)
	childName := seiNodeName(group, ordinal)

	labels := map[string]string{
		groupLabel:                group.Name,
		groupOrdinalLabel:         strconv.Itoa(ordinal),
		p2pEndpointComponentLabel: p2pEndpointComponentValue,
	}

	annotations := map[string]string{
		externalDNSHostnameAnnotation: hostname,
		awsLBTypeAnnotation:           awsLBTypeValue,
		awsLBSchemeAnnotation:         awsLBSchemeValue,
		awsLBTargetTypeAnnotation:     targetType,
		awsLBCrossZoneAnnotation:      awsLBCrossZoneAttributeStr,
		managedByAnnotation:           controllerName,
	}

	// target-type=ip registers pod IPs directly; NodePort is not used so
	// we explicitly disable allocation to preserve the limited 30000-32767
	// range. target-type=instance requires a NodePort for the NLB→node→pod
	// hop (kube-proxy / Cilium socket-LB), so leave allocation at the
	// kube default (true) by passing nil.
	var allocateNodePorts *bool
	if targetType == NLBTargetTypeIP {
		f := false
		allocateNodePorts = &f
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   group.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy:         corev1.ServiceExternalTrafficPolicyTypeLocal,
			AllocateLoadBalancerNodePorts: allocateNodePorts,
			Selector: map[string]string{
				noderesource.NodeLabel: childName,
			},
			Ports: []corev1.ServicePort{{
				Name:       "p2p",
				Port:       seiconfig.PortP2P,
				TargetPort: intstr.FromInt32(seiconfig.PortP2P),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
