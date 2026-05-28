// Package nodedeployment — publishable.go is the SND-side networking sub-reconciler
// for per-pod P2P LoadBalancer Services. Owns the deterministic vanity-hostname
// derivation, the Service apply/delete lifecycle, and the population of
// `Status.NetworkingStatus.PublishableEndpoints`. The child SeiNode reconciler
// never reads from this file — it observes `Spec.ExternalAddress` and proceeds.
//
// Lifecycle invariants:
//
//   - Hostnames are derived purely from SND identity (`Name`, `ChainID`, the
//     controller's `GatewayPublicDomain`). No Service status read-back, no AWS
//     NLB hostname plumbed into the child.
//   - external-dns watches `Service` resources with the
//     `external-dns.alpha.kubernetes.io/hostname` annotation and provisions a
//     CNAME from the vanity name to the NLB's allocated DNS. The pod itself
//     advertises the vanity name to peers via `p2p.external_address`; peers
//     resolve the CNAME at dial time. We never block STS creation on this.
//   - Owner ref is the SND, not the child. On scale-down we explicitly delete
//     the per-ordinal Service before deleting the child SeiNode; on full SND
//     deletion the owner-ref cascade handles cleanup.
//   - When the cluster lacks a parseable `SEI_VPC_CIDR`, this entire sub-path
//     is bypassed and `ConditionNetworkingReady = False / VPCCIDRNotConfigured`.
//     The controller never per-reconcile-introspects Pod IPs — misconfiguration
//     surfaces as NLB target-registration failure events on the LB Service,
//     which is the AWS-side ground truth.
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
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const (
	// publishableFieldOwner isolates the publishable Service apply path from
	// the rest of the SND's SSA writes so the two paths cannot fight each other
	// over field ownership.
	publishableFieldOwner = client.FieldOwner("nodedeployment-controller-publishable")

	// publishableComponentLabel marks Services owned by this sub-reconciler so
	// list-based cleanup can target them without entangling the HTTP path's
	// external Service.
	publishableComponentLabel = "sei.io/component"
	publishableComponentValue = "p2p-lb"

	// externalDNSHostnameAnnotation is the external-dns annotation that drives
	// CNAME creation from the vanity hostname to the NLB's allocated DNS name.
	// Pinned key — changing it would orphan in-cluster DNS records.
	externalDNSHostnameAnnotation = "external-dns.alpha.kubernetes.io/hostname"

	// AWS Load Balancer Controller annotations for an internet-facing NLB
	// with IP targets and cross-zone load balancing. Hardcoded for the
	// single use case in scope; revisit if per-cluster knobs land.
	awsLBTypeAnnotation        = "service.beta.kubernetes.io/aws-load-balancer-type"
	awsLBSchemeAnnotation      = "service.beta.kubernetes.io/aws-load-balancer-scheme"
	awsLBTargetTypeAnnotation  = "service.beta.kubernetes.io/aws-load-balancer-nlb-target-type"
	awsLBCrossZoneAnnotation   = "service.beta.kubernetes.io/aws-load-balancer-attributes"
	awsLBTypeValue             = "external"
	awsLBSchemeValue           = "internet-facing"
	awsLBTargetTypeValue       = "ip"
	awsLBCrossZoneAttributeStr = "load_balancing.cross_zone.enabled=true"

	// publishableServiceSuffix is appended to the child SeiNode name to form
	// the per-pod LB Service name. Stable identifier — operators key dashboards
	// and runbooks off it.
	publishableServiceSuffix = "-p2p"
)

// effectiveChainID returns the chain ID the SND will stamp into child
// SeiNodes. Mirrors the precedence used by `generateSeiNode`: the
// template's explicit value wins, otherwise fall back to the Genesis
// ceremony's ChainID. Validators always set template ChainID via the
// ceremony override at child create, but a non-validator SND has no
// Genesis block and relies entirely on the template value.
func effectiveChainID(group *seiv1alpha1.SeiNodeDeployment) string {
	if id := group.Spec.Template.Spec.ChainID; id != "" {
		return id
	}
	if group.Spec.Genesis != nil {
		return group.Spec.Genesis.ChainID
	}
	return ""
}

// publishableHostname is the deterministic vanity hostname for the given
// ordinal. Returns "" if the inputs cannot produce a valid DNS-1123
// hostname (missing chain ID, missing public domain, or chain ID with
// DNS-illegal characters that slipped past admission). Callers must
// treat the empty string as "skip" — the apply boundary fails closed.
func (r *SeiNodeDeploymentReconciler) publishableHostname(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	if r.GatewayPublicDomain == "" {
		return ""
	}
	chainID := effectiveChainID(group)
	if chainID == "" {
		return ""
	}
	// Belt-and-suspenders: the API patterns reject DNS-illegal IDs at
	// admission, but admission rules can be bypassed (cluster CRD edits,
	// older CRD schema in-cluster). Validate the chain ID's label shape
	// before composing it into a host.
	if errs := validation.IsDNS1123Label(chainID); len(errs) > 0 {
		return ""
	}
	host := fmt.Sprintf("%s-p2p.%s.%s",
		seiNodeName(group, ordinal),
		chainID,
		r.GatewayPublicDomain,
	)
	return host
}

// publishableExternalAddress returns "<hostname>:<p2p-port>" suitable for
// CometBFT's `p2p.external_address`. Empty when the hostname cannot be
// derived (see publishableHostname).
func (r *SeiNodeDeploymentReconciler) publishableExternalAddress(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	host := r.publishableHostname(group, ordinal)
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, seiconfig.PortP2P)
}

// publishableServiceName is the per-pod LB Service name. Stable across
// SND edits — only the ordinal can shift it.
func publishableServiceName(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	return seiNodeName(group, ordinal) + publishableServiceSuffix
}

// reconcilePublishableServices server-side-applies one LoadBalancer
// Service per desired ordinal and trims any orphan Services left over
// from scale-down events the controller missed.
func (r *SeiNodeDeploymentReconciler) reconcilePublishableServices(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	desiredNames := make(map[string]struct{}, group.Spec.Replicas)
	for i := range int(group.Spec.Replicas) {
		host := r.publishableHostname(group, i)
		if host == "" {
			// Capability gating happens upstream in reconcileNetworking;
			// the only way to reach this branch is a chain ID that
			// somehow escaped admission validation. Fail closed: set the
			// stable False reason, do not apply.
			setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionFalse,
				"InvalidChainIDForDNS",
				fmt.Sprintf("chainID %q is not a valid DNS-1123 label; cannot compose publishable hostname", effectiveChainID(group)))
			return nil
		}
		desired := generatePublishableService(group, i, host)
		desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
		if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference on publishable Service %s: %w", desired.Name, err)
		}
		//nolint:staticcheck // SSA apply via untyped object mirrors the rest of this controller; typed ApplyConfiguration is a separate effort
		if err := r.Patch(ctx, desired, client.Apply, publishableFieldOwner, client.ForceOwnership); err != nil {
			return fmt.Errorf("applying publishable Service %s: %w", desired.Name, err)
		}
		desiredNames[desired.Name] = struct{}{}
	}

	if err := r.deleteOrphanPublishableServices(ctx, group, desiredNames); err != nil {
		return fmt.Errorf("trimming orphan publishable Services: %w", err)
	}

	setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionTrue,
		"PublishableServicesApplied",
		fmt.Sprintf("%d publishable Service(s) reconciled", len(desiredNames)))
	return nil
}

// deleteOrphanPublishableServices removes publishable Services labelled
// for this SND whose names are not in desiredNames. Used after a
// scale-down or replica reset to clear stranded LB Services.
func (r *SeiNodeDeploymentReconciler) deleteOrphanPublishableServices(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, desiredNames map[string]struct{}) error {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list,
		client.InNamespace(group.Namespace),
		client.MatchingLabels{
			groupLabel:                group.Name,
			publishableComponentLabel: publishableComponentValue,
		},
	); err != nil {
		return fmt.Errorf("listing publishable Services: %w", err)
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
			return fmt.Errorf("deleting orphan publishable Service %s: %w", svc.Name, err)
		}
	}
	return nil
}

// deletePublishableServices removes every publishable Service owned by
// the SND. Called on opt-out (TCP unset on a previously-publishable SND)
// and as part of the general networking-resources cleanup path.
func (r *SeiNodeDeploymentReconciler) deletePublishableServices(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	return r.deleteOrphanPublishableServices(ctx, group, nil)
}

// deletePublishableServiceForOrdinal is the explicit per-ordinal delete
// scaleDown calls before deleting the child SeiNode. Idempotent —
// silently no-ops when the Service is already gone.
func (r *SeiNodeDeploymentReconciler) deletePublishableServiceForOrdinal(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, ordinal int) error {
	name := publishableServiceName(group, ordinal)
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: group.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching publishable Service %s for delete: %w", name, err)
	}
	if !metav1.IsControlledBy(svc, group) {
		return nil
	}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting publishable Service %s: %w", name, err)
	}
	return nil
}

// generatePublishableService is a pure factory — given an SND, ordinal,
// and the deterministic vanity hostname, returns the desired Service
// spec. No external state, no API reads. The annotation set drives AWS
// LBC and external-dns; the selector reuses the SeiNode-controller's
// `sei.io/node=<name>` label, which is stamped onto the pod by the
// StatefulSet template.
func generatePublishableService(group *seiv1alpha1.SeiNodeDeployment, ordinal int, hostname string) *corev1.Service {
	name := publishableServiceName(group, ordinal)
	childName := seiNodeName(group, ordinal)

	labels := map[string]string{
		groupLabel:                group.Name,
		groupOrdinalLabel:         strconv.Itoa(ordinal),
		publishableComponentLabel: publishableComponentValue,
	}

	annotations := map[string]string{
		externalDNSHostnameAnnotation: hostname,
		awsLBTypeAnnotation:           awsLBTypeValue,
		awsLBSchemeAnnotation:         awsLBSchemeValue,
		awsLBTargetTypeAnnotation:     awsLBTargetTypeValue,
		awsLBCrossZoneAnnotation:      awsLBCrossZoneAttributeStr,
		managedByAnnotation:           controllerName,
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
			AllocateLoadBalancerNodePorts: new(bool), // explicit false; NLBs use IP targets, not NodePort routing
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
