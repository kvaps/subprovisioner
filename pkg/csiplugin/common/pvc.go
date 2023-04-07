// SPDX-License-Identifier: Apache-2.0

package common

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

func FindPvcByLabelSelector(
	ctx context.Context,
	clientset *Clientset,
	labelSelector string,
) (*corev1.PersistentVolumeClaim, error) {
	list, err := clientset.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).
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

func StrategicMergePatchPvc(
	ctx context.Context,
	clientset *Clientset,
	pvcName string,
	pvcNamespace string,
	patch corev1.PersistentVolumeClaim,
) error {
	jsonPatch, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).
		Patch(ctx, pvcName, types.StrategicMergePatchType, jsonPatch, metav1.PatchOptions{})
	return err
}

func SetPvcStateToIdle(
	ctx context.Context,
	clientset *Clientset,
	pvcName string,
	pvcNamespace string,
) error {
	return StrategicMergePatchPvc(
		ctx, clientset, pvcName, pvcNamespace,
		corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{Domain + "/state": "idle"},
			},
		},
	)
}

func SetPvcStateTo(
	ctx context.Context,
	clientset *Clientset,
	pvcName string,
	pvcNamespace string,
	newState string,
) error {
	pvcs := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvc, err := pvcs.Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if pvc.DeletionTimestamp != nil {
			return status.Errorf(codes.FailedPrecondition, "volume is being deleted")
		}

		switch pvc.Annotations[Domain+"/state"] {
		case newState:
			return nil
		case "idle":
			pvc.Annotations[Domain+"/state"] = newState
			_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
			return err
		case "expanding":
			return status.Errorf(codes.FailedPrecondition, "volume is being expanded")
		case "cloning":
			return status.Errorf(codes.FailedPrecondition, "volume is being cloned")
		case "snapshotting":
			return status.Errorf(codes.FailedPrecondition, "volume is being snapshotted")
		case "staged":
			return status.Errorf(codes.FailedPrecondition, "volume is staged")
		default:
			return status.Errorf(codes.FailedPrecondition, "volume is in an unknown state")
		}
	})
}

func StagePvcOnNode(
	ctx context.Context,
	clientset *Clientset,
	pvcName string,
	pvcNamespace string,
	nodeName string,
) error {
	pvcs := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvc, err := pvcs.Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		state := pvc.Annotations[Domain+"/state"]

		if pvc.DeletionTimestamp != nil {
			return status.Errorf(codes.FailedPrecondition, "volume is being deleted")
		} else if state == "expanding" {
			return status.Errorf(codes.FailedPrecondition, "volume is being expanded")
		} else if state == "snapshotting" {
			return status.Errorf(codes.FailedPrecondition, "volume is being snapshotted")
		} else if state == "cloning" {
			return status.Errorf(codes.FailedPrecondition, "volume is being cloned")
		} else if state != "idle" && state != "staged" {
			return status.Errorf(codes.FailedPrecondition, "volume is in an unknown state")
		}

		pvc.Annotations[Domain+"/state"] = "staged"

		stagedOnNodes := stringListToSet(pvc.Annotations[Domain+"/staged-on-nodes"])
		stagedOnNodes[nodeName] = struct{}{}
		pvc.Annotations[Domain+"/staged-on-nodes"] = setToStringList(stagedOnNodes)

		_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
		return err
	})
}

func UnstagePvcFromNode(
	ctx context.Context,
	clientset *Clientset,
	pvcName string,
	pvcNamespace string,
	nodeName string,
) error {
	pvcs := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvc, err := pvcs.Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if pvc.Annotations[Domain+"/state"] == "staged" {
			stagedOnNodes := stringListToSet(pvc.Annotations[Domain+"/staged-on-nodes"])
			delete(stagedOnNodes, nodeName)

			if len(stagedOnNodes) == 0 {
				delete(pvc.Annotations, Domain+"/staged-on-nodes")
				pvc.Annotations[Domain+"/state"] = "idle"
			} else {
				pvc.Annotations[Domain+"/staged-on-nodes"] = setToStringList(stagedOnNodes)
			}
		}

		_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
		return err
	})
}

func stringListToSet(list string) map[string]struct{} {
	set := map[string]struct{}{}
	if list != "" {
		for _, item := range strings.Split(list, ",") {
			set[item] = struct{}{}
		}
	}
	return set
}

func setToStringList(set map[string]struct{}) string {
	var builder strings.Builder
	empty := true
	for item := range set {
		if !empty {
			builder.WriteRune(',')
		}
		builder.WriteString(item)
		empty = false
	}
	return builder.String()
}
