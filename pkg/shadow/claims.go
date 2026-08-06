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
				Source:   resourceapi.AllocationConfigSourceClaim,
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
func validateShadowOwnership(shadow, compositeClaim *resourceapi.ResourceClaim, member *store.DeviceMember, driverName, requestName string) error {
	if shadow.DeletionTimestamp != nil {
		return fmt.Errorf("shadow claim %s is terminating", shadow.Name)
	}
	if got := shadow.Labels["app.kubernetes.io/managed-by"]; got != driverName {
		return fmt.Errorf("shadow claim %s is not managed by %q (got %q)", shadow.Name, driverName, got)
	}
	if got := shadow.Labels["composite-claim-uid"]; got != string(compositeClaim.UID) {
		return fmt.Errorf("shadow claim %s belongs to composite claim UID %q, not %q", shadow.Name, got, compositeClaim.UID)
	}
	// The reconciler resolves the parent through the owner reference's name, so the whole
	// reference must match, not just the UID; a shadow with the right UID but a wrong
	// name would be adopted here and then treated as an orphan there.
	owned := false
	for _, ref := range shadow.OwnerReferences {
		if ref.APIVersion == "resource.k8s.io/v1" && ref.Kind == "ResourceClaim" &&
			ref.Name == compositeClaim.Name && ref.UID == compositeClaim.UID {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("shadow claim %s is not owned by composite claim %s/%s", shadow.Name, compositeClaim.Name, compositeClaim.UID)
	}
	// The name is a hash and can collide, so confirm the shadow actually allocates this
	// member's underlying device for this request before adopting it.
	if !shadowAllocatesMember(shadow, member, requestName) {
		return fmt.Errorf("shadow claim %s does not allocate request %q for %s/%s/%s", shadow.Name, requestName, member.Driver, member.Pool, member.Device)
	}
	return nil
}

// shadowAllocatesMember reports whether the shadow's status allocation targets exactly
// this member's underlying device for this request. Binding the request name, and
// requiring a single result, keeps two composite requests that resolve to the same
// underlying device (the overlapping pairs in #69) from sharing one shadow.
func shadowAllocatesMember(shadow *resourceapi.ResourceClaim, member *store.DeviceMember, requestName string) bool {
	if shadow.Status.Allocation == nil {
		return false
	}
	results := shadow.Status.Allocation.Devices.Results
	if len(results) != 1 {
		return false
	}
	r := results[0]
	return r.Request == requestName &&
		r.Driver == member.Driver && r.Pool == member.Pool && r.Device == member.Device
}

// Get fetches an existing shadow claim and confirms it belongs to the given
// composite claim before it is adopted as an idempotent Prepare.
func (m *ClaimManager) Get(
	ctx context.Context,
	compositeClaim *resourceapi.ResourceClaim,
	member *store.DeviceMember,
	requestName string,
) (*ShadowClaimInfo, error) {
	shadowName := shadowClaimName(compositeClaim.Name, member)

	existing, err := m.client.ResourceClaims(compositeClaim.Namespace).Get(ctx, shadowName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get shadow claim %s: %w", shadowName, err)
	}

	if err := validateShadowOwnership(existing, compositeClaim, member, m.driverName, requestName); err != nil {
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

// AdoptedShadow identifies a shadow claim found on the API server together with the
// underlying driver it was prepared on, so a caller can unprepare it before deleting.
type AdoptedShadow struct {
	Info   ShadowClaimInfo
	Driver string
	// HasAllocation is true when the shadow carries a status allocation. A shadow with
	// no allocation was never prepared on an underlying driver, so it holds no resource
	// and can be deleted directly rather than unprepared.
	HasAllocation bool
}

// ListForCompositeClaim returns the shadow claims owned by a composite claim, each with
// the underlying driver taken from its allocation. The caller uses this to tear down
// shadows the plugin no longer tracks in memory (a Prepare rollback kept them, or they
// outlived a restart that never checkpointed them): unprepare each before deleting so
// the underlying resource is released, not just the shadow object.
func (m *ClaimManager) ListForCompositeClaim(ctx context.Context, namespace, compositeClaimUID string) ([]AdoptedShadow, error) {
	labelSelector := fmt.Sprintf("app.kubernetes.io/managed-by=%s,composite-claim-uid=%s", m.driverName, compositeClaimUID)
	list, err := m.client.ResourceClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("list shadow claims for composite %s: %w", compositeClaimUID, err)
	}

	out := make([]AdoptedShadow, 0, len(list.Items))
	for i := range list.Items {
		claim := &list.Items[i]
		hasAlloc := claim.Status.Allocation != nil && len(claim.Status.Allocation.Devices.Results) > 0
		driver := ""
		if hasAlloc {
			driver = claim.Status.Allocation.Devices.Results[0].Driver
		}
		out = append(out, AdoptedShadow{
			Info:          ShadowClaimInfo{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
			Driver:        driver,
			HasAllocation: hasAlloc,
		})
	}
	return out, nil
}
