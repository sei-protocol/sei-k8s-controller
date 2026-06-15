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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

const (
	// NodeLabel is the standard label key used on all SeiNode-owned resources.
	NodeLabel = "sei.io/node"

	// chainLabel and roleLabel are observability identity labels lifted
	// into `chain_id` and `component` metric labels by platform-owned
	// (Pod|Service)Monitor relabelings. Pod-template only, never in the
	// StatefulSet selector.
	chainLabel = "sei.io/chain"
	roleLabel  = "sei.io/role"

	roleValidator = "validator"
	roleArchive   = "archive"
	roleReplayer  = "replayer"
	roleFullNode  = "node"

	dataDir = platform.DataDir

	// homeVarRef is the K8s VariableReference form of HOME, substituted from
	// container.Env at pod-create. Spelled `$(HOME)` (not `$HOME`) — only the
	// K8s syntax is expanded by kubelet; shell-style refs are passed through
	// literally and fail at exec.
	homeVarRef = "$(HOME)"

	// seidHomeFlag is the cosmos-sdk flag that sets seid's working dir.
	seidHomeFlag = "--home"

	// seidStartSubcommand is the seid subcommand that boots the node.
	seidStartSubcommand = "start"

	shellBash = "/bin/bash"

	// Pod-spec container names. Used as both the .Name on built containers
	// and the lookup key for the operator-keyring containment guard.
	containerNameSeid                 = "seid"
	containerNameSidecar              = "sei-sidecar"
	containerNameRBACProxy            = "kube-rbac-proxy"
	containerNameCosmosExporter       = "cosmos-exporter"
	servicePortNameAPI                = "api"
	rbacProxyConfigVolumeName         = "rbac-proxy-config"
	rbacProxyConfigMountPath          = "/etc/kube-rbac-proxy"
	RBACProxyPort               int32 = 8443

	pathHealthz  = "/v0/healthz"
	pathLivez    = "/v0/livez"
	pathStartupz = "/v0/startupz"
	pathMetrics  = "/v0/metrics"

	signingKeyVolumeName    = "signing-key"
	privValidatorKeyDataKey = "priv_validator_key.json"

	nodeKeyVolumeName = "node-key"
	nodeKeyDataKey    = "node_key.json"

	operatorKeyringVolumeName = "operator-keyring"
	// keyring.New(BackendFile, rootDir) opens rootDir/keyring-file/.
	// Used as the mount path for the projected .secret Secret.
	operatorKeyringDirName = "keyring-file"

	keyringPassphraseEnvVar = "SEI_KEYRING_PASSPHRASE"
	// Required — the SDK's "os" compile-time default has no distroless analogue.
	keyringBackendEnvVar = "SEI_KEYRING_BACKEND"
	// SDK appends "keyring-<backend>"; passing $SEI_HOME resolves to
	// $SEI_HOME/keyring-file/ or $SEI_HOME/keyring-test/.
	keyringDirEnvVar   = "SEI_KEYRING_DIR"
	keyringBackendFile = "file"
	keyringBackendTest = "test"

	// sidecarTmpVolumeName backs an emptyDir at /tmp — required because the
	// sidecar runs with ReadOnlyRootFilesystem and Go stdlib defaults to /tmp.
	sidecarTmpVolumeName = "sidecar-tmp"
	sidecarTmpMountPath  = "/tmp"

	// sidecarNonRootUID is the nonroot UID/GID baked into distroless and
	// chainguard static-debian12 base images. Pod-level fsGroup matches so
	// the non-root sidecar can read kubelet-projected 0o400 Secret files.
	sidecarNonRootUID int64 = 65532

	// defaultCosmosExporterPort matches sei-cosmos-exporter's upstream
	// default. Platform PodMonitors target the named port `cosmos-metrics`.
	defaultCosmosExporterPort int32 = 9300
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
// User-provided podLabels are applied first; system labels win.
func ResourceLabels(node *seiv1alpha1.SeiNode) map[string]string {
	labels := make(map[string]string, len(node.Spec.PodLabels)+3)
	maps.Copy(labels, node.Spec.PodLabels)
	labels[NodeLabel] = node.Name
	if node.Spec.ChainID != "" {
		labels[chainLabel] = node.Spec.ChainID
	}
	labels[roleLabel] = deriveRole(node)
	return labels
}

// deriveRole returns the role label value for the node's mode. Stamped
// onto the pod template as `sei.io/role` and lifted into the `sei_role`
// metric label by the platform PodMonitor (see
// platform/clusters/*/monitoring/podmonitor-seid.yaml). Total — a node with
// no mode sub-spec is a full node — so `sei.io/role` is never empty and the
// metric series never drops.
func deriveRole(node *seiv1alpha1.SeiNode) string {
	switch {
	case node.Spec.Validator != nil:
		return roleValidator
	case node.Spec.Archive != nil:
		return roleArchive
	case node.Spec.Replayer != nil:
		return roleReplayer
	default:
		return roleFullNode
	}
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
	if p.KubeRBACProxyImage == "" {
		return nil, fmt.Errorf("images.kubeRBACProxy is not configured in the app-config file")
	}
	one := int32(1)
	labels := ResourceLabels(node)
	podSpec, err := buildNodePodSpec(node, p)
	if err != nil {
		return nil, err
	}

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
// Scoped to the .secret path: the test-backend keyring shares the data PVC
// seid already owns, so the guard has nothing to assert. Operators who need
// keyring/seid isolation set .secret, which engages this check.
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
	ports := append(ServicePorts(), corev1.ServicePort{
		Name:       servicePortNameAPI,
		Port:       RBACProxyPort,
		TargetPort: intstr.FromInt32(RBACProxyPort),
		Protocol:   corev1.ProtocolTCP,
	})
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

func buildNodePodSpec(node *seiv1alpha1.SeiNode, p PlatformConfig) (corev1.PodSpec, error) {
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
	proxyConfigVolume := corev1.Volume{
		Name: rbacProxyConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: RBACProxyConfigMapName(node)},
			},
		},
	}
	sidecarTmpVolume := corev1.Volume{
		Name:         sidecarTmpVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	volumes := make([]corev1.Volume, 0, 3+len(signingVolumes)+len(nodeVolumes)+len(keyringVolumes))
	volumes = append(volumes, dataVolume, sidecarTmpVolume, proxyConfigVolume)
	volumes = append(volumes, signingVolumes...)
	volumes = append(volumes, nodeVolumes...)
	volumes = append(volumes, keyringVolumes...)

	pool := p.NodepoolForMode(NodeMode(node))

	spec := corev1.PodSpec{
		// AutomountServiceAccountToken is explicit here because the
		// kube-rbac-proxy fronting the sidecar API calls TokenReview +
		// SubjectAccessReview against the K8s API using the pod's
		// projected SA token. Cluster-default flips that silently
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
	// trust boundary; tightening it (drop shareProcessNamespace, harden the
	// seid SecurityContext, separate sidecar SA) is a separate workstream.
	spec.ShareProcessNamespace = ptr.To(true)
	// Kubelet handles PVC ownership migration: on first mount of a legacy
	// root-owned volume, OnRootMismatch triggers a recursive `chown :65532`
	// + `chmod g+rwX`. Subsequent mounts skip the walk.
	fsGroup := sidecarNonRootUID
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch
	spec.SecurityContext = &corev1.PodSecurityContext{
		FSGroup:             &fsGroup,
		FSGroupChangePolicy: &fsGroupChangePolicy,
	}
	spec.InitContainers = []corev1.Container{
		buildSeidInitContainer(node),
		buildSidecarContainer(node, p),
		buildRBACProxyContainer(node, p),
	}
	ceContainer, err := buildCosmosExporterContainer(p)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	spec.Containers = []corev1.Container{
		buildSidecarMainContainer(node, p),
		ceContainer,
	}

	return spec, nil
}

func sidecarImage(node *seiv1alpha1.SeiNode, p PlatformConfig) string {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Image != "" {
		return node.Spec.Sidecar.Image
	}
	return p.SidecarImage
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
	// Sidecar binds loopback and trusts the X-Remote-User header from
	// kube-rbac-proxy. The proxy is the only client reachable on the
	// sidecar's port inside the pod's net ns.
	env = append(env, corev1.EnvVar{Name: "SEI_SIDECAR_AUTHN_MODE", Value: "trusted-header"})
	env = append(env, keyringEnv...)

	keyringMounts := operatorKeyringMounts(node)
	mounts := make([]corev1.VolumeMount, 0, 2+len(keyringMounts))
	mounts = append(mounts,
		// Mounted RW so generate-gentx can write the operator key the sidecar later reads.
		corev1.VolumeMount{Name: "data", MountPath: dataDir},
		corev1.VolumeMount{Name: sidecarTmpVolumeName, MountPath: sidecarTmpMountPath},
	)
	mounts = append(mounts, keyringMounts...)

	c := corev1.Container{
		Name:          containerNameSidecar,
		Image:         sidecarImage(node, p),
		Command:       []string{"seictl", "serve"},
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Env:           env,
		Ports: []corev1.ContainerPort{
			{Name: "sidecar", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts:    mounts,
		SecurityContext: sidecarSecurityContext(),
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
	// Sidecar binds loopback; gate startup on the proxy's /v0/healthz
	// (a bypass path that forwards through to the sidecar).
	container.StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: pathHealthz,
			Port: intstr.FromInt32(RBACProxyPort),
		}},
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

// defaultCosmosExporterResources: no CPU limit — cosmos-exporter calls
// seid's gRPC on every scrape; throttling turns into visible scrape gaps.
func defaultCosmosExporterResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("384Mi"),
		},
	}
}

// cosmosExporterWaitCommand renders bash that waits for seid's gRPC port,
// then exec's cosmos-exporter. Container.Command overrides ENTRYPOINT, so
// the binary path is spelled out.
func cosmosExporterWaitCommand() (command []string, args []string) {
	exporter := "/usr/local/bin/sei-cosmos-exporter"
	exporterArgs := []string{
		"--denom", "usei",
		"--denom-coefficient", "1000000",
		"--bech-prefix", "sei",
		"--listen-address", fmt.Sprintf(":%d", defaultCosmosExporterPort),
	}

	var b strings.Builder
	b.WriteString(exporter)
	for _, a := range exporterArgs {
		fmt.Fprintf(&b, " %q", a)
	}

	script := fmt.Sprintf(
		`echo "waiting for gRPC :%d to be available..."; `+
			`until (exec 3<>/dev/tcp/localhost/%d) 2>/dev/null; do `+
			`sleep 5; done; `+
			`echo "gRPC available, starting cosmos-exporter"; `+
			`exec %s`,
		seiconfig.PortGRPC, seiconfig.PortGRPC, b.String(),
	)

	return []string{shellBash, "-c"}, []string{script}
}

// buildCosmosExporterContainer renders the cosmos-exporter sidecar.
func buildCosmosExporterContainer(p PlatformConfig) (corev1.Container, error) {
	if p.CosmosExporterImage == "" {
		return corev1.Container{}, fmt.Errorf("images.cosmosExporter is required in the app-config file")
	}
	command, args := cosmosExporterWaitCommand()
	return corev1.Container{
		Name:    containerNameCosmosExporter,
		Image:   p.CosmosExporterImage,
		Command: command,
		Args:    args,
		Ports: []corev1.ContainerPort{
			{Name: "cosmos-metrics", ContainerPort: defaultCosmosExporterPort, Protocol: corev1.ProtocolTCP},
		},
		SecurityContext: sidecarSecurityContext(),
		Resources:       defaultCosmosExporterResources(),
		// /tmp: ReadOnlyRootFilesystem EROFS insurance.
		VolumeMounts: []corev1.VolumeMount{
			{Name: sidecarTmpVolumeName, MountPath: sidecarTmpMountPath},
		},
		// Generous failureThreshold so Prometheus targets don't flap while
		// the wait loop holds :9300 off during cold start.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(defaultCosmosExporterPort),
				},
			},
			PeriodSeconds:    10,
			FailureThreshold: 6,
		},
		// InitialDelaySeconds: 600 gives the wait loop room before kubelet probes.
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(defaultCosmosExporterPort),
				},
			},
			InitialDelaySeconds: 600,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
	}, nil
}

func sidecarWaitCommand(node *seiv1alpha1.SeiNode) (command []string, args []string) {
	// Canonical seid invocation. "$HOME" (shell-expanded inside bash -c)
	// resolves from the container env declared in buildNodeMainContainer.
	cmd := "seid"
	cmdArgs := []string{seidStartSubcommand, seidHomeFlag, "$HOME"}

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

	return []string{shellBash, "-c"}, []string{script}
}

func buildNodeMainContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	signingMounts := signingKeyMounts(node)
	nodeMounts := nodeKeyMounts(node)
	mounts := make([]corev1.VolumeMount, 0, 1+len(signingMounts)+len(nodeMounts))
	mounts = append(mounts, corev1.VolumeMount{Name: "data", MountPath: dataDir})
	mounts = append(mounts, signingMounts...)
	mounts = append(mounts, nodeMounts...)
	// HOME must appear before TMPDIR so K8s VariableReference $(HOME) in
	// args[] resolves at pod-create. K8s reads container.Env (not the
	// process env) for variable substitution, ordered top-down.
	container := corev1.Container{
		Name:            containerNameSeid,
		Image:           node.Spec.Image,
		SecurityContext: seidNonRootSecurityContext(),
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: dataDir},
			{Name: "TMPDIR", Value: dataDir + "/tmp"},
		},
		// Canonical invocation. $(HOME) is K8s VariableReference syntax,
		// substituted from container.Env before exec.
		Command:      []string{"seid"},
		Args:         []string{seidStartSubcommand, seidHomeFlag, homeVarRef},
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

	if NeedsLongStartup(node) {
		container.StartupProbe.FailureThreshold = 1800
	}

	return container
}

// buildSeidInitContainer materializes $HOME/config on first boot via
// `seid init` and ensures $HOME/tmp exists. The script uses shell
// $HOME (the script runs via /bin/sh -c) so the path stays in lock-step
// with the HOME env var.
func buildSeidInitContainer(node *seiv1alpha1.SeiNode) corev1.Container {
	script := fmt.Sprintf(
		`if [ -f "$HOME/config/genesis.json" ]; then echo "data directory already initialized, skipping seid init"; else seid init %s --chain-id %s --home "$HOME" --overwrite; fi && mkdir -p "$HOME/tmp"`,
		node.Spec.ChainID, node.Spec.ChainID,
	)
	return corev1.Container{
		Name:  "seid-init",
		Image: node.Spec.Image,
		Command: []string{
			"/bin/sh", "-c", script,
		},
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: dataDir},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataDir},
		},
		SecurityContext: seidNonRootSecurityContext(),
	}
}

// seidNonRootSecurityContext is shared by seid main and seid-init (same
// binary, same mount, same privilege floor). PSA `restricted`-eligible.
// ReadOnlyRootFilesystem omitted: seid's Go runtime may use /tmp on the
// rootfs and no emptyDir is wired for it on seid containers.
func seidNonRootSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr.To(true),              //nolint:modernize // ptr.To(true) is idiomatic; new(true) is invalid Go
		RunAsUser:                ptr.To(sidecarNonRootUID), //nolint:modernize // ptr.To(65532) is idiomatic; new(int64) would zero
		RunAsGroup:               ptr.To(sidecarNonRootUID), //nolint:modernize // ptr.To(65532) is idiomatic; new(int64) would zero
		AllowPrivilegeEscalation: ptr.To(false),             //nolint:modernize // ptr.To(false) is idiomatic; new(false) is invalid Go
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
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

// operatorKeyringEnvVars configures the sidecar's keyring. Branches:
//
//   - Non-validator: no env vars.
//   - Validator with .secret: file backend; passphrase from the referenced
//     Secret; keyring projected at $SEI_HOME/keyring-file/ by
//     operatorKeyringVolumes.
//   - Validator without .secret: test backend on the data PVC at
//     $SEI_HOME/keyring-test/ — where generate-gentx writes the validator
//     key during a genesis ceremony. Unencrypted.
//
// SEI_KEYRING_DIR is $SEI_HOME on both validator branches; the SDK appends
// "keyring-<backend>" itself.
//
// The passphrase lives in its own Secret because the keyring data Secret is
// projected as a directory — embedding the passphrase would land it inside
// that directory, and the file backend would read it as keyring contents.
func operatorKeyringEnvVars(node *seiv1alpha1.SeiNode) []corev1.EnvVar {
	if node.Spec.Validator == nil {
		return nil
	}
	src := operatorKeyringSecretSource(node)
	if src == nil {
		return []corev1.EnvVar{
			{Name: keyringBackendEnvVar, Value: keyringBackendTest},
			{Name: keyringDirEnvVar, Value: dataDir},
		}
	}
	return []corev1.EnvVar{
		{Name: keyringBackendEnvVar, Value: keyringBackendFile},
		{Name: keyringDirEnvVar, Value: dataDir},
		{
			Name: keyringPassphraseEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: src.PassphraseSecretRef.SecretName},
					Key:                  src.PassphraseSecretRef.Key,
				},
			},
		},
	}
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

// RBACProxyConfigMapName returns the name of the ConfigMap carrying the
// kube-rbac-proxy authorization config for a given SeiNode.
func RBACProxyConfigMapName(node *seiv1alpha1.SeiNode) string {
	return node.Name + "-rbac-proxy-config"
}

// buildRBACProxyContainer fronts the loopback-bound sidecar with
// kube-rbac-proxy on plaintext :8443. The proxy performs TokenReview +
// SubjectAccessReview against the K8s API and forwards passing requests
// upstream; the private cluster network provides confidentiality.
//
// --ignore-paths bypasses authn AND authz for healthz/metrics so
// kubelet probes don't need a bearer token.
func buildRBACProxyContainer(node *seiv1alpha1.SeiNode, p PlatformConfig) corev1.Container {
	return corev1.Container{
		Name:          containerNameRBACProxy,
		Image:         p.KubeRBACProxyImage,
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Args: []string{
			fmt.Sprintf("--insecure-listen-address=0.0.0.0:%d", RBACProxyPort),
			fmt.Sprintf("--upstream=http://127.0.0.1:%d/", SidecarPort(node)),
			"--config-file=" + rbacProxyConfigMountPath + "/config.yaml",
			"--ignore-paths=" + strings.Join(bypassPaths(), ","),
			"--auth-header-fields-enabled=true",
			"--v=0",
		},
		Ports: []corev1.ContainerPort{
			{Name: servicePortNameAPI, ContainerPort: RBACProxyPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: rbacProxyConfigVolumeName, MountPath: rbacProxyConfigMountPath, ReadOnly: true},
		},
		StartupProbe:    proxyStartupProbe(),
		LivenessProbe:   proxyLivenessProbe(),
		ReadinessProbe:  proxyReadinessProbe(),
		SecurityContext: sidecarSecurityContext(),
	}
}

// bypassPaths are skipped by kube-rbac-proxy authn AND authz. Must
// match the sidecar's middleware bypass list (sidecar/server/auth.go).
// SECURITY: only public health/metrics paths — never state, keyring,
// or operator actions.
func bypassPaths() []string {
	return []string{pathHealthz, pathStartupz, pathLivez, pathMetrics}
}

func proxyStartupProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(RBACProxyPort)},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		FailureThreshold:    60,
	}
}

func proxyLivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: pathLivez,
			Port: intstr.FromInt32(RBACProxyPort),
		}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

// proxyReadinessProbe gates pod readiness on the sidecar via the
// proxy: --ignore-paths skips authn, but the proxy still forwards
// upstream, so a wedged sidecar surfaces as 5xx here.
func proxyReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: pathHealthz,
			Port: intstr.FromInt32(RBACProxyPort),
		}},
		InitialDelaySeconds: 2,
		PeriodSeconds:       5,
		FailureThreshold:    6,
	}
}

// GenerateRBACProxyConfigMap produces the ConfigMap carrying the
// kube-rbac-proxy authorization config — a single coarse SAR scoped
// to (apiGroup=sei.io, resource=seinodetasks, namespace=<ns>, name=<node>).
// Verb is derived from HTTP method by the proxy. The field name is
// `apiGroup`, matching kube-rbac-proxy's authz.ResourceAttributes
// struct (pkg/authz/auth.go).
//
// `name` scopes the SAR to the specific SeiNode so operators can bind
// ClusterRoles with resourceNames to narrow access per-validator;
// empty resourceNames still matches.
func GenerateRBACProxyConfigMap(node *seiv1alpha1.SeiNode) *corev1.ConfigMap {
	config := strings.Join([]string{
		"authorization:",
		"  resourceAttributes:",
		"    apiGroup: sei.io",
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

// SidecarURLForNode returns the in-cluster URL for a node's sidecar.
// All traffic flows through kube-rbac-proxy on plaintext :8443; the
// SAR authz check is the trust boundary, the private cluster network
// provides confidentiality.
func SidecarURLForNode(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("http://%s-0.%s.%s.svc.cluster.local:%d",
		node.Name, node.Name, node.Namespace, RBACProxyPort)
}
