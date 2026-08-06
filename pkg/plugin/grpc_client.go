// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"

	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1"

	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
)

const defaultPluginDir = "/var/lib/kubelet/plugins"

// GRPCClient connects to underlying DRA drivers via their kubelet plugin gRPC sockets.
type GRPCClient struct {
	pluginDir string

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

func NewGRPCClient(pluginDir string) *GRPCClient {
	if pluginDir == "" {
		pluginDir = defaultPluginDir
	}
	return &GRPCClient{
		pluginDir: pluginDir,
		conns:     make(map[string]*grpc.ClientConn),
	}
}

// Prepare calls NodePrepareResources on the underlying driver for a shadow claim.
// Returns the CDI device IDs from the response.
func (c *GRPCClient) Prepare(ctx context.Context, driverName string, claim *shadow.ShadowClaimInfo) (*drapbv1.NodePrepareResourceResponse, error) {
	client, err := c.getClient(driverName)
	if err != nil {
		return nil, err
	}

	req := &drapbv1.NodePrepareResourcesRequest{
		Claims: []*drapbv1.Claim{
			{
				Namespace: claim.Namespace,
				Uid:       claim.UID,
				Name:      claim.Name,
			},
		},
	}

	klog.V(2).InfoS("grpc: calling NodePrepareResources", "driver", driverName, "namespace", claim.Namespace, "claim", claim.Name, "uid", claim.UID)

	resp, err := client.NodePrepareResources(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("NodePrepareResources on %s: %w", driverName, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("driver %s returned a nil NodePrepareResources response", driverName)
	}

	claimResp, ok := resp.Claims[claim.UID]
	if !ok || claimResp == nil {
		return nil, fmt.Errorf("driver %s did not return a response for claim %s", driverName, claim.UID)
	}
	if claimResp.Error != "" {
		return nil, fmt.Errorf("driver %s prepare failed for claim %s: %s", driverName, claim.UID, claimResp.Error)
	}

	return claimResp, nil
}

// Unprepare calls NodeUnprepareResources on the underlying driver for a shadow claim.
func (c *GRPCClient) Unprepare(ctx context.Context, driverName string, claim *shadow.ShadowClaimInfo) error {
	client, err := c.getClient(driverName)
	if err != nil {
		return err
	}

	req := &drapbv1.NodeUnprepareResourcesRequest{
		Claims: []*drapbv1.Claim{
			{
				Namespace: claim.Namespace,
				Uid:       claim.UID,
				Name:      claim.Name,
			},
		},
	}

	klog.V(2).InfoS("grpc: calling NodeUnprepareResources", "driver", driverName, "namespace", claim.Namespace, "claim", claim.Name)

	resp, err := client.NodeUnprepareResources(ctx, req)
	if err != nil {
		return fmt.Errorf("NodeUnprepareResources on %s: %w", driverName, err)
	}
	if resp == nil {
		return fmt.Errorf("driver %s returned a nil NodeUnprepareResources response", driverName)
	}

	claimResp, ok := resp.Claims[claim.UID]
	if !ok || claimResp == nil {
		// The DRA contract requires one result per input claim, so a missing entry is
		// a protocol violation, not proof that teardown succeeded.
		return fmt.Errorf("driver %s did not return an unprepare response for claim %s", driverName, claim.UID)
	}
	if claimResp.Error != "" {
		return fmt.Errorf("driver %s unprepare failed for claim %s: %s", driverName, claim.UID, claimResp.Error)
	}

	return nil
}

func (c *GRPCClient) getClient(driverName string) (drapbv1.DRAPluginClient, error) {
	conn, err := c.getConn(driverName)
	if err != nil {
		return nil, err
	}
	return drapbv1.NewDRAPluginClient(conn), nil
}

func (c *GRPCClient) getConn(driverName string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if conn, ok := c.conns[driverName]; ok {
		return conn, nil
	}

	socketPath := filepath.Join(c.pluginDir, driverName, "dra.sock")
	target := "unix://" + socketPath

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s at %s: %w", driverName, socketPath, err)
	}

	c.conns[driverName] = conn
	klog.InfoS("grpc: connected", "driver", driverName, "socket", socketPath)
	return conn, nil
}

// Close closes all gRPC connections.
func (c *GRPCClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, conn := range c.conns {
		conn.Close()
		klog.InfoS("grpc: closed connection", "driver", name)
	}
	c.conns = make(map[string]*grpc.ClientConn)
}
