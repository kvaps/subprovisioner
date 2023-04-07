// SPDX-License-Identifier: Apache-2.0

package common

import (
	"context"
	"encoding/json"
	"errors"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func FindVolumeSnapshotByLabelSelector(
	ctx context.Context,
	clientset *Clientset,
	labelSelector string,
) (*volumesnapshotv1.VolumeSnapshot, error) {
	list, err := clientset.SnapshotV1().VolumeSnapshots(metav1.NamespaceAll).
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

func MergePatchVolumeSnapshot(
	ctx context.Context,
	clientset *Clientset,
	volumeSnapshotName string,
	volumeSnapshotNamespace string,
	patch volumesnapshotv1.VolumeSnapshot,
) error {
	jsonPatch, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = clientset.SnapshotV1().VolumeSnapshots(volumeSnapshotNamespace).
		Patch(ctx, volumeSnapshotName, types.MergePatchType, jsonPatch, metav1.PatchOptions{})
	return err
}
