// SPDX-License-Identifier: Apache-2.0

package common

import (
	"crypto/sha256"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

const (
	Domain  = "subprovisioner.gitlab.io"
	Version = "0.0.0"
)

func GenerateVolumeImagePath(pvcUid types.UID) string {
	return fmt.Sprintf("/var/backing/pvc-%s.qcow2", pvcUid)
}

func GenerateSnapshotImagePath(volumeSnapshotUid types.UID) string {
	return fmt.Sprintf("/var/backing/snapshot-%s.qcow2", volumeSnapshotUid)
}

func GenerateCreationJobName(pvcUid types.UID) string {
	return fmt.Sprintf("subprovisioner-create-%s", pvcUid)
}

func GenerateDeletionJobName(pvcUid types.UID) string {
	return fmt.Sprintf("subprovisioner-delete-%s", pvcUid)
}

func GenerateSnapshottingJobName(volumeSnapshotUid types.UID) string {
	return fmt.Sprintf("subprovisioner-snapshot-%s", volumeSnapshotUid)
}

func GenerateExpansionJobName(pvcUid types.UID) string {
	return fmt.Sprintf("subprovisioner-expand-%s", pvcUid)
}

func GenerateStagingReplicaSetName(pvcUid types.UID, nodeName string) string {
	// Node object names must be DNS Subdomain Names, and so can be up to 253 characters in length, which means we
	// can't embed nodeName directly in the object name we return here. But we also don't want to use the Node
	// object's uid, just in case the Node object is recreated with the same name for some reason but still refers
	// to the same actual node in the cluster. We thus hash nodeName and append the result to the object name
	// instead, and use SHA-256 to ensure there are no accidental (or purposeful) collisions.
	hashedNodeName := sha256.Sum256([]byte(nodeName))
	return fmt.Sprintf("subprovisioner-stage-%s-on-%x", pvcUid, hashedNodeName)
}
