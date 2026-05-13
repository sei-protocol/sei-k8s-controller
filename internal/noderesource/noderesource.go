// Package noderesource contains pure generation functions for Kubernetes
// resources owned by a SeiNode. Both the node controller and plan task
// implementations import this package to produce StatefulSets, Services,
// and PVCs from a SeiNode spec.
package noderesource

import (
	"fmt"
	"maps"
	"strings"

	seiconfig "github.com/sei-protocol/sei-config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

const (
	// NodeLabel is the standard label key used on all SeiNode-owned resources.
	NodeLabel = "sei.io/node"

	dataDir               = platform.DataDir
	defaultSidecarImage   = platform.DefaultSidecarImage
	defaultRBACProxyImage = platform.DefaultRBACProxyImage

	// Pod-spec container names. Used as both the .Name on built containers
	// and the lookup key for the operator-keyring containment guard.
	containerNameSeid               = "seid"
	containerNameSidecar            = "sei-sidecar"
	containerNameRBACProxy          = "kube-rbac-proxy"
	servicePortNameAPI              = "api"
	rbacProxyConfigVolumeName       = "rbac-proxy-config"
	sidecarTLSVolumeName            = "sidecar-tls"
	rbacProxyConfigMountPath        = "/etc/kube-rbac-proxy"
	sidecarTLSMountPath             = "/etc/tls"
	rbacProxyPort             int32 = 8443

	pathHealthz  = "/v0/healthz"
	pathLivez    = "/v0/livez"
	pathStartupz = "/v0/startupz"
	pathMetrics  = "/v0/metrics"

	signingKeyVolumeName    = "signing-key"
	privValidatorKeyDataKey = "priv_validator_key.json"

	nodeKeyVolumeName = "node-key"
	nodeKeyDataKey    = "node_key.json"

	operatorKeyringVolumeName = "operator-keyring"
	// operatorKeyringDirName is fixed by the Cosmos SDK file-backend keyring:
	// keyring.New(name, BackendFile, homeDir, ...) opens homeDir/keyring-file/.
	// Not a controller choice; this constant mirrors the SDK contract.
	operatorKeyringDirName  = "keyring-file"
	keyringPassphraseEnvVar = "SEI_KEYRING_PASSPHRASE"

	// sidecarTmpVolumeName backs an emptyDir at /tmp — required because the
	// sidecar runs with ReadOnlyRootFilesystem and Go stdlib defaults to /tmp.
	sidecarTmpVolumeName = "sidecar-tmp"

	// sidecarNonRootUID is the nonroot UID/GID baked into distroless and
	// chainguard static-debian12 base images. Pod-level fsGroup matches so
	// the non-root sidecar can read kubelet-projected 0o400 Secret files.
	sidecarNonRootUID int64 = 65532
)

// PlatformConfig is an alias for platform.Config.
type PlatformConfig = platform.Config

// DataPVCName returns the PVC name for a node's data volume.
func DataPVCName(node *seiv1alpha1.SeiNode) string {
	if dv := node.Spec.DataVolume; dv != nil && dv.Import != nil && dv.Import.PVCName != "" {
		return dv.Import.PVCName
	}
	return fmt.Sprintf("data-%s", node.Name)
}

// SelectorLabels returns the minimal, immutable label set used as the
// StatefulSet selector. Only sei.io/node is included because each SeiNode
// has a unique name within a namespace, making it sufficient to select its
// pods. Mutable labels must NOT appear here because Kubernetes forbids
// changing a StatefulSet's selector after creation.
func SelectorLabels(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{NodeLabel: node.Name}
}

// ResourceLabels returns labels for the StatefulSet pod template.
// User-provided podLabels are applied first; the system sei.io/node label
// is set last so it cannot be overridden.
func ResourceLabels(node *seiv1alpha1.SeiNode) map[string]string {
	labels := make(map[string]string, len(node.Spec.PodLabels)+1)
	maps.Copy(labels, node.Spec.PodLabels)
	labels[NodeLabel] = node.Name
	return labels
}

// NodeMode returns the sei-config mode string for the node based on which
// sub-spec is populated. Falls back to "full" if none is set.
func NodeMode(node *seiv1alpha1.SeiNode) string {
	switch {
	case node.Spec.Archive != nil:
		return string(seiconfig.ModeArchive)
	case node.Spec.Validator != nil:
		return string(seiconfig.ModeValidator)
	case node.Spec.Replayer != nil:
		return string(seiconfig.ModeFull)
	default:
		return string(seiconfig.ModeFull)
	}
}

// NeedsLongStartup returns true when the node's bootstrap strategy involves
// replaying blocks, requiring extended startup probe thresholds.
func NeedsLongStartup(node *seiv1alpha1.SeiNode) bool {
	switch {
	case node.Spec.FullNode != nil:
		return node.Spec.FullNode.Snapshot != nil
	case node.Spec.Validator != nil:
		return node.Spec.Validator.Snapshot != nil
	case node.Spec.Replayer != nil:
		return true
	case node.Spec.Archive != nil:
		return true
	default:
		return false
	}
}

// DefaultStorageForMode returns the StorageClass name and PVC size for a
// node based on its operating mode.
func DefaultStorageForMode(mode string, p PlatformConfig) (storageClass string, size string) {
	switch mode {
	case string(seiconfig.ModeArchive):
		return p.StorageClassArchive, p.StorageSizeArchive
	case string(seiconfig.ModeFull), string(seiconfig.ModeValidator):
		return p.StorageClassPerf, p.StorageSizeDefault
	default:
		return p.StorageClassDefault, p.StorageSizeDefault
	}
}

// DefaultResourcesForMode returns CPU and memory requests for the seid
// container based on the node's operating mode.
func DefaultResourcesForMode(mode string, p PlatformConfig) corev1.ResourceRequirements {
	switch mode {
	case string(seiconfig.ModeArchive):
		return makeResources(p.ResourceCPUArchive, p.ResourceMemArchive)
	default:
		return makeResources(p.ResourceCPUDefault, p.ResourceMemDefault)
	}
}

// ---------------------------------------------------------------------------
// StatefulSet generation
// ---------------------------------------------------------------------------

// GenerateStatefulSet produces the desired StatefulSet for a SeiNode.
//
// Returns an error if the resulting pod-spec violates the operator-keyring
// containment invariant — only the sidecar container may mount that volume,
// never seid main or any non-sidecar init container.
func GenerateStatefulSet(node *seiv1alpha1.SeiNode, p PlatformConfig) (*appsv1.StatefulSet, error) {
	one := int32(1)
	labels := ResourceLabels(node)
	podSpec := buildNodePodSpec(node, p)

	if err := assertNoOperatorKeyringOnSeidContainers(node, &podSpec); err != nil {
		return nil, err
	}

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
				MatchLabels: SelectorLabels(node),
			},
			// Pod lifecycle is the SeiNode controller's responsibility (replace-pod).
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"karpenter.sh/do-not-disrupt": "true",
					},
				},
				Spec: podSpec,
			},
		},
	}, nil
}

// assertNoOperatorKeyringOnSeidContainers fails closed if a future refactor
// lands operator-keyring material on the seid main container or a non-sidecar
// init container. Checks both the keyring volume mount AND env-var references
// to the passphrase Secret — either alone is enough for a compromised seid
// container to recover the unlocked operator key.
//
// No-op when the node has no operator-keyring configured.
func assertNoOperatorKeyringOnSeidContainers(node *seiv1alpha1.SeiNode, spec *corev1.PodSpec) error {
	src := operatorKeyringSecretSource(node)
	if src == nil {
		return nil
	}
	passphraseSecretName := src.PassphraseSecretRef.SecretName

	check := func(c *corev1.Container) error {
		for _, m := range c.VolumeMounts {
			if m.Name == operatorKeyringVolumeName {
				return fmt.Errorf("pod-spec for %s/%s mounts operator-keyring volume on container %q; "+
					"operator-keyring is exclusively the sidecar's — mounting on seid would collapse the sidecar/seid trust boundary",
					node.Namespace, node.Name, c.Name)
			}
		}
		for _, ev := range c.Env {
			if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil &&
				ev.ValueFrom.SecretKeyRef.Name == passphraseSecretName {
				return fmt.Errorf("pod-spec for %s/%s references operator-keyring passphrase Secret %q in env %q on container %q; "+
					"the passphrase is exclusively the sidecar's",
					node.Namespace, node.Name, passphraseSecretName, ev.Name, c.Name)
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == passphraseSecretName {
				return fmt.Errorf("pod-spec for %s/%s references operator-keyring passphrase Secret %q via envFrom on container %q; "+
					"the passphrase is exclusively the sidecar's",
					node.Namespace, node.Name, passphraseSecretName, c.Name)
			}
		}
		return nil
	}

	for i := range spec.Containers {
		if spec.Containers[i].Name == containerNameSidecar {
			continue
		}
		if err := check(&spec.Containers[i]); err != nil {
			return err
		}
	}
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == containerNameSidecar {
			continue
		}
		if err := check(&spec.InitContainers[i]); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Service generation
// ---------------------------------------------------------------------------

// GenerateHeadlessService produces the desired headless Service for a SeiNode.
func GenerateHeadlessService(node *seiv1alpha1.SeiNode) *corev1.Service {
	ports := ServicePorts()
	if SidecarTLSEnabled(node) {
		ports = append(ports, corev1.ServicePort{
			Name:       servicePortNameAPI,
			Port:       rbacProxyPort,
			TargetPort: intstr.FromInt32(rbacProxyPort),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
			Labels:    ResourceLabels(node),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 SelectorLabels(node),
			Ports:                    ports,
			PublishNotReadyAddresses: true,
		},
	}
}

// ---------------------------------------------------------------------------
// PVC generation
// ---------------------------------------------------------------------------

// GenerateDataPVC produces the desired PersistentVolumeClaim for a SeiNode's
// data volume.
func GenerateDataPVC(node *seiv1alpha1.SeiNode, p PlatformConfig) *corev1.PersistentVolumeClaim {
	sc, size := DefaultStorageForMode(NodeMode(node), p)

	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DataPVCName(node),
			Namespace: node.Namespace,
			Labels:    ResourceLabels(node),
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
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

// ContainerPorts returns the container port list for seid.
func ContainerPorts() []corev1.ContainerPort {
	np := seiconfig.NodePorts()
	ports := make([]corev1.ContainerPort, len(np))
	for i, p := range np {
		ports[i] = corev1.ContainerPort{Name: p.Name, ContainerPort: p.Port, Protocol: corev1.ProtocolTCP}
	}
	return ports
}

// ServicePorts returns the service port list for the headless Service.
func ServicePorts() []corev1.ServicePort {
	np := seiconfig.NodePorts()
	ports := make([]corev1.ServicePort, len(np))
	for i, p := range np {
		ports[i] = corev1.ServicePort{Name: p.Name, Port: p.Port, TargetPort: intstr.FromInt32(p.Port), Protocol: corev1.ProtocolTCP}
		if p.Name == "grpc" {
			h2c := "kubernetes.io/h2c"
			ports[i].AppProtocol = &h2c
		}
	}
	return ports
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func buildNodePodSpec(node *seiv1alpha1.SeiNode, p PlatformConfig) corev1.PodSpec {
	dataVolume := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: DataPVCName(node),
			},
		},
	}

	signingVolumes := signingKeyVolumes(node)
	nodeVolumes := nodeKeyVolumes(node)
	keyringVolumes := operatorKeyringVolumes(node)
	tlsVolumes := sidecarTLSVolumes(node)
	sidecarTmpVolume := corev1.Volume{
		Name:         sidecarTmpVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	volumes := make([]corev1.Volume, 0, 2+len(signingVolumes)+len(nodeVolumes)+len(keyringVolumes)+len(tlsVolumes))
	volumes = append(volumes, dataVolume, sidecarTmpVolume)
	volumes = append(volumes, signingVolumes...)
	volumes = append(volumes, nodeVolumes...)
	volumes = append(volumes, keyringVolumes...)
	volumes = append(volumes, tlsVolumes...)

	pool := p.NodepoolForMode(NodeMode(node))

	spec := corev1.PodSpec{
		// AutomountServiceAccountToken is explicit here because the future
		// kube-rbac-proxy fronting the sidecar API (sei-protocol/seictl#165)
		// calls TokenReview + SubjectAccessReview against the K8s API using
		// the pod's projected SA token. Cluster-default flips that silently
		// disable this would break the auth path.
		AutomountServiceAccountToken: ptr.To(true), //nolint:modernize // ptr.To(true) is idiomatic; new(true) is invalid Go
		ServiceAccountName:           p.ServiceAccount,
		Tolerations: []corev1.Toleration{
			{Key: p.TolerationKey, Value: pool, Effect: corev1.TaintEffectNoSchedule},
		},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "karpenter.sh/nodepool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{pool},
						}},
					}},
				},
			},
		},
		Volumes: volumes,
	}

	// ShareProcessNamespace is currently enabled across all SeiNode pods. A
	// compromised seid container can therefore read /proc/<sidecar>/environ
	// and /proc/<sidecar>/mem, including the operator-keyring passphrase and
	// the unlocked in-memory keyring. This is a known limitation of the v1
	// trust boundary — not a one-way door. The broader sidecar/seid isolation
	// hardening (drop shareProcessNamespace, harden the seid main container's
	// SecurityContext, separate the sidecar's SA) is tracked as a follow-up.
	// See PR #220 review thread.
	spec.ShareProcessNamespace = ptr.To(true)
	// FSGroup grants the non-root sidecar UID read access to 0o400
	// Secret-projected files. ChangePolicy=OnRootMismatch avoids recursive
	// chown on every pod start (the "Always" default is costly on archive PVCs).
	fsGroup := sidecarNonRootUID
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch
	spec.SecurityContext = &corev1.PodSecurityContext{
		FSGroup:             &fsGroup,
		FSGroupChangePolicy: &fsGroupChangePolicy,
	}
	initContainers := []corev1.Container{
		buildSeidInitContainer(node),
		buildSidecarContainer(node, p),
	}
	if SidecarTLSEnabled(node) {
		initContainers = append(initContainers, buildRBACProxyContainer(node, p))
	}
	spec.InitContainers = initContainers
	spec.Containers = []corev1.Container{buildSidecarMainContainer(node, p)}

	return spec
}

func sidecarImage(node *seiv1alpha1.SeiNode) string {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Image != "" {
		return node.Spec.Sidecar.Image
	}
	return defaultSidecarImage
}

// SidecarPort returns the sidecar HTTP port for the node.
func SidecarPort(node *seiv1alpha1.SeiNode) int32 {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Port != 0 {
		return node.Spec.Sidecar.Port
	}
	return seiconfig.PortSidecar
}

func buildSidecarContainer(node *seiv1alpha1.SeiNode, p PlatformConfig) corev1.Container {
	port := SidecarPort(node)
	keyringEnv := operatorKeyringEnvVars(node)
	env := make([]corev1.EnvVar, 0, 8+len(keyringEnv))
	env = append(env,
		corev1.EnvVar{Name: "SEI_CHAIN_ID", Value: node.Spec.ChainID},
		corev1.EnvVar{Name: "SEI_SIDECAR_PORT", Value: fmt.Sprintf("%d", port)},
		corev1.EnvVar{Name: "SEI_HOME", Value: dataDir},
		corev1.EnvVar{Name: "SEI_GENESIS_BUCKET", Value: p.GenesisBucket},
		corev1.EnvVar{Name: "SEI_GENESIS_REGION", Value: p.GenesisRegion},
		corev1.EnvVar{Name: "SEI_SNAPSHOT_BUCKET", Value: p.SnapshotBucket},
		corev1.EnvVar{Name: "SEI_SNAPSHOT_REGION", Value: p.SnapshotRegion},
	)
	if SidecarTLSEnabled(node) {
		// trusted-header flips the sidecar to loopback bind and turns on
		// the X-Remote-User check; kube-rbac-proxy is the only reachable
		// client to :7777 inside the pod's net ns.
		env = append(env, corev1.EnvVar{Name: "SEI_SIDECAR_AUTHN_MODE", Value: "trusted-header"})
	}
	env = append(env, keyringEnv...)

	keyringMounts := operatorKeyringMounts(node)
	mounts := make([]corev1.VolumeMount, 0, 2+len(keyringMounts))
	mounts = append(mounts,
		corev1.VolumeMount{Name: "data", MountPath: dataDir},
		corev1.VolumeMount{Name: sidecarTmpVolumeName, MountPath: "/tmp"},
	)
	mounts = append(mounts, keyringMounts...)

	c := corev1.Container{
		Name:          containerNameSidecar,
		Image:         sidecarImage(node),
		Command:       []string{"seictl", "serve"},
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Env:           env,
		Ports: []corev1.ContainerPort{
			{Name: "sidecar", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts:    mounts,
		SecurityContext: sidecarSecurityContext(),
		LivenessProbe:   sidecarLivenessProbe(node, port),
		ReadinessProbe:  sidecarReadinessProbe(node, port),
	}
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Resources != nil {
		c.Resources = *node.Spec.Sidecar.Resources
	}
	return c
}

func buildSidecarMainContainer(node *seiv1alpha1.SeiNode, p PlatformConfig) corev1.Container {
	container := buildNodeMainContainer(node)
	container.Command, container.Args = sidecarWaitCommand(node)
	container.Resources = DefaultResourcesForMode(NodeMode(node), p)
	// In TLS mode the sidecar binds loopback (unreachable from kubelet);
	// gate startup on the proxy's HTTPS endpoint instead — the proxy
	// proxies /v0/healthz to the sidecar (it's a bypass path) so the
	// signal is equivalent.
	startupProbeAction := corev1.HTTPGetAction{
		Path: pathHealthz,
		Port: intstr.FromInt32(SidecarPort(node)),
	}
	if SidecarTLSEnabled(node) {
		startupProbeAction = corev1.HTTPGetAction{
			Scheme: corev1.URISchemeHTTPS,
			Path:   pathHealthz,
			Port:   intstr.FromInt32(rbacProxyPort),
		}
	}
	container.StartupProbe = &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{HTTPGet: &startupProbeAction},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		FailureThreshold:    86400,
	}
	container.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/lag_status",
				Port: intstr.FromInt32(seiconfig.PortRPC),
			},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
		FailureThreshold:    3,
		TimeoutSeconds:      5,
	}
	return container
}

func sidecarWaitCommand(node *seiv1alpha1.SeiNode) (command []string, args []string) {
	cmd := "seid"
	cmdArgs := []string{"start", "--home", dataDir}
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
			`if (exec 3<>/dev/tcp/localhost/%d && printf "GET /v0/healthz HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3 && head -1 <&3 | grep -q "200") 2>/dev/null; then `+
			`break; `+
			`fi; `+
			`sleep 5; done; `+
			`echo "sidecar ready, starting seid"; `+
			`exec %s`,
		SidecarPort(node), b.String(),
	)

	return []string{"/bin/bash", "-c"}, []string{script}
}

func buildNodeMainContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	signingMounts := signingKeyMounts(node)
	nodeMounts := nodeKeyMounts(node)
	mounts := make([]corev1.VolumeMount, 0, 1+len(signingMounts)+len(nodeMounts))
	mounts = append(mounts, corev1.VolumeMount{Name: "data", MountPath: dataDir})
	mounts = append(mounts, signingMounts...)
	mounts = append(mounts, nodeMounts...)
	container := corev1.Container{
		Name:  containerNameSeid,
		Image: node.Spec.Image,
		Env: []corev1.EnvVar{
			{Name: "TMPDIR", Value: dataDir + "/tmp"},
		},
		VolumeMounts: mounts,
		Ports:        ContainerPorts(),
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(seiconfig.PortRPC),
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

	if NeedsLongStartup(node) {
		container.StartupProbe.FailureThreshold = 1800
	}

	return container
}

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

func makeResources(cpu, memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
	}
}

// signingKeyVolumes is called only from the production StatefulSet pod-spec.
// The bootstrap Job pod-spec must never include these volumes — see the
// safety invariant on task.GenerateBootstrapJob.
func signingKeyVolumes(node *seiv1alpha1.SeiNode) []corev1.Volume {
	src := signingKeySecretSource(node)
	if src == nil {
		return nil
	}
	return []corev1.Volume{{
		Name: signingKeyVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  src.SecretName,
				DefaultMode: ptr.To[int32](0o400),
				Items: []corev1.KeyToPath{
					{Key: privValidatorKeyDataKey, Path: privValidatorKeyDataKey},
				},
			},
		},
	}}
}

// signingKeyMounts uses subPath deliberately: kubelet does not auto-refresh
// subPath mounts, so a Secret edit cannot hot-swap the consensus key under
// a running seid — which would risk signing two different blocks at the
// same height. Rotating the key requires a deliberate pod restart paired
// with on-chain MsgEditValidator.
func signingKeyMounts(node *seiv1alpha1.SeiNode) []corev1.VolumeMount {
	if signingKeySecretSource(node) == nil {
		return nil
	}
	return []corev1.VolumeMount{{
		Name:      signingKeyVolumeName,
		MountPath: dataDir + "/config/" + privValidatorKeyDataKey,
		SubPath:   privValidatorKeyDataKey,
		ReadOnly:  true,
	}}
}

func signingKeySecretSource(node *seiv1alpha1.SeiNode) *seiv1alpha1.SecretSigningKeySource {
	if node.Spec.Validator == nil || node.Spec.Validator.SigningKey == nil {
		return nil
	}
	return node.Spec.Validator.SigningKey.Secret
}

// nodeKeyVolumes mounts the node-key Secret on the production pod only.
// The bootstrap Job uses an ephemeral node ID generated by `seid init`.
func nodeKeyVolumes(node *seiv1alpha1.SeiNode) []corev1.Volume {
	src := nodeKeySecretSource(node)
	if src == nil {
		return nil
	}
	return []corev1.Volume{{
		Name: nodeKeyVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  src.SecretName,
				DefaultMode: ptr.To[int32](0o400),
				Items: []corev1.KeyToPath{
					{Key: nodeKeyDataKey, Path: nodeKeyDataKey},
				},
			},
		},
	}}
}

// nodeKeyMounts uses subPath for symmetry with signingKeyMounts. Kubelet
// pins subPath mounts at pod start, so a Secret edit cannot hot-swap the
// node ID under a running seid — which would force a peer-graph reset.
func nodeKeyMounts(node *seiv1alpha1.SeiNode) []corev1.VolumeMount {
	if nodeKeySecretSource(node) == nil {
		return nil
	}
	return []corev1.VolumeMount{{
		Name:      nodeKeyVolumeName,
		MountPath: dataDir + "/config/" + nodeKeyDataKey,
		SubPath:   nodeKeyDataKey,
		ReadOnly:  true,
	}}
}

func nodeKeySecretSource(node *seiv1alpha1.SeiNode) *seiv1alpha1.SecretNodeKeySource {
	if node.Spec.Validator == nil || node.Spec.Validator.NodeKey == nil {
		return nil
	}
	return node.Spec.Validator.NodeKey.Secret
}

// operatorKeyringVolumes projects the operator-keyring Secret as a directory
// under $SEI_HOME/keyring-file/ — the Cosmos SDK file-backend layout.
// Mounted on the sidecar container only; the seid main and bootstrap pods
// never see this material.
func operatorKeyringVolumes(node *seiv1alpha1.SeiNode) []corev1.Volume {
	src := operatorKeyringSecretSource(node)
	if src == nil {
		return nil
	}
	return []corev1.Volume{{
		Name: operatorKeyringVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  src.SecretName,
				DefaultMode: ptr.To[int32](0o400),
			},
		},
	}}
}

func operatorKeyringMounts(node *seiv1alpha1.SeiNode) []corev1.VolumeMount {
	if operatorKeyringSecretSource(node) == nil {
		return nil
	}
	return []corev1.VolumeMount{{
		Name:      operatorKeyringVolumeName,
		MountPath: dataDir + "/" + operatorKeyringDirName,
		ReadOnly:  true,
	}}
}

// operatorKeyringEnvVars injects the keyring unlock passphrase into the
// sidecar process via a separate Secret reference. The passphrase lives in
// its own Secret because the keyring data Secret is projected as a
// directory — co-locating the passphrase as a data key would land it as a
// file inside the keyring directory.
func operatorKeyringEnvVars(node *seiv1alpha1.SeiNode) []corev1.EnvVar {
	src := operatorKeyringSecretSource(node)
	if src == nil {
		return nil
	}
	return []corev1.EnvVar{{
		Name: keyringPassphraseEnvVar,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: src.PassphraseSecretRef.SecretName},
				Key:                  src.PassphraseSecretRef.Key,
			},
		},
	}}
}

func operatorKeyringSecretSource(node *seiv1alpha1.SeiNode) *seiv1alpha1.SecretOperatorKeyringSource {
	if node.Spec.Validator == nil || node.Spec.Validator.OperatorKeyring == nil {
		return nil
	}
	return node.Spec.Validator.OperatorKeyring.Secret
}

// sidecarSecurityContext locks the sidecar to non-root, read-only rootfs,
// no privilege escalation, all caps dropped, and the runtime's default
// seccomp profile. Scope is deliberately the sidecar only — applying the
// same to the seid main container is a larger blast-radius change owned
// by a different workstream.
func sidecarSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr.To(true),              //nolint:modernize // ptr.To(true) is idiomatic; new(true) is invalid Go
		RunAsUser:                ptr.To(sidecarNonRootUID), //nolint:modernize // false-positive: new() takes a type, not a value
		RunAsGroup:               ptr.To(sidecarNonRootUID), //nolint:modernize // false-positive: new() takes a type, not a value
		AllowPrivilegeEscalation: ptr.To(false),             //nolint:modernize // ptr.To(false) is idiomatic; new(false) is invalid Go
		ReadOnlyRootFilesystem:   ptr.To(true),              //nolint:modernize // ptr.To(true) is idiomatic; new(true) is invalid Go
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// SidecarTLSEnabled reports whether the SeiNode opts into kube-rbac-proxy
// fronting via spec.sidecar.tls.
func SidecarTLSEnabled(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.Sidecar != nil && node.Spec.Sidecar.TLS != nil
}

// SidecarTLSSecretName is the cert-manager-managed Secret carrying the
// proxy's TLS material. Exported because plan tasks reference it when
// emitting the Certificate.
func SidecarTLSSecretName(node *seiv1alpha1.SeiNode) string {
	return node.Name + "-sidecar-tls"
}

// RBACProxyConfigMapName is the ConfigMap carrying the proxy's
// resourceAttributes + allow-paths config. Exported for the same reason.
func RBACProxyConfigMapName(node *seiv1alpha1.SeiNode) string {
	return node.Name + "-rbac-proxy-config"
}

func sidecarTLSVolumes(node *seiv1alpha1.SeiNode) []corev1.Volume {
	if !SidecarTLSEnabled(node) {
		return nil
	}
	return []corev1.Volume{
		{
			Name: rbacProxyConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: RBACProxyConfigMapName(node)},
				},
			},
		},
		{
			Name: sidecarTLSVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  SidecarTLSSecretName(node),
					DefaultMode: ptr.To[int32](0o400),
				},
			},
		},
	}
}

// In TLS mode the sidecar binds loopback and is unreachable from
// kubelet; probes move to the proxy container instead. Pod readiness
// still gates on all containers, so a wedged sidecar fails the proxy's
// probe (the upstream call returns 5xx) and the pod stays NotReady.

func sidecarLivenessProbe(node *seiv1alpha1.SeiNode, sidecarPort int32) *corev1.Probe {
	if SidecarTLSEnabled(node) {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: pathLivez, Port: intstr.FromInt32(sidecarPort)},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

func sidecarReadinessProbe(node *seiv1alpha1.SeiNode, sidecarPort int32) *corev1.Probe {
	if SidecarTLSEnabled(node) {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: pathHealthz, Port: intstr.FromInt32(sidecarPort)},
		},
		InitialDelaySeconds: 2,
		PeriodSeconds:       5,
		FailureThreshold:    6,
	}
}

// buildRBACProxyContainer is the kube-rbac-proxy native sidecar that
// terminates TLS on :8443 and forwards authn-passed requests to the
// loopback-bound seictl sidecar.
//
// --ignore-paths (CLI flag, not config-file) bypasses BOTH authn and
// authz for the listed paths. The config-file's allowedPaths gates AT
// the authz layer, which still requires authentication first; kubelet
// probes carry no bearer token so they would 401 there.
//
// Probes live on this container, not the seictl sidecar: kubelet
// failures restart the unhealthy container, and the proxy is the
// externally-reachable endpoint.
func buildRBACProxyContainer(node *seiv1alpha1.SeiNode, _ PlatformConfig) corev1.Container {
	ignorePaths := strings.Join(bypassPaths(), ",")
	return corev1.Container{
		Name:          containerNameRBACProxy,
		Image:         defaultRBACProxyImage,
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Args: []string{
			fmt.Sprintf("--secure-listen-address=0.0.0.0:%d", rbacProxyPort),
			fmt.Sprintf("--upstream=http://127.0.0.1:%d/", SidecarPort(node)),
			"--config-file=" + rbacProxyConfigMountPath + "/config.yaml",
			"--tls-cert-file=" + sidecarTLSMountPath + "/tls.crt",
			"--tls-private-key-file=" + sidecarTLSMountPath + "/tls.key",
			// Reload TLS material from disk every 30s so cert-manager
			// renewals propagate without a pod restart.
			"--tls-reload-interval=30s",
			"--ignore-paths=" + ignorePaths,
			"--v=2",
		},
		Ports: []corev1.ContainerPort{
			{Name: servicePortNameAPI, ContainerPort: rbacProxyPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: rbacProxyConfigVolumeName, MountPath: rbacProxyConfigMountPath, ReadOnly: true},
			{Name: sidecarTLSVolumeName, MountPath: sidecarTLSMountPath, ReadOnly: true},
		},
		StartupProbe:    proxyStartupProbe(),
		LivenessProbe:   proxyLivenessProbe(),
		ReadinessProbe:  proxyReadinessProbe(),
		SecurityContext: sidecarSecurityContext(),
	}
}

// bypassPaths is the authoritative list of paths the proxy lets through
// without authn/authz. Must match the sidecar's middleware bypass list
// (seictl sidecar/server/auth.go BypassPaths).
func bypassPaths() []string {
	return []string{pathHealthz, pathStartupz, pathLivez, pathMetrics}
}

func proxyStartupProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(rbacProxyPort)},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		FailureThreshold:    60, // ~5min for first-cert issuance
	}
}

func proxyLivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Scheme: corev1.URISchemeHTTPS,
				Path:   pathLivez,
				Port:   intstr.FromInt32(rbacProxyPort),
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

func proxyReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Scheme: corev1.URISchemeHTTPS,
				Path:   pathHealthz,
				Port:   intstr.FromInt32(rbacProxyPort),
			},
		},
		InitialDelaySeconds: 2,
		PeriodSeconds:       5,
		FailureThreshold:    6,
	}
}

// GenerateSidecarCertificate returns an unstructured Certificate so
// the controller avoids depending on cert-manager Go types — the
// three fields we need (apiVersion, kind, spec) are stable.
func GenerateSidecarCertificate(node *seiv1alpha1.SeiNode) *unstructured.Unstructured {
	if !SidecarTLSEnabled(node) {
		return nil
	}
	tls := node.Spec.Sidecar.TLS
	svcDNS := fmt.Sprintf("%s.%s.svc.cluster.local", node.Name, node.Namespace)
	pod0DNS := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", node.Name, node.Name, node.Namespace)

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("cert-manager.io/v1")
	obj.SetKind("Certificate")
	obj.SetName(SidecarTLSSecretName(node))
	obj.SetNamespace(node.Namespace)
	obj.SetLabels(ResourceLabels(node))
	_ = unstructured.SetNestedMap(obj.Object, map[string]any{
		"secretName":  SidecarTLSSecretName(node),
		"duration":    "2160h", // 90 days
		"renewBefore": "360h",  // 15 days
		"commonName":  svcDNS,
		"dnsNames":    []any{svcDNS, pod0DNS},
		"issuerRef": map[string]any{
			"name":  tls.IssuerRef.Name,
			"kind":  tls.IssuerRef.Kind,
			"group": tls.IssuerRef.Group,
		},
	}, "spec")
	return obj
}

// GenerateRBACProxyConfigMap produces the ConfigMap carrying the
// kube-rbac-proxy authorization config — a single coarse SAR scoped
// to (group=sei.io, resource=seinodetasks, namespace=<ns>, name=<node>).
// Verb is derived from HTTP method by the proxy. Bypass paths live on
// the proxy's --ignore-paths CLI flag, not this file (config-file
// allowedPaths gates at the authz layer and still requires authn).
//
// `name` scopes the SAR to the specific SeiNode so operators can bind
// ClusterRoles with resourceNames to narrow access per-validator;
// empty resourceNames still matches.
func GenerateRBACProxyConfigMap(node *seiv1alpha1.SeiNode) *corev1.ConfigMap {
	if !SidecarTLSEnabled(node) {
		return nil
	}
	config := strings.Join([]string{
		"authorization:",
		"  resourceAttributes:",
		"    group: sei.io",
		"    resource: seinodetasks",
		"    namespace: " + node.Namespace,
		"    name: " + node.Name,
		"",
	}, "\n")
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RBACProxyConfigMapName(node),
			Namespace: node.Namespace,
			Labels:    ResourceLabels(node),
		},
		Data: map[string]string{"config.yaml": config},
	}
}
