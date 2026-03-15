package node

import (
	"fmt"
	"net/url"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const defaultSidecarPort int32 = 7777

func generateNodeStatefulSet(node *seiv1alpha1.SeiNode) *appsv1.StatefulSet {
	one := int32(1)
	labels := resourceLabelsForNode(node)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &one,
			ServiceName: node.Name,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       buildNodePodSpec(node),
			},
		},
	}
}

func buildNodePodSpec(node *seiv1alpha1.SeiNode) corev1.PodSpec {
	dataVolume := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: nodeDataPVCClaimName(node),
			},
		},
	}

	spec := corev1.PodSpec{
		ServiceAccountName: nodeServiceAccount,
		Tolerations: []corev1.Toleration{
			{Key: "sei.io/workload", Value: "sei-node", Effect: corev1.TaintEffectNoSchedule},
		},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "karpenter.sh/nodepool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"sei-node"},
						}},
					}},
				},
			},
		},
		Volumes: []corev1.Volume{dataVolume},
	}

	spec.ShareProcessNamespace = ptr.To(true)
	spec.InitContainers = []corev1.Container{
		buildSeidInitContainer(node),
		buildSidecarContainer(node),
	}
	spec.Containers = []corev1.Container{buildSidecarMainContainer(node)}

	return spec
}

func sidecarImage(node *seiv1alpha1.SeiNode) string {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Image != "" {
		return node.Spec.Sidecar.Image
	}
	return defaultSidecarImage
}

func sidecarPort(node *seiv1alpha1.SeiNode) int32 {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Port != 0 {
		return node.Spec.Sidecar.Port
	}
	return defaultSidecarPort
}

// buildSidecarContainer constructs the sei-sidecar as a restartable init
// container that runs alongside seid for the pod's lifetime.
func buildSidecarContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	port := sidecarPort(node)
	c := corev1.Container{
		Name:          "sei-sidecar",
		Image:         sidecarImage(node),
		Command:       []string{"seictl", "serve"},
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Env: []corev1.EnvVar{
			{Name: "SEI_SIDECAR_PORT", Value: fmt.Sprintf("%d", port)},
			{Name: "SEI_HOME", Value: dataDir},
		},
		Ports: []corev1.ContainerPort{
			{Name: "sidecar", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataDir},
		},
	}
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Resources != nil {
		c.Resources = *node.Spec.Sidecar.Resources
	}
	return c
}

// buildSidecarMainContainer builds the seid container that waits for the
// sidecar to become healthy before exec'ing seid. Kubernetes starts main
// containers as soon as restartable init containers are running, so we
// wrap the entrypoint in a shell loop that polls /healthz until 200.
func buildSidecarMainContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	container := buildNodeMainContainer(node)
	container.Command, container.Args = sidecarWaitCommand(node)
	container.Resources = defaultResourcesForMode(node.Spec.Mode)
	container.StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/v0/healthz",
				Port: intstr.FromInt32(sidecarPort(node)),
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		FailureThreshold:    86400,
	}
	return container
}

// sidecarWaitCommand wraps the node's entrypoint in a shell polling loop
// that blocks until the sidecar's /healthz returns 200, then exec's seid.
func sidecarWaitCommand(node *seiv1alpha1.SeiNode) (command []string, args []string) {
	cmd := "seid"
	var cmdArgs []string
	if node.Spec.Entrypoint != nil && len(node.Spec.Entrypoint.Command) > 0 {
		cmd = node.Spec.Entrypoint.Command[0]
		cmdArgs = append(node.Spec.Entrypoint.Command[1:], node.Spec.Entrypoint.Args...)
	}

	var b strings.Builder
	b.WriteString(cmd)
	for _, a := range cmdArgs {
		fmt.Fprintf(&b, " %q", a)
	}

	script := fmt.Sprintf(
		`echo "waiting for sidecar to become ready..."; `+
			`while true; do `+
			`{ exec 3<>/dev/tcp/localhost/%d; } 2>/dev/null && `+
			`printf "GET /v0/healthz HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3 && `+
			`head -1 <&3 | grep -q "200" && break; `+
			`exec 3>&-; sleep 5; done; `+
			`exec 3>&-; `+
			`echo "sidecar ready, starting seid"; `+
			`exec %s`,
		sidecarPort(node), b.String(),
	)

	return []string{"/bin/bash", "-c"}, []string{script}
}

// nodeDataPVCClaimName returns the PVC name to mount as the data volume.
// Genesis nodes reference the PVC pre-provisioned by SeiNodePool's prep Job.
// Snapshot nodes reference the PVC created by the SeiNode controller.
func nodeDataPVCClaimName(node *seiv1alpha1.SeiNode) string {
	if hasGenesisPVC(node) {
		return node.Spec.Genesis.PVC.DataPVC
	}
	return nodeDataPVCName(node)
}

func buildNodeMainContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	container := corev1.Container{
		Name:  "seid",
		Image: node.Spec.Image,
		Env: []corev1.EnvVar{
			{Name: "TMPDIR", Value: dataDir + "/tmp"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataDir},
		},
		Ports: containerPorts(),
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(26657),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			FailureThreshold:    30,
		},
	}

	if node.Spec.Entrypoint != nil {
		container.Command = node.Spec.Entrypoint.Command
		container.Args = node.Spec.Entrypoint.Args
	}

	// State-synced nodes replay blocks from the snapshot/sync height to the chain tip.
	// This can take several hours on a live chain.
	if needsStateSync(node) {
		container.StartupProbe.FailureThreshold = 1800
	}

	return container
}

// buildSeidInitContainer creates the init container that bootstraps the seid
// home directory. When a SeiNodePool prep job has already populated the PVC
// (genesis.json, validator keys, config, etc.), running "seid init --overwrite"
// would destroy that state and produce an empty genesis with no validators.
// The genesis.json guard below is a stopgap; ideally the SeiNode spec should
// carry an explicit field (e.g. spec.skipInit) so the controller can
// distinguish pre-populated volumes from fresh ones without filesystem probes.
func buildSeidInitContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	script := fmt.Sprintf(
		`if [ -f %s/config/genesis.json ]; then echo "data directory already initialized, skipping seid init"; else seid init %s --chain-id %s --home %s --overwrite; fi && mkdir -p %s/tmp`,
		dataDir, node.Spec.ChainID, node.Spec.ChainID, dataDir, dataDir,
	)
	return corev1.Container{
		Name:  "seid-init",
		Image: node.Spec.Image,
		Command: []string{
			"/bin/sh", "-c", script,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataDir},
		},
	}
}

func generateNodeHeadlessService(node *seiv1alpha1.SeiNode) *corev1.Service {
	labels := resourceLabelsForNode(node)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 labels,
			Ports:                    servicePorts(),
			PublishNotReadyAddresses: true,
		},
	}
}

func generateNodeDataPVC(node *seiv1alpha1.SeiNode) *corev1.PersistentVolumeClaim {
	sc, size := defaultStorageForMode(node.Spec.Mode)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeDataPVCName(node),
			Namespace: node.Namespace,
			Labels:    resourceLabelsForNode(node),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}

	return pvc
}

func containerPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{Name: "evm-rpc", ContainerPort: 8545, Protocol: corev1.ProtocolTCP},
		{Name: "evm-ws", ContainerPort: 8546, Protocol: corev1.ProtocolTCP},
		{Name: "grpc", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
		{Name: "p2p", ContainerPort: 26656, Protocol: corev1.ProtocolTCP},
		{Name: "rpc", ContainerPort: 26657, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: 26660, Protocol: corev1.ProtocolTCP},
	}
}

func servicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "evm-rpc", Port: 8545, TargetPort: intstr.FromInt32(8545), Protocol: corev1.ProtocolTCP},
		{Name: "evm-ws", Port: 8546, TargetPort: intstr.FromInt32(8546), Protocol: corev1.ProtocolTCP},
		{Name: "grpc", Port: 9090, TargetPort: intstr.FromInt32(9090), Protocol: corev1.ProtocolTCP},
		{Name: "p2p", Port: 26656, TargetPort: intstr.FromInt32(26656), Protocol: corev1.ProtocolTCP},
		{Name: "rpc", Port: 26657, TargetPort: intstr.FromInt32(26657), Protocol: corev1.ProtocolTCP},
		{Name: "metrics", Port: 26660, TargetPort: intstr.FromInt32(26660), Protocol: corev1.ProtocolTCP},
	}
}

// parseS3URI splits an s3://bucket/prefix URI into its bucket and prefix parts.
func parseS3URI(uri string) (bucket, prefix string) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return uri, ""
	}
	bucket = u.Host
	prefix = strings.TrimPrefix(u.Path, "/")
	return bucket, prefix
}
