package nodedeployment

import (
	"context"
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// internalServiceName returns the deterministic name of the ClusterIP
// Service that publishes in-cluster access for a SeiNodeDeployment.
func internalServiceName(group *seiv1alpha1.SeiNodeDeployment) string {
	return fmt.Sprintf("%s-internal", group.Name)
}

// reconcileInternalService applies the cluster-internal ClusterIP Service
// that kube-proxy load-balances across healthy child pods and stamps
// status.internalService on the group. This runs unconditionally — it is
// independent of .spec.networking and lives alongside the external Service /
// HTTPRoute reconciliation.
func (r *SeiNodeDeploymentReconciler) reconcileInternalService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	desired := generateInternalService(group)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on internal Service: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying internal Service: %w", err)
	}

	group.Status.InternalService = &seiv1alpha1.InternalServiceStatus{
		Name:      desired.Name,
		Namespace: desired.Namespace,
		Ports: seiv1alpha1.InternalServicePorts{
			Rpc:     seiconfig.PortRPC,
			EvmHttp: seiconfig.PortEVMHTTP,
			Rest:    seiconfig.PortREST,
		},
	}
	return nil
}

// orphanInternalService strips the owner reference on the internal Service
// so the resource survives parent deletion under DeletionPolicy=Retain. The
// internal Service's lifecycle is unconditional — it does not participate
// in the networking-resources teardown path — so its retain handling lives
// alongside its create path, not alongside orphanNetworkingResources.
func (r *SeiNodeDeploymentReconciler) orphanInternalService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: internalServiceName(group), Namespace: group.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching internal Service for orphan: %w", err)
	}
	if err := r.removeOwnerRef(ctx, svc, group); err != nil {
		return fmt.Errorf("orphaning internal Service: %w", err)
	}
	return nil
}

// generateInternalService is a pure function that produces the desired
// ClusterIP Service for in-cluster consumers. The Service is always created
// regardless of .spec.networking. kube-proxy load-balances traffic across
// ready child pods using the deployment-scoped pod label.
//
// Only stateless HTTP request/response ports are exposed (rpc, evm-http,
// rest). Stateful protocols — EVM WebSocket (8546), gRPC / HTTP/2 streaming
// (9090), P2P gossip — do not work correctly behind a kube-proxy L4 LB
// (WebSocket subscriptions pin state to a pod; HTTP/2 connections pin
// per-connection). Consumers needing those use the per-node headless
// Services for deterministic per-pod addressing.
func generateInternalService(group *seiv1alpha1.SeiNodeDeployment) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        internalServiceName(group),
			Namespace:   group.Namespace,
			Labels:      resourceLabels(group),
			Annotations: managedByAnnotations(),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Select by deployment membership only (not revision): during a
			// rollout, kube-proxy should keep routing to whichever pods are
			// Ready, irrespective of which generation they belong to.
			Selector: groupOnlySelector(group),
			// kube-proxy must exclude not-ready pods so clients never see
			// partially-bootstrapped nodes.
			PublishNotReadyAddresses: false,
			Ports: []corev1.ServicePort{
				{
					Name:       "rpc",
					Port:       seiconfig.PortRPC,
					TargetPort: intstr.FromInt32(seiconfig.PortRPC),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "evm-http",
					Port:       seiconfig.PortEVMHTTP,
					TargetPort: intstr.FromInt32(seiconfig.PortEVMHTTP),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "rest",
					Port:       seiconfig.PortREST,
					TargetPort: intstr.FromInt32(seiconfig.PortREST),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}
