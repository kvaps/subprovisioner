// SPDX-License-Identifier: Apache-2.0

package common

import (
	"context"
	"errors"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ReplicaSetConfig struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string

	MatchLabels map[string]string
	Replicas    int32
	NodeName    string

	Image   string
	Command []string
	Args    []string

	BackingPvcName     string
	BackingPvcBasePath string
}

// Idempotent. The backing volume is mounted at "/var/backing", and "/var/lib/kubelet" is passed through to the
// container.
func CreateReplicaSet(ctx context.Context, clientset *Clientset, config ReplicaSetConfig) error {
	privileged := true
	hostPathType := v1.HostPathDirectory
	podSpec := v1.PodSpec{
		NodeName: config.NodeName,
		Containers: []v1.Container{
			{
				Name:    "container",
				Image:   config.Image,
				Command: config.Command,
				Args:    config.Args,
				SecurityContext: &v1.SecurityContext{
					Privileged: &privileged,
				},
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "backing",
						MountPath: "/var/backing",
						SubPath:   config.BackingPvcBasePath,
					},
					{
						Name:      "plugins-dir",
						MountPath: "/var/lib/kubelet/plugins",
					},
					{
						Name:      "volume-dir",
						MountPath: "/var/lib/kubelet/pods",
					},
				},
			},
		},
		Volumes: []v1.Volume{
			{
				Name: "backing",
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
						ClaimName: config.BackingPvcName,
					},
				},
			},
			{
				Name: "plugins-dir",
				VolumeSource: v1.VolumeSource{
					HostPath: &v1.HostPathVolumeSource{
						Path: "/var/lib/kubelet/plugins",
						Type: &hostPathType,
					},
				},
			},
			{
				Name: "volume-dir",
				VolumeSource: v1.VolumeSource{
					HostPath: &v1.HostPathVolumeSource{
						Path: "/var/lib/kubelet/pods",
						Type: &hostPathType,
					},
				},
			},
		},
	}

	replicaSet := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      config.Labels,
			Annotations: config.Annotations,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &config.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: config.MatchLabels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: config.MatchLabels,
				},
				Spec: podSpec,
			},
		},
	}

	_, err := clientset.AppsV1().ReplicaSets(config.Namespace).Create(ctx, &replicaSet, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return err
	}

	return nil
}

func FindReplicaSetByLabelSelector(
	ctx context.Context,
	clientset *Clientset,
	labelSelector string,
) (*appsv1.ReplicaSet, error) {
	list, err := clientset.AppsV1().ReplicaSets(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}

	switch len(list.Items) {
	case 0:
		return nil, errors.New("no objects found")
	case 1:
		return &list.Items[0], nil
	default:
		return nil, errors.New("more than one object found")
	}
}

// Idempotent. Succeeds immediately if the object no longer exists.
func DeleteReplicaSetSynchronously(
	ctx context.Context,
	clientset *Clientset,
	replicaSetName string,
	replicaSetNamespace string,
) error {
	replicaSets := clientset.AppsV1().ReplicaSets(replicaSetNamespace)

	propagationPolicy := metav1.DeletePropagationForeground
	err := replicaSets.Delete(ctx, replicaSetName, metav1.DeleteOptions{PropagationPolicy: &propagationPolicy})

	// TODO: Watch instead of polling.
	for {
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return nil
			} else {
				return err
			}
		}

		time.Sleep(1 * time.Second)

		_, err = replicaSets.Get(ctx, replicaSetName, metav1.GetOptions{})
	}
}
