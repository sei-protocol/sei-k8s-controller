package nodedeployment

import (
	"context"
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// internalRpcServiceName returns the deterministic name of the ClusterIP
// Service that publishes in-cluster RPC for a SeiNodeDeployment.
func internalRpcServiceName(group *seiv1alpha1.SeiNodeDeployment) string {
	return fmt.Sprintf("%s-rpc", group.Name)
}

// reconcileInternalRpcService applies the cluster-internal ClusterIP Service
// that kube-proxy load-balances across healthy child pods and stamps
// status.rpcService on the group. This runs unconditionally — it is
// independent of .spec.networking and lives alongside the external Service
// / HTTPRoute reconciliation.
func (r *SeiNodeDeploymentReconciler) reconcileInternalRpcService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	desired := generateInternalRpcService(group)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on internal RPC Service: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying internal RPC Service: %w", err)
	}

	group.Status.RpcService = &seiv1alpha1.RpcServiceStatus{
		Name:      desired.Name,
		Namespace: desired.Namespace,
		Ports: seiv1alpha1.RpcServicePorts{
			Rpc:     seiconfig.PortRPC,
			EvmHttp: seiconfig.PortEVMHTTP,
			EvmWs:   seiconfig.PortEVMWS,
			Rest:    seiconfig.PortREST,
			Grpc:    seiconfig.PortGRPC,
		},
	}
	return nil
}

// generateInternalRpcService is a pure function that produces the desired
// ClusterIP Service for in-cluster RPC consumers. The Service is always
// created regardless of .spec.networking. kube-proxy load-balances traffic
// across ready child pods using the deployment-scoped pod label.
//
// Port names match the milestone contract (rpc, evm-http, evm-ws, rest, grpc),
// which differs from seiconfig.NodePorts() (which uses "evm-rpc") — these names
// are part of the interface contract and intentionally kube-native-friendly.
func generateInternalRpcService(group *seiv1alpha1.SeiNodeDeployment) *corev1.Service {
	h2c := "kubernetes.io/h2c"
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        internalRpcServiceName(group),
			Namespace:   group.Namespace,
			Labels:      resourceLabels(group),
			Annotations: managedByAnnotations(),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Select by deployment membership only (not revision): during
			// a rollout, kube-proxy should keep routing to whichever pods
			// are Ready, irrespective of which generation they belong to.
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
					Name:       "evm-ws",
					Port:       seiconfig.PortEVMWS,
					TargetPort: intstr.FromInt32(seiconfig.PortEVMWS),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "rest",
					Port:       seiconfig.PortREST,
					TargetPort: intstr.FromInt32(seiconfig.PortREST),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:        "grpc",
					Port:        seiconfig.PortGRPC,
					TargetPort:  intstr.FromInt32(seiconfig.PortGRPC),
					Protocol:    corev1.ProtocolTCP,
					AppProtocol: &h2c,
				},
			},
		},
	}
}
