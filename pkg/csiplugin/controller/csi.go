// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/lithammer/dedent"
	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type ControllerServer struct {
	csi.UnimplementedControllerServer
	Clientset *common.Clientset
	Image     string
}

func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// TODO: If we are cloning an existing volume but cloning is eventually cancelled before succeeding due to the
	// new PVC being deleted, the source PVC might forever be stuck in the "cloning" state and be unmountable. Fix
	// this somehow. Maybe add some label to the new PVC identifying the source PVC (if Kubernetes doesn't already
	// add one of those), and have a controller watch for their deletion and cancel volume clonings as needed.

	// TODO: Reject unknown parameters in req.Parameters?

	getParameter := func(key string) (string, error) {
		value := req.Parameters[key]
		if value == "" {
			return "", status.Errorf(codes.InvalidArgument, "missing/empty parameter \"%s\"", key)
		}
		return value, nil
	}

	pvcName, err := getParameter("csi.storage.k8s.io/pvc/name")
	if err != nil {
		return nil, err
	}
	pvcNamespace, err := getParameter("csi.storage.k8s.io/pvc/namespace")
	if err != nil {
		return nil, err
	}
	backingPvcName, err := getParameter("backingClaimName")
	if err != nil {
		return nil, err
	}
	backingPvcNamespace, err := getParameter("backingClaimNamespace")
	if err != nil {
		return nil, err
	}
	backingPvcBasePath := req.Parameters["basePath"]

	pvc, err := s.Clientset.CoreV1().
		PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// capacity

	capacity, _, maxCapacity, err := validateCapacity(req.CapacityRange)
	if err != nil {
		return nil, err
	}

	// capabilities

	for _, cap := range req.VolumeCapabilities {
		if cap.GetBlock() == nil {
			return nil, status.Errorf(codes.InvalidArgument, "only block volumes are supported")
		}

		switch cap.AccessMode.Mode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		default:
			return nil, status.Errorf(
				codes.InvalidArgument,
				"only access modes ReadWriteOnce, ReadWriteOncePod, and ReadOnlyMany are supported",
			)
		}
	}

	// We add a finalizer to the PVC here and remove it on deletion after all cleanup is done. DeleteVolume() is
	// only called once all finalizers are removed from the PVC. This ensures that problems with cleanup become
	// apparent due to the PVC not going away, and makes it harder for users to delete the backing volume while it
	// is still in use by a volume backed by it. Also, it ensures that volumes whose creation fails and is given up
	// on (because the corresponding PVC has meanwhile been deleted) are never leaked, as in those cases Kubernetes
	// doesn't know how to call DeleteVolume() because it doesn't know what VolumeId to use.

	err = common.StrategicMergePatchPvc(
		ctx, s.Clientset, pvcName, pvcNamespace,
		corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					common.Domain + "/uid": string(pvc.UID),
				},
				Annotations: map[string]string{
					common.Domain + "/backing-pvc-name":      backingPvcName,
					common.Domain + "/backing-pvc-namespace": backingPvcNamespace,
					common.Domain + "/backing-pvc-base-path": backingPvcBasePath,
					common.Domain + "/capacity":              strconv.FormatInt(capacity, 10),
					common.Domain + "/state":                 "idle",
				},
				Finalizers: []string{common.Domain + "/cleanup"},
			},
		},
	)
	if err != nil {
		return nil, err
	}

	// create qcow2 file

	if req.VolumeContentSource == nil {
		err = s.createVolumeFromNothing(
			ctx, backingPvcName, backingPvcNamespace, backingPvcBasePath, pvc, capacity,
		)
	} else if source := req.VolumeContentSource.GetVolume(); source != nil {
		err = s.createVolumeFromVolume(
			ctx, backingPvcName, backingPvcNamespace, backingPvcBasePath, pvc, capacity,
			maxCapacity, types.UID(source.VolumeId),
		)
	} else if source := req.VolumeContentSource.GetSnapshot(); source != nil {
		err = s.createVolumeFromSnapshot(
			ctx, backingPvcName, backingPvcNamespace, backingPvcBasePath, pvc, capacity,
			maxCapacity, types.UID(source.SnapshotId),
		)
	} else {
		err = status.Errorf(codes.InvalidArgument, "unsupported volume content source")
	}
	if err != nil {
		return nil, err
	}

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			CapacityBytes: capacity,
			VolumeId:      string(pvc.UID),
			VolumeContext: map[string]string{
				"pvcName":             pvcName,
				"pvcNamespace":        pvcNamespace,
				"backingPvcName":      backingPvcName,
				"backingPvcNamespace": backingPvcNamespace,
				"backingPvcBasePath":  backingPvcBasePath,
			},
			ContentSource: req.VolumeContentSource,
		},
	}
	return resp, nil
}

func (s *ControllerServer) createVolumeFromNothing(
	ctx context.Context,
	backingPvcName string,
	backingPvcNamespace string,
	backingPvcBasePath string,
	pvc *corev1.PersistentVolumeClaim,
	capacity int64,
) error {
	volumeImagePath := common.GenerateVolumeImagePath(pvc.UID)
	creationJobName := common.GenerateCreationJobName(pvc.UID)

	err := common.CreateJob(
		ctx, s.Clientset,
		common.JobConfig{
			Name:      creationJobName,
			Namespace: backingPvcNamespace,
			Labels: map[string]string{
				common.Domain + "/component": "volume-creation",
				common.Domain + "/pvc-uid":   string(pvc.UID),
			},
			Image: s.Image,
			Command: []string{
				"qemu-img", "create", "-f", "qcow2",
				volumeImagePath, strconv.FormatInt(capacity, 10),
			},
			BackingPvcName:     backingPvcName,
			BackingPvcBasePath: backingPvcBasePath,
		},
	)
	if err != nil {
		return err
	}

	err = common.WaitForJobToSucceed(ctx, s.Clientset, creationJobName, backingPvcNamespace)
	if err != nil {
		return err
	}

	// Keeping the volume creation Job around until the volume is deleted makes idempotency easier, so that's what
	// we do.

	return nil
}

func (s *ControllerServer) createVolumeFromVolume(
	ctx context.Context,
	backingPvcName string,
	backingPvcNamespace string,
	backingPvcBasePath string,
	destPvc *corev1.PersistentVolumeClaim,
	capacity int64,
	maxCapacity int64,
	sourcePvcUid types.UID,
) error {
	sourcePvc, err := common.FindPvcByLabelSelector(
		ctx, s.Clientset, fmt.Sprintf("%s/uid=%s", common.Domain, sourcePvcUid))
	if err != nil {
		return err
	}

	err = common.SetPvcStateTo(ctx, s.Clientset, sourcePvc.Name, sourcePvc.Namespace, "cloning")
	if err != nil {
		return err
	}

	sourceCapacity, err := strconv.ParseInt(sourcePvc.Annotations[common.Domain+"/capacity"], 10, 64)
	if err != nil {
		return status.Errorf(codes.Unknown, "failed to determine source volume capacity")
	}
	if maxCapacity != 0 && sourceCapacity > maxCapacity {
		return status.Errorf(
			codes.InvalidArgument, "source volume capacity (%d) exceeds maximum capacity (%d)",
			sourceCapacity, maxCapacity,
		)
	}
	if capacity < sourceCapacity {
		capacity = sourceCapacity
	}

	sourceVolumeImagePath := common.GenerateVolumeImagePath(sourcePvc.UID)
	destVolumeImagePath := common.GenerateVolumeImagePath(destPvc.UID)
	commonAncestorImageName := fmt.Sprintf("cloned-%s-to-%s.qcow2", sourcePvc.UID, destPvc.UID)
	creationJobName := common.GenerateCreationJobName(destPvc.UID)

	creationScript := dedent.Dedent(
		`
		set -o errexit -o pipefail -o nounset -o xtrace

		source="$1"
		dest="$2"
		common_ancestor_relative="$3"
		capacity="$4"

		# It's okay if we leave the "destination" volume image messed up when volume creation is cancelled, but
		# the same doesn't hold for the "source" volume image. Hence we replace the source volume image
		# atomically as the last operation.

		ln -f "${source}" "/var/backing/${common_ancestor_relative}"

		qemu-img create -f qcow2 -b "${common_ancestor_relative}" -F qcow2 "${dest}" "${capacity}"

		qemu-img create -f qcow2 -b "${common_ancestor_relative}" -F qcow2 "${source}.new"
		mv -f "${source}.new" "${source}"

		chmod a-w "/var/backing/${common_ancestor_relative}"  # should never modify this image
		`,
	)

	err = common.CreateJob(
		ctx, s.Clientset,
		common.JobConfig{
			Name:      creationJobName,
			Namespace: backingPvcNamespace,
			Labels: map[string]string{
				common.Domain + "/component": "volume-creation",
				common.Domain + "/pvc-uid":   string(destPvc.UID),
			},
			Image: s.Image,
			Command: []string{
				"bash", "-c", creationScript, "bash",
				sourceVolumeImagePath, destVolumeImagePath, commonAncestorImageName,
				strconv.FormatInt(capacity, 10),
			},
			BackingPvcName:     backingPvcName,
			BackingPvcBasePath: backingPvcBasePath,
		},
	)
	if err != nil {
		return err
	}

	err = common.WaitForJobToSucceed(ctx, s.Clientset, creationJobName, backingPvcNamespace)
	if err != nil {
		return err
	}

	err = common.SetPvcStateToIdle(ctx, s.Clientset, sourcePvc.Name, sourcePvc.Namespace)
	if err != nil {
		return err
	}

	// Keeping the volume creation Job around until the volume is deleted makes idempotency easier, so that's what
	// we do.

	return nil
}

func (s *ControllerServer) createVolumeFromSnapshot(
	ctx context.Context,
	backingPvcName string,
	backingPvcNamespace string,
	backingPvcBasePath string,
	destPvc *corev1.PersistentVolumeClaim,
	capacity int64,
	maxCapacity int64,
	volumeSnapshotUid types.UID,
) error {
	// TODO: Make sure snapshot is of volume with same backing volume configuration.

	volumeSnapshot, err := common.FindVolumeSnapshotByLabelSelector(
		ctx, s.Clientset, fmt.Sprintf("%s/uid=%s", common.Domain, volumeSnapshotUid))
	if err != nil {
		return err
	}

	snapshotSize, err := strconv.ParseInt(volumeSnapshot.Annotations[common.Domain+"/size"], 10, 64)
	if err != nil {
		return status.Errorf(codes.Unknown, "failed to determine source snapshot size")
	}
	if maxCapacity != 0 && snapshotSize > maxCapacity {
		return status.Errorf(
			codes.InvalidArgument, "source snapshot size (%d) exceeds maximum capacity (%d)",
			snapshotSize, maxCapacity,
		)
	}
	if capacity < snapshotSize {
		capacity = snapshotSize
	}

	creationJobName := common.GenerateCreationJobName(destPvc.UID)
	err = common.CreateJob(
		ctx, s.Clientset,
		common.JobConfig{
			Name:      creationJobName,
			Namespace: backingPvcNamespace,
			Labels: map[string]string{
				common.Domain + "/component": "volume-creation",
				common.Domain + "/pvc-uid":   string(destPvc.UID),
			},
			Image: s.Image,
			Command: []string{
				"qemu-img",
				"create",
				"-f", "qcow2",
				"-b", fmt.Sprintf("snapshot-%s.qcow2", volumeSnapshot.UID),
				"-F", "qcow2",
				fmt.Sprintf("/var/backing/pvc-%s.qcow2", destPvc.UID),
				strconv.FormatInt(capacity, 10),
			},
			BackingPvcName:     backingPvcName,
			BackingPvcBasePath: backingPvcBasePath,
		},
	)
	if err != nil {
		return err
	}

	err = common.WaitForJobToSucceed(ctx, s.Clientset, creationJobName, backingPvcNamespace)
	if err != nil {
		return err
	}

	// Keeping the volume creation Job around until the volume is deleted makes idempotency easier, so that's what
	// we do.

	return nil
}

func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	// This will only be called after we remove our finalizer from the volume's PVC, at which point the volume will
	// already have been deleted.

	if req.VolumeId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "must specify volume id")
	}

	resp := &csi.DeleteVolumeResponse{}
	return resp, nil
}

func (s *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ValidateVolumeCapabilities not required by Kubernetes")
}

func (s *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}

	csiCaps := make([]*csi.ControllerServiceCapability, len(caps))
	for i, cap := range caps {
		csiCaps[i] = &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: csiCaps,
	}
	return resp, nil
}

func (s *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	// TODO: If we are snapshotting a volume but snapshotting is eventually cancelled before succeeding due to the
	// VolumeSnapshot being deleted, the source PVC might forever be stuck in the "snapshotting" state and be
	// unmountable. Fix this somehow. Maybe add some label to the VolumeSnapshot identifying the source PVC (if
	// Kubernetes doesn't already add one of those), and have a controller watch for their deletion and cancel
	// volume snapshottings as needed.

	// TODO: Reject unknown parameters in req.Parameters?

	getParameter := func(key string) (string, error) {
		value := req.Parameters[key]
		if value == "" {
			return "", status.Errorf(codes.InvalidArgument, "missing/empty parameter \"%s\"", key)
		}
		return value, nil
	}

	volumeSnapshotName, err := getParameter("csi.storage.k8s.io/volumesnapshot/name")
	if err != nil {
		return nil, err
	}
	volumeSnapshotNamespace, err := getParameter("csi.storage.k8s.io/volumesnapshot/namespace")
	if err != nil {
		return nil, err
	}

	volumeSnapshot, err := s.Clientset.SnapshotV1().VolumeSnapshots(volumeSnapshotNamespace).
		Get(ctx, volumeSnapshotName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	sourcePvcUid := types.UID(req.SourceVolumeId)
	sourcePvc, err := common.FindPvcByLabelSelector(
		ctx, s.Clientset, fmt.Sprintf("%s/uid=%s", common.Domain, sourcePvcUid))
	if err != nil {
		return nil, err
	}

	err = common.SetPvcStateTo(ctx, s.Clientset, sourcePvc.Name, sourcePvc.Namespace, "snapshotting")
	if err != nil {
		return nil, err
	}

	backingPvcName := sourcePvc.Annotations[common.Domain+"/backing-pvc-name"]
	backingPvcNamespace := sourcePvc.Annotations[common.Domain+"/backing-pvc-namespace"]
	backingPvcBasePath := sourcePvc.Annotations[common.Domain+"/backing-pvc-base-path"]

	size, err := strconv.ParseInt(sourcePvc.Annotations[common.Domain+"/capacity"], 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, "failed to determine snapshot size")
	}

	err = common.MergePatchVolumeSnapshot(
		ctx, s.Clientset, volumeSnapshotName, volumeSnapshotNamespace,
		volumesnapshotv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					common.Domain + "/uid": string(volumeSnapshot.UID),
				},
				Annotations: map[string]string{
					common.Domain + "/backing-pvc-name":      backingPvcName,
					common.Domain + "/backing-pvc-namespace": backingPvcNamespace,
					common.Domain + "/backing-pvc-base-path": backingPvcBasePath,
					common.Domain + "/size":                  strconv.FormatInt(size, 10),
				},
			},
		},
	)
	if err != nil {
		return nil, err
	}

	snapshottingJobName := common.GenerateSnapshottingJobName(volumeSnapshot.UID)
	snapshottingScript := dedent.Dedent(
		`
		set -o errexit -o pipefail -o nounset -o xtrace

		pvc="$1"
		snapshot="$2"

		ln -f "/var/backing/${pvc}" "/var/backing/${snapshot}"

		qemu-img create -f qcow2 -b "${snapshot}" -F qcow2 "/var/backing/${pvc}.new"
		mv -f "/var/backing/${pvc}.new" "/var/backing/${pvc}"

		chmod a-w "/var/backing/${snapshot}"  # should never modify this image
		`,
	)

	err = common.CreateJob(
		ctx, s.Clientset,
		common.JobConfig{
			Name:      snapshottingJobName,
			Namespace: backingPvcNamespace,
			Labels: map[string]string{
				common.Domain + "/component": "volume-snapshotting",
				common.Domain + "/pvc-uid":   string(sourcePvc.UID),
			},
			Image: s.Image,
			Command: []string{
				"bash", "-c", snapshottingScript, "bash",
				fmt.Sprintf("pvc-%s.qcow2", sourcePvc.UID),
				fmt.Sprintf("snapshot-%s.qcow2", volumeSnapshot.UID),
			},
			BackingPvcName:     backingPvcName,
			BackingPvcBasePath: backingPvcBasePath,
		},
	)
	if err != nil {
		return nil, err
	}

	err = common.WaitForJobToSucceed(ctx, s.Clientset, snapshottingJobName, backingPvcNamespace)
	if err != nil {
		return nil, err
	}

	err = common.DeleteJobSynchronously(ctx, s.Clientset, snapshottingJobName, backingPvcNamespace)
	if err != nil {
		return nil, err
	}

	err = common.SetPvcStateToIdle(ctx, s.Clientset, sourcePvc.Name, sourcePvc.Namespace)
	if err != nil {
		return nil, err
	}

	resp := &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      size,
			SnapshotId:     string(volumeSnapshot.UID),
			SourceVolumeId: req.SourceVolumeId,
			CreationTime:   timestamppb.Now(), // is this fine?
			ReadyToUse:     true,
		},
	}
	return resp, nil
}

func (s *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	// TODO: Delete any qcow2 images in the backing chains that aren't referenced by any PVC or snapshot anymore. To
	// ensure idempotency, probably begin by creating graph of all qcow2 files connected to the top-level file being
	// deleted (regardless of edge direction), determine which will be left dangling and should be deleted, and
	// finally delete them all in one go. Must also take care to synchronize with volumes being created from the
	// snapshot.

	if req.SnapshotId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "must specify snapshot id")
	}

	resp := &csi.DeleteSnapshotResponse{}
	return resp, nil
}

func (s *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	// TODO: Handle case where this RPC is retried with a larger min capacity, but the volume expansion job is
	// already running and expanding the volume to the previous lower min capacity.

	// TODO: How can we ensure that the volume expansion job is cleaned up if this RPC fails and the PVC is deleted
	// before this RPC is retried? Maybe ensure here that we don't try to create the volume expansion job if the
	// volume is marked for deletion, and delete the volume expansion job from the PVC cleanup logic?

	if req.VolumeId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "must specify volume id")
	}

	// determine new capacity

	capacity, _, maxCapacity, err := validateCapacity(req.CapacityRange)
	if err != nil {
		return nil, err
	}

	// lookup PVC

	pvcUid := types.UID(req.VolumeId)

	pvc, err := common.FindPvcByLabelSelector(ctx, s.Clientset, fmt.Sprintf("%s/uid=%s", common.Domain, pvcUid))
	if err != nil {
		return nil, err
	}

	backingPvcName := pvc.Annotations[common.Domain+"/backing-pvc-name"]
	backingPvcNamespace := pvc.Annotations[common.Domain+"/backing-pvc-namespace"]
	backingPvcBasePath := pvc.Annotations[common.Domain+"/backing-pvc-base-path"]

	currentCapacity, err := strconv.ParseInt(pvc.Annotations[common.Domain+"/capacity"], 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, "failed to determine current volume capacity")
	}
	if maxCapacity != 0 && currentCapacity > maxCapacity {
		return nil, status.Errorf(
			codes.InvalidArgument, "current volume capacity (%d) exceeds maximum capacity (%d)",
			currentCapacity, maxCapacity,
		)
	}

	if currentCapacity >= capacity {
		// The volume is already big enough. One reason this may happen is that this gRPC was called before and
		// succeeded, but the external-resizer sidecar container failed to patch the PVC because the PVC was
		// mutated while the gRPC was being run (we changed the state annotation on it twice). external-resizer
		// should arguably be fixed to tolerate this. TODO: We should eventually get rid of annotations on the
		// PVC that the user can control, though, and this problem may just go away then.
		resp := &csi.ControllerExpandVolumeResponse{
			CapacityBytes:         currentCapacity,
			NodeExpansionRequired: false,
		}
		return resp, nil
	}

	// update volume state

	err = common.SetPvcStateTo(ctx, s.Clientset, pvc.Name, pvc.Namespace, "expanding")
	if err != nil {
		return nil, err
	}

	// create volume expansion job

	volumeImagePath := common.GenerateVolumeImagePath(pvc.UID)
	expansionJobName := common.GenerateExpansionJobName(pvc.UID)

	expansionScript := dedent.Dedent(
		`
		set -o errexit -o pipefail -o nounset -o xtrace
		size="$( qemu-img info -f qcow2 --output=json "$1" | jq '.["virtual-size"]' )"
		if [ "${size}" -lt "$2" ]; then
		    qemu-img resize -f qcow2 "$1" "$2"
		fi
		`,
	)

	err = common.CreateJob(
		ctx, s.Clientset,
		common.JobConfig{
			Name:      expansionJobName,
			Namespace: backingPvcNamespace,
			Labels: map[string]string{
				common.Domain + "/component": "volume-expansion",
				common.Domain + "/pvc-uid":   string(pvc.UID),
			},
			Image: s.Image,
			Command: []string{
				"bash", "-c", expansionScript, "bash",
				volumeImagePath, strconv.FormatInt(capacity, 10),
			},
			BackingPvcName:     backingPvcName,
			BackingPvcBasePath: backingPvcBasePath,
		},
	)
	if err != nil {
		return nil, err
	}

	// await volume expansion job

	err = common.WaitForJobToSucceed(ctx, s.Clientset, expansionJobName, backingPvcNamespace)
	if err != nil {
		return nil, err
	}

	// delete volume expansion job

	err = common.DeleteJobSynchronously(ctx, s.Clientset, expansionJobName, backingPvcNamespace)
	if err != nil {
		return nil, err
	}

	// set volume back to idle

	err = common.StrategicMergePatchPvc(
		ctx, s.Clientset, pvc.Name, pvc.Namespace,
		corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					common.Domain + "/capacity": strconv.FormatInt(capacity, 10),
					common.Domain + "/state":    "idle",
				},
			},
		},
	)
	if err != nil {
		return nil, err
	}

	resp := &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         capacity,
		NodeExpansionRequired: false,
	}
	return resp, nil
}

func validateCapacity(capacityRange *csi.CapacityRange) (capacity int64, minCapacity int64, maxCapacity int64, err error) {
	if capacityRange == nil {
		return -1, -1, -1, status.Errorf(codes.InvalidArgument, "must specify capacity")
	}

	minCapacity = capacityRange.RequiredBytes
	maxCapacity = capacityRange.LimitBytes

	if minCapacity == 0 {
		return -1, -1, -1, status.Errorf(codes.InvalidArgument, "must specify minimum capacity")
	}
	if maxCapacity != 0 && maxCapacity < minCapacity {
		return -1, -1, -1, status.Errorf(codes.InvalidArgument, "minimum capacity must not exceed maximum capacity")
	}

	// qcow2 image size must be a multiple of 512, so round minCapacity up to a multiple of 512. TODO: Check for
	// overflow.
	capacity = (minCapacity + 511) / 512 * 512

	if maxCapacity != 0 && maxCapacity < capacity {
		return -1, -1, -1, status.Errorf(codes.InvalidArgument, "capacity must be a multiple of 512")
	}

	return
}
