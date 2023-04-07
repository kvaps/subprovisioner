// SPDX-License-Identifier: Apache-2.0

package csiplugin

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/external-snapshotter/client/v6/clientset/versioned"
	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/common"
	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/controller"
	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/identity"
	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/node"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func RunControllerPlugin(csiSocketPath string, image string) error {
	clientset, listener, server, err := setup(csiSocketPath)
	if err != nil {
		return err
	}

	// run monitor

	monitor := controller.ControllerMonitor{
		Clientset: clientset,
		Image:     image,
	}
	go monitor.Run()

	// run gRPC server

	csi.RegisterIdentityServer(server, &identity.IdentityServer{})
	csi.RegisterControllerServer(server, &controller.ControllerServer{
		Clientset: clientset,
		Image:     image,
	})
	return server.Serve(listener)

	// TODO: Handle SIGTERM gracefully.
}

func RunNodePlugin(csiSocketPath string, nodeName string, image string) error {
	clientset, listener, server, err := setup(csiSocketPath)
	if err != nil {
		return err
	}

	// run gRPC server

	csi.RegisterIdentityServer(server, &identity.IdentityServer{})
	csi.RegisterNodeServer(server, &node.NodeServer{
		Clientset: clientset,
		NodeName:  nodeName,
		Image:     image,
	})
	return server.Serve(listener)

	// TODO: Handle SIGTERM gracefully.
}

func setup(csiSocketPath string) (*common.Clientset, net.Listener, *grpc.Server, error) {
	// set up Kubernetes API connection

	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, nil, err
	}

	kubernetesClientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, err
	}

	snapshotClientset, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, err
	}

	clientset := &common.Clientset{
		Clientset:         kubernetesClientset,
		SnapshotClientSet: snapshotClientset,
	}

	// create gRPC server

	err = os.Remove(csiSocketPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, nil, err
	}

	listener, err := net.Listen("unix", csiSocketPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to listen: %v", err)
	}

	interceptor := func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		log.Printf("%s({ %+v})", info.FullMethod, req)
		resp, err := handler(ctx, req)
		if err == nil {
			log.Printf("%s(...) --> { %+v}", info.FullMethod, resp)
		} else {
			log.Printf("%s(...) --> %+v", info.FullMethod, err)
		}
		return resp, err
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(interceptor))

	return clientset, listener, server, nil
}
