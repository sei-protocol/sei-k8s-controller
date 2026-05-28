// Package nodedeployment — publishable.go is the SND-side sub-reconciler
// for per-pod P2P LoadBalancer Services. Hostnames are derived from SND
// identity; external-dns creates the CNAME to the NLB. The child SeiNode
// reconciler is unaware and just reads Spec.ExternalAddress.
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
	publishableFieldOwner = client.FieldOwner("nodedeployment-controller-publishable")

	publishableComponentLabel = "sei.io/component"
	publishableComponentValue = "p2p-lb"

	externalDNSHostnameAnnotation = "external-dns.alpha.kubernetes.io/hostname"

	awsLBTypeAnnotation        = "service.beta.kubernetes.io/aws-load-balancer-type"
	awsLBSchemeAnnotation      = "service.beta.kubernetes.io/aws-load-balancer-scheme"
	awsLBTargetTypeAnnotation  = "service.beta.kubernetes.io/aws-load-balancer-nlb-target-type"
	awsLBCrossZoneAnnotation   = "service.beta.kubernetes.io/aws-load-balancer-attributes"
	awsLBTypeValue             = "external"
	awsLBSchemeValue           = "internet-facing"
	awsLBTargetTypeValue       = "ip"
	awsLBCrossZoneAttributeStr = "load_balancing.cross_zone.enabled=true"

	publishableServiceSuffix = "-p2p"
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

// publishableHostname returns "<seinode>-p2p.<chainID>.<domain>" or "" when
// the publishable path is not configured.
func (r *SeiNodeDeploymentReconciler) publishableHostname(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	if r.PublishableDomain == "" {
		return ""
	}
	chainID := effectiveChainID(group)
	if chainID == "" {
		return ""
	}
	return fmt.Sprintf("%s-p2p.%s.%s", seiNodeName(group, ordinal), chainID, r.PublishableDomain)
}

// publishableExternalAddress returns "<hostname>:<port>" or "" when the
// hostname is unavailable.
func (r *SeiNodeDeploymentReconciler) publishableExternalAddress(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	host := r.publishableHostname(group, ordinal)
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, seiconfig.PortP2P)
}

func publishableServiceName(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	return seiNodeName(group, ordinal) + publishableServiceSuffix
}

// reconcilePublishableServices server-side-applies one LoadBalancer Service
// per desired ordinal and trims orphans.
func (r *SeiNodeDeploymentReconciler) reconcilePublishableServices(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	desiredNames := make(map[string]struct{}, group.Spec.Replicas)
	for i := range int(group.Spec.Replicas) {
		desired := generatePublishableService(group, i, r.publishableHostname(group, i))
		desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
		if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference on publishable Service %s: %w", desired.Name, err)
		}
		//nolint:staticcheck // SSA apply via untyped object mirrors the rest of this controller
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

// deleteOrphanPublishableServices removes publishable Services owned by the
// SND whose names are not in desiredNames. Pass nil to delete all.
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

func (r *SeiNodeDeploymentReconciler) deletePublishableServices(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	return r.deleteOrphanPublishableServices(ctx, group, nil)
}

// orphanPublishableServices drops the SND owner ref so the Services survive
// SND deletion under DeletionPolicyRetain.
func (r *SeiNodeDeploymentReconciler) orphanPublishableServices(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list,
		client.InNamespace(group.Namespace),
		client.MatchingLabels{
			groupLabel:                group.Name,
			publishableComponentLabel: publishableComponentValue,
		},
	); err != nil {
		return fmt.Errorf("listing publishable Services for orphan: %w", err)
	}
	for i := range list.Items {
		if err := r.removeOwnerRef(ctx, &list.Items[i], group); err != nil {
			return fmt.Errorf("orphaning publishable Service %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

// deletePublishableServiceForOrdinal removes the per-ordinal Service before
// scaleDown deletes the child. Idempotent.
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
			AllocateLoadBalancerNodePorts: new(bool),
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
