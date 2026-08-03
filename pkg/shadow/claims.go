// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package shadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
	"k8s.io/klog/v2"

	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

// shadowClaimName generates a deterministic name for a shadow claim.
// The composite claim name is hashed to a short prefix, leaving room for the
// sourceName+device suffix within the 63-char limit. The name is deliberately
// independent of the composite claim UID so it stays stable across a driver
// restart; ownership is checked separately when an existing shadow is adopted.
func shadowClaimName(compositeClaimName string, member *store.DeviceMember) string {
	h := sha256.Sum256([]byte(compositeClaimName))
	prefix := hex.EncodeToString(h[:4])
	name := fmt.Sprintf("shadow-%s-%s-%s", prefix, member.SourceName, member.Device)
	if len(name) > 63 {
		name = name[:63]
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
		_ = m.Delete(ctx, &ShadowClaimInfo{Namespace: compositeClaim.Namespace, Name: shadowName, UID: string(created.UID)})
		return nil, fmt.Errorf("update shadow claim status %s: %w", shadowName, err)
	}

	klog.V(2).InfoS("shadow: created claim", "namespace", updated.Namespace, "name", updated.Name, "uid", updated.UID, "driver", member.Driver, "pool", member.Pool, "device", member.Device)

	return &ShadowClaimInfo{
		Namespace: updated.Namespace,
		Name:      updated.Name,
		UID:       string(updated.UID),
	}, nil
}

// validateShadowOwnership reports whether an existing shadow claim belongs to the
// given composite claim. The deterministic name is not proof of identity: a
// composite claim deleted and re-created under the same name reuses the name but
// gets a new UID, so a shadow left from the previous incarnation must not be
// adopted as an idempotent Prepare. Every mismatch fails closed.
func validateShadowOwnership(shadow, compositeClaim *resourceapi.ResourceClaim, member *store.DeviceMember) error {
	if shadow.DeletionTimestamp != nil {
		return fmt.Errorf("shadow claim %s is terminating", shadow.Name)
	}
	if got := shadow.Labels["composite-claim-uid"]; got != string(compositeClaim.UID) {
		return fmt.Errorf("shadow claim %s belongs to composite claim UID %q, not %q", shadow.Name, got, compositeClaim.UID)
	}
	owned := false
	for _, ref := range shadow.OwnerReferences {
		if ref.UID == compositeClaim.UID {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("shadow claim %s is not owned by composite claim %s", shadow.Name, compositeClaim.UID)
	}
	// The name is a hash and can collide, so confirm the shadow actually allocates
	// this member's underlying device before adopting it.
	if !shadowAllocatesMember(shadow, member) {
		return fmt.Errorf("shadow claim %s does not allocate %s/%s/%s", shadow.Name, member.Driver, member.Pool, member.Device)
	}
	return nil
}

// shadowAllocatesMember reports whether the shadow's status allocation targets the
// given underlying device.
func shadowAllocatesMember(shadow *resourceapi.ResourceClaim, member *store.DeviceMember) bool {
	if shadow.Status.Allocation == nil {
		return false
	}
	for _, r := range shadow.Status.Allocation.Devices.Results {
		if r.Driver == member.Driver && r.Pool == member.Pool && r.Device == member.Device {
			return true
		}
	}
	return false
}

// Get fetches an existing shadow claim and confirms it belongs to the given
// composite claim before it is adopted as an idempotent Prepare.
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

	if err := validateShadowOwnership(existing, compositeClaim, member); err != nil {
		return nil, err
	}

	return &ShadowClaimInfo{
		Namespace: existing.Namespace,
		Name:      existing.Name,
		UID:       string(existing.UID),
	}, nil
}

// DeleteExact deletes a shadow ResourceClaim only if it still has the expected UID,
// so a same-named replacement created by a newer claim incarnation is preserved.
// NotFound, or a different UID at the same name, means the exact target is gone.
func DeleteExact(ctx context.Context, client resourceclient.ResourceV1Interface, namespace, name, uid string) error {
	if uid == "" {
		return fmt.Errorf("delete shadow claim %s/%s: missing expected UID", namespace, name)
	}
	expected := types.UID(uid)
	err := client.ResourceClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &expected},
	})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	// The delete was rejected (a UID precondition conflict, or a transient error).
	// If the exact target is already gone (NotFound, or the name now holds a
	// different UID), treat it as done; otherwise propagate. This does not depend
	// on the precise error type the API server returns for a precondition failure.
	current, getErr := client.ResourceClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) || (getErr == nil && string(current.UID) != uid) {
		return nil
	}
	return fmt.Errorf("delete shadow claim %s/%s uid %s: %w", namespace, name, uid, err)
}

// Delete removes a shadow ResourceClaim, guarding on its recorded UID so a
// same-named replacement is never deleted by mistake.
func (m *ClaimManager) Delete(ctx context.Context, info *ShadowClaimInfo) error {
	if info == nil {
		return fmt.Errorf("delete shadow claim: nil info")
	}
	if err := DeleteExact(ctx, m.client, info.Namespace, info.Name, info.UID); err != nil {
		return err
	}
	klog.V(2).InfoS("shadow: deleted claim", "namespace", info.Namespace, "name", info.Name, "uid", info.UID)
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
		if delErr := m.Delete(ctx, &ShadowClaimInfo{Namespace: namespace, Name: claim.Name, UID: string(claim.UID)}); delErr != nil {
			errs = append(errs, delErr)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to delete %d shadow claims: %v", len(errs), errs)
	}
	return nil
}
