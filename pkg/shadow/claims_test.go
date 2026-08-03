// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package shadow

import (
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

// A shadow claim is adopted for an idempotent Prepare only when it belongs to the
// composite claim asking for it and allocates the member being prepared. A shadow
// from a same-named but deleted claim (different UID), one whose name collided with
// a different underlying device, or a terminating one must be rejected.
func TestValidateShadowOwnership(t *testing.T) {
	const parentUID = "uid-parent"
	parent := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-claim", UID: types.UID(parentUID)},
	}
	member := &store.DeviceMember{Driver: "gpu.example", Pool: "pool-a", Device: "gpu0"}
	shadowFor := func(uid string, m *store.DeviceMember) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "shadow-abc-gpu-gpu0",
				Labels:          map[string]string{"composite-claim-uid": uid},
				OwnerReferences: []metav1.OwnerReference{{Kind: "ResourceClaim", UID: types.UID(uid)}},
			},
			Status: resourceapi.ResourceClaimStatus{
				Allocation: &resourceapi.AllocationResult{
					Devices: resourceapi.DeviceAllocationResult{
						Results: []resourceapi.DeviceRequestAllocationResult{
							{Driver: m.Driver, Pool: m.Pool, Device: m.Device},
						},
					},
				},
			},
		}
	}

	if err := validateShadowOwnership(shadowFor(parentUID, member), parent, member); err != nil {
		t.Fatalf("shadow owned by the same claim and allocating this member must be adopted, got error: %v", err)
	}

	if err := validateShadowOwnership(shadowFor("uid-stale", member), parent, member); err == nil {
		t.Fatal("shadow from a previous incarnation (different UID) must be rejected, got nil")
	}

	other := &store.DeviceMember{Driver: "gpu.example", Pool: "pool-a", Device: "gpu1"}
	if err := validateShadowOwnership(shadowFor(parentUID, other), parent, member); err == nil {
		t.Fatal("shadow whose name collided with a different device must be rejected, got nil")
	}

	terminating := shadowFor(parentUID, member)
	deleteTime := metav1.Now()
	terminating.DeletionTimestamp = &deleteTime
	if err := validateShadowOwnership(terminating, parent, member); err == nil {
		t.Fatal("terminating shadow must be rejected, got nil")
	}
}
