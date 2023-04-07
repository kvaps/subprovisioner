// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/ptypes/wrappers"
	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/common"
)

type IdentityServer struct {
	csi.UnimplementedIdentityServer
}

func (s *IdentityServer) GetPluginInfo(ctx context.Context, in *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	resp := &csi.GetPluginInfoResponse{
		Name:          common.Domain,
		VendorVersion: common.Version,
	}
	return resp, nil
}

func (s *IdentityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	caps := []*csi.PluginCapability{
		{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
				},
			},
		},
		{
			Type: &csi.PluginCapability_VolumeExpansion_{
				VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
					Type: csi.PluginCapability_VolumeExpansion_OFFLINE,
				},
			},
		},
	}

	resp := &csi.GetPluginCapabilitiesResponse{
		Capabilities: caps,
	}
	return resp, nil
}

func (s *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	resp := &csi.ProbeResponse{
		Ready: &wrappers.BoolValue{
			Value: true,
		},
	}
	return resp, nil
}
