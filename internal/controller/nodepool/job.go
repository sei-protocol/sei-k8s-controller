package nodepool

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func generateDataPVC(sn *seiv1alpha1.SeiNodePool, ordinal int) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataPVCName(sn, ordinal),
			Namespace: sn.Namespace,
			Labels:    resourceLabels(sn),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(defaultStorageSize),
				},
			},
		},
	}

	if defaultStorageClass != "" {
		sc := defaultStorageClass
		pvc.Spec.StorageClassName = &sc
	}

	return pvc
}

func generateGenesisPVC(sn *seiv1alpha1.SeiNodePool) *corev1.PersistentVolumeClaim {
	sc := efsStorageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      genesisPVCName(sn),
			Namespace: sn.Namespace,
			Labels:    resourceLabels(sn),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(defaultStorageSize),
				},
			},
		},
	}
}

func generateGenesisJob(sn *seiv1alpha1.SeiNodePool) *batchv1.Job {
	labels := resourceLabels(sn)
	backoffLimit := int32(3)
	defaultMode := int32(0o755)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-genesis", sn.Name),
			Namespace: sn.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "genesis",
							Image:   sn.Spec.NodeConfiguration.Image,
							Command: []string{"sh", "/scripts/generate.sh"},
							Env: []corev1.EnvVar{
								{Name: "NUM_NODES", Value: fmt.Sprintf("%d", sn.Spec.NodeConfiguration.NodeCount)},
								{Name: "CHAIN_ID", Value: sn.Spec.ChainID},
								{Name: "NODES_DIR", Value: genesisNodesDir(sn)},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "genesis-data", MountPath: genesisDir},
								{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "genesis-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: genesisPVCName(sn),
								},
							},
						},
						{
							Name: "scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: genesisScriptConfigMapName(sn),
									},
									DefaultMode: &defaultMode,
								},
							},
						},
					},
				},
			},
		},
	}
}

func generatePrepJob(sn *seiv1alpha1.SeiNodePool, ordinal int) *batchv1.Job {
	labels := resourceLabels(sn)
	backoffLimit := int32(3)

	mainContainer := corev1.Container{
		Name:    "genesis-copy",
		Image:   sn.Spec.NodeConfiguration.Image,
		Command: []string{"sh", "-c", prepNodeScript},
		Env: []corev1.EnvVar{
			{Name: "NODE_INDEX", Value: fmt.Sprintf("%d", ordinal)},
			{Name: "DATA_DIR", Value: dataDir},
			{Name: "GENESIS_NODES_DIR", Value: genesisNodesDir(sn)},
			{Name: "POOL_NAME", Value: sn.Name},
			{Name: "NAMESPACE", Value: sn.Namespace},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: dataDir},
			{Name: "genesis-data", MountPath: genesisDir, ReadOnly: true},
		},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prepJobName(sn, ordinal),
			Namespace: sn.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: nodeServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers:         []corev1.Container{mainContainer},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: dataPVCName(sn, ordinal),
								},
							},
						},
						{
							Name: "genesis-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: genesisPVCName(sn),
									ReadOnly:  true,
								},
							},
						},
					},
				},
			},
		},
	}
}
