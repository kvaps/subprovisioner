// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

type NodeServer struct {
	csi.UnimplementedNodeServer
	Clientset *common.Clientset
	NodeName  string
	Image     string
}

func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	// TODO: If QSD pod fails, Kubernetes might just try to continuously unpublish and publish the volume, which
	// will go nowhere, instead of also unstaging and restaging it. How can we avoid this? Maybe just make the QSD
	// pod recover automatically?

	// TODO: NBD client cleanup is currently best-effort. Is it possible to make it more reliable somehow?

	// TODO: Must enforce access modes ourselves; check the CSI spec.

	if req.VolumeCapability.GetBlock() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "expected a block volume")
	}

	var readonly bool
	switch req.VolumeCapability.AccessMode.Mode {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		readonly = true
	default:
		readonly = false
	}

	pvcUid := types.UID(req.VolumeId)
	pvcName := req.VolumeContext["pvcName"]
	pvcNamespace := req.VolumeContext["pvcNamespace"]
	backingPvcName := req.VolumeContext["backingPvcName"]
	backingPvcNamespace := req.VolumeContext["backingPvcNamespace"]
	backingPvcBasePath := req.VolumeContext["backingPvcBasePath"]

	// add node name to PVC annotation listing nodes on which it is staged

	err := common.StagePvcOnNode(ctx, s.Clientset, pvcName, pvcNamespace, s.NodeName)
	if err != nil {
		return nil, err
	}

	// stage volume

	volumeImagePath := common.GenerateVolumeImagePath(pvcUid)
	stagingReplicaSetName := common.GenerateStagingReplicaSetName(pvcUid, s.NodeName)

	labels := map[string]string{
		common.Domain + "/component": "volume-staging",
		common.Domain + "/node-name": s.NodeName,
		common.Domain + "/pvc-uid":   string(pvcUid),
	}

	// TODO: Is it possible to configure NBD block devices without having to set
	// securityContext.privileged to true on the QSD container? Does it matter, given we need it for
	// file system mounts (probably)?
	err = common.CreateReplicaSet(
		ctx, s.Clientset,
		common.ReplicaSetConfig{
			Name:      stagingReplicaSetName,
			Namespace: backingPvcNamespace,
			Labels:    labels,
			Annotations: map[string]string{
				common.Domain + "/pvc-name":              pvcName,
				common.Domain + "/pvc-namespace":         pvcNamespace,
				common.Domain + "/backing-pvc-name":      backingPvcName,
				common.Domain + "/backing-pvc-namespace": backingPvcNamespace,
			},
			MatchLabels: labels,
			Replicas:    1,
			NodeName:    s.NodeName,
			Image:       s.Image,
			Command: []string{
				"/subprovisioner/qsd-with-nbd.sh",
				volumeImagePath, req.StagingTargetPath, strconv.FormatBool(readonly),
			},
			BackingPvcName:     backingPvcName,
			BackingPvcBasePath: backingPvcBasePath,
		},
	)
	if err != nil {
		return nil, err
	}

	err = common.WaitUntilFileIsBlockDevice(ctx, req.StagingTargetPath)
	if err != nil {
		return nil, err
	}

	resp := &csi.NodeStageVolumeResponse{}
	return resp, nil
}

func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	pvcUid := types.UID(req.VolumeId)

	// delete volume staging ReplicaSet

	stagingReplicaSet, err := common.FindReplicaSetByLabelSelector(
		ctx, s.Clientset,
		strings.Join(
			[]string{
				fmt.Sprintf("%s/component=volume-staging", common.Domain),
				fmt.Sprintf("%s/node-name=%s", common.Domain, s.NodeName),
				fmt.Sprintf("%s/pvc-uid=%s", common.Domain, pvcUid),
			},
			",",
		),
	)
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, err
	}

	if err == nil {
		err = common.DeleteReplicaSetSynchronously(
			ctx, s.Clientset,
			stagingReplicaSet.Name, stagingReplicaSet.Namespace,
		)
		if err != nil {
			return nil, err
		}
	}

	// delete block special file

	err = os.Remove(req.StagingTargetPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// remove node name to PVC annotation listing nodes on which it is staged

	pvc, err := common.FindPvcByLabelSelector(ctx, s.Clientset, fmt.Sprintf("%s/uid=%s", common.Domain, pvcUid))
	if err != nil {
		return nil, err
	}

	err = common.UnstagePvcFromNode(ctx, s.Clientset, pvc.Name, pvc.Namespace, s.NodeName)
	if err != nil {
		return nil, err
	}

	resp := &csi.NodeUnstageVolumeResponse{}
	return resp, nil
}

func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	// TODO: Must enforce access modes ourselves; check the CSI spec.

	// Kubernetes might place a directory at the path where the block node should go (for some reason). TODO: Check
	// if that isn't our fault somehow.
	err := os.Remove(req.TargetPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	err = os.Symlink(req.StagingTargetPath, req.TargetPath)
	if err != nil {
		return nil, err
	}

	if req.Readonly {
		// TODO: Is changing the block node mode sufficient here?

		stat, err := os.Stat(req.TargetPath)
		if err != nil {
			return nil, err
		}

		err = os.Chmod(req.TargetPath, stat.Mode() & ^fs.FileMode(0222)) // clear write bits
		if err != nil {
			return nil, err
		}
	}

	resp := &csi.NodePublishVolumeResponse{}
	return resp, nil
}

func (s *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	err := os.Remove(req.TargetPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	resp := &csi.NodeUnpublishVolumeResponse{}
	return resp, nil
}

func (s *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}

	csiCaps := make([]*csi.NodeServiceCapability, len(caps))
	for i, cap := range caps {
		csiCaps[i] = &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	resp := &csi.NodeGetCapabilitiesResponse{
		Capabilities: csiCaps,
	}
	return resp, nil
}

func (s *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	resp := &csi.NodeGetInfoResponse{
		NodeId: s.NodeName,
	}
	return resp, nil
}
