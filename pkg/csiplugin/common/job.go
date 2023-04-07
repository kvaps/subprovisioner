// SPDX-License-Identifier: Apache-2.0

package common

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type JobConfig struct {
	Name      string
	Namespace string
	Labels    map[string]string

	Image   string
	Command []string
	Args    []string

	BackingPvcName     string
	BackingPvcBasePath string
}

// Idempotent. The backing volume is mounted at "/var/backing".
func CreateJob(ctx context.Context, clientset *Clientset, config JobConfig) error {
	podSpec := v1.PodSpec{
		RestartPolicy: v1.RestartPolicyNever,
		Containers: []v1.Container{
			{
				Name:    "container",
				Image:   config.Image,
				Command: config.Command,
				Args:    config.Args,
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "backing",
						MountPath: "/var/backing",
						SubPath:   config.BackingPvcBasePath,
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
		},
	}

	var backofflimit int32 = 99999
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    config.Labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backofflimit,
			Template: v1.PodTemplateSpec{
				Spec: podSpec,
			},
		},
	}

	_, err := clientset.BatchV1().Jobs(config.Namespace).Create(ctx, &job, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return err
	}

	return nil
}

func WaitForJobToSucceed(
	ctx context.Context,
	clientset *Clientset,
	jobName string,
	jobNamespace string,
) error {
	// TODO: Watch instead of polling.
	for {
		job, err := clientset.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if job.Status.Succeeded > 0 {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
}

// Idempotent. Succeeds immediately if the object no longer exists.
func DeleteJobSynchronously(
	ctx context.Context,
	clientset *Clientset,
	jobName string,
	jobNamespace string,
) error {
	jobs := clientset.BatchV1().Jobs(jobNamespace)

	propagationPolicy := metav1.DeletePropagationForeground
	err := jobs.Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &propagationPolicy})

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

		_, err = jobs.Get(ctx, jobName, metav1.GetOptions{})
	}
}
