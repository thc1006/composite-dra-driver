// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package shadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
	"k8s.io/klog/v2"

	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

// shadowClaimName generates a deterministic name for a shadow claim.
// Hashes the composite claim name to a short prefix so the member-identifying
// suffix (sourceName + device) has room within the 63-char K8s name limit.
func shadowClaimName(compositeClaimName string, member *store.DeviceMember) string {
	h := sha256.Sum256([]byte(compositeClaimName))
	prefix := hex.EncodeToString(h[:4])
	name := fmt.Sprintf("shadow-%s-%s-%s", prefix, member.SourceName, member.Device)
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// ClaimManager creates and deletes shadow ResourceClaims for underlying drivers.
type ClaimManager struct {
	client     resourceclient.ResourceV1Interface
	driverName string
}

func NewClaimManager(client resourceclient.ResourceV1Interface, driverName string) *ClaimManager {
	return &ClaimManager{
		client:     client,
		driverName: driverName,
	}
}

// ShadowClaimInfo holds the created shadow claim details needed for gRPC calls.
type ShadowClaimInfo struct {
	Namespace string
	Name      string
	UID       string
}

// Create builds and persists a shadow ResourceClaim for one underlying device member.
// The shadow claim has:
// - Pre-filled allocation pointing to the specific underlying device
// - ReservedFor set to the pod that owns the composite claim
// - OwnerReference pointing to the composite claim for GC
func (m *ClaimManager) Create(
	ctx context.Context,
	compositeClaim *resourceapi.ResourceClaim,
	member *store.DeviceMember,
	requestName string,
	opaqueConfig []byte,
) (*ShadowClaimInfo, error) {
	shadowName := shadowClaimName(compositeClaim.Name, member)

	allocationResult := resourceapi.AllocationResult{
		Devices: resourceapi.DeviceAllocationResult{
			Results: []resourceapi.DeviceRequestAllocationResult{
				{
					Request: requestName,
					Driver:  member.Driver,
					Pool:    member.Pool,
					Device:  member.Device,
				},
			},
		},
	}

	if opaqueConfig != nil {
		allocationResult.Devices.Config = []resourceapi.DeviceAllocationConfiguration{
			{
				Source: resourceapi.AllocationConfigSourceClaim,
				Requests: []string{requestName},
				DeviceConfiguration: resourceapi.DeviceConfiguration{
					Opaque: &resourceapi.OpaqueDeviceConfiguration{
						Driver:     member.Driver,
						Parameters: runtime.RawExtension{Raw: opaqueConfig},
					},
				},
			},
		}
	}

	shadowClaim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shadowName,
			Namespace: compositeClaim.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": m.driverName,
				"composite-claim-uid":          string(compositeClaim.UID),
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "resource.k8s.io/v1",
					Kind:       "ResourceClaim",
					Name:       compositeClaim.Name,
					UID:        compositeClaim.UID,
				},
			},
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests: []resourceapi.DeviceRequest{
					{
						Name: requestName,
						Exactly: &resourceapi.ExactDeviceRequest{
							DeviceClassName: member.DeviceClassName,
							AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
							Count:           1,
						},
					},
				},
			},
		},
	}

	created, err := m.client.ResourceClaims(compositeClaim.Namespace).Create(ctx, shadowClaim, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create shadow claim %s: %w", shadowName, err)
	}

	created.Status = resourceapi.ResourceClaimStatus{
		Allocation:  &allocationResult,
		ReservedFor: compositeClaim.Status.ReservedFor,
	}

	updated, err := m.client.ResourceClaims(compositeClaim.Namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		_ = m.client.ResourceClaims(compositeClaim.Namespace).Delete(ctx, shadowName, metav1.DeleteOptions{})
		return nil, fmt.Errorf("update shadow claim status %s: %w", shadowName, err)
	}

	klog.V(2).InfoS("shadow: created claim", "namespace", updated.Namespace, "name", updated.Name, "uid", updated.UID, "driver", member.Driver, "pool", member.Pool, "device", member.Device)

	return &ShadowClaimInfo{
		Namespace: updated.Namespace,
		Name:      updated.Name,
		UID:       string(updated.UID),
	}, nil
}

// Get fetches an existing shadow claim's info by its deterministic name.
func (m *ClaimManager) Get(
	ctx context.Context,
	compositeClaim *resourceapi.ResourceClaim,
	member *store.DeviceMember,
) (*ShadowClaimInfo, error) {
	shadowName := shadowClaimName(compositeClaim.Name, member)

	existing, err := m.client.ResourceClaims(compositeClaim.Namespace).Get(ctx, shadowName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get shadow claim %s: %w", shadowName, err)
	}

	return &ShadowClaimInfo{
		Namespace: existing.Namespace,
		Name:      existing.Name,
		UID:       string(existing.UID),
	}, nil
}

// Delete removes a shadow ResourceClaim.
func (m *ClaimManager) Delete(ctx context.Context, namespace, name string) error {
	err := m.client.ResourceClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("delete shadow claim %s/%s: %w", namespace, name, err)
	}
	klog.V(2).InfoS("shadow: deleted claim", "namespace", namespace, "name", name)
	return nil
}

// DeleteForCompositeClaim deletes all shadow claims owned by a composite claim.
func (m *ClaimManager) DeleteForCompositeClaim(ctx context.Context, namespace, compositeClaimUID string) error {
	labelSelector := fmt.Sprintf("app.kubernetes.io/managed-by=%s,composite-claim-uid=%s", m.driverName, compositeClaimUID)
	list, err := m.client.ResourceClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return fmt.Errorf("list shadow claims for composite %s: %w", compositeClaimUID, err)
	}

	var errs []error
	for _, claim := range list.Items {
		if delErr := m.Delete(ctx, namespace, claim.Name); delErr != nil {
			errs = append(errs, delErr)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to delete %d shadow claims: %v", len(errs), errs)
	}
	return nil
}
