package seinetwork

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
// Service that publishes in-cluster access for a SeiNetwork.
func internalServiceName(network *seiv1alpha1.SeiNetwork) string {
	return fmt.Sprintf("%s-internal", network.Name)
}

// reconcileInternalService applies the cluster-internal ClusterIP Service
// that kube-proxy load-balances across healthy child pods and stamps
// status.internalService on the network. This runs unconditionally —
// external networking is GitOps-owned (PLT-451); the internal ClusterIP is
// not, and the dev/bench flows use it for in-cluster RPC discovery.
func (r *SeiNetworkReconciler) reconcileInternalService(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	desired := generateInternalService(network)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(network, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on internal Service: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying internal Service: %w", err)
	}

	network.Status.InternalService = &seiv1alpha1.InternalServiceStatus{
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
// so the resource survives parent deletion under DeletionPolicy=Retain.
func (r *SeiNetworkReconciler) orphanInternalService(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: internalServiceName(network), Namespace: network.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching internal Service for orphan: %w", err)
	}
	if err := r.removeOwnerRef(ctx, svc, network); err != nil {
		return fmt.Errorf("orphaning internal Service: %w", err)
	}
	return nil
}

// generateInternalService is a pure function that produces the desired
// ClusterIP Service for in-cluster consumers. kube-proxy load-balances
// traffic across ready child pods using the network-scoped pod label.
//
// Only stateless HTTP request/response ports are exposed (rpc, evm-http,
// rest). Stateful protocols — EVM WebSocket (8546), gRPC / HTTP/2 streaming
// (9090), P2P gossip — do not work correctly behind a kube-proxy L4 LB
// (WebSocket subscriptions pin state to a pod; HTTP/2 connections pin
// per-connection). Consumers needing those use the per-node headless
// Services for deterministic per-pod addressing.
func generateInternalService(network *seiv1alpha1.SeiNetwork) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        internalServiceName(network),
			Namespace:   network.Namespace,
			Labels:      resourceLabels(network),
			Annotations: managedByAnnotations(),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Select by network membership: kube-proxy routes to whichever
			// pods are Ready, regardless of generation. InPlace rollouts
			// update pods in place, so pinning to a revision label would
			// strand traffic between the old and new generations.
			Selector: groupSelector(network),
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
