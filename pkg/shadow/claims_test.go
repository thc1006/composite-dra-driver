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
	const driverName = "composite.example"
	const requestName = "req-0"
	parent := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-claim", UID: types.UID(parentUID)},
	}
	member := &store.DeviceMember{Driver: "gpu.example", Pool: "pool-a", Device: "gpu0"}
	shadowFor := func(uid string, m *store.DeviceMember) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: "shadow-abc-gpu-gpu0",
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": driverName,
					"composite-claim-uid":          uid,
				},
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "resource.k8s.io/v1", Kind: "ResourceClaim", Name: "my-claim", UID: types.UID(uid)},
				},
			},
			Status: resourceapi.ResourceClaimStatus{
				Allocation: &resourceapi.AllocationResult{
					Devices: resourceapi.DeviceAllocationResult{
						Results: []resourceapi.DeviceRequestAllocationResult{
							{Request: requestName, Driver: m.Driver, Pool: m.Pool, Device: m.Device},
						},
					},
				},
			},
		}
	}

	if err := validateShadowOwnership(shadowFor(parentUID, member), parent, member, driverName, requestName); err != nil {
		t.Fatalf("shadow owned by the same claim and allocating this member must be adopted, got error: %v", err)
	}

	if err := validateShadowOwnership(shadowFor("uid-stale", member), parent, member, driverName, requestName); err == nil {
		t.Fatal("shadow from a previous incarnation (different UID) must be rejected, got nil")
	}

	other := &store.DeviceMember{Driver: "gpu.example", Pool: "pool-a", Device: "gpu1"}
	if err := validateShadowOwnership(shadowFor(parentUID, other), parent, member, driverName, requestName); err == nil {
		t.Fatal("shadow whose name collided with a different device must be rejected, got nil")
	}

	terminating := shadowFor(parentUID, member)
	deleteTime := metav1.Now()
	terminating.DeletionTimestamp = &deleteTime
	if err := validateShadowOwnership(terminating, parent, member, driverName, requestName); err == nil {
		t.Fatal("terminating shadow must be rejected, got nil")
	}

	wrongName := shadowFor(parentUID, member)
	wrongName.OwnerReferences[0].Name = "someone-else"
	if err := validateShadowOwnership(wrongName, parent, member, driverName, requestName); err == nil {
		t.Fatal("shadow whose owner-reference name does not match the parent must be rejected, got nil")
	}

	wrongManagedBy := shadowFor(parentUID, member)
	wrongManagedBy.Labels["app.kubernetes.io/managed-by"] = "other-driver"
	if err := validateShadowOwnership(wrongManagedBy, parent, member, driverName, requestName); err == nil {
		t.Fatal("shadow managed by a different driver must be rejected, got nil")
	}

	// Same device, different request: the #69 overlap must not let this shadow be adopted
	// for req-0 when it was allocated for another request.
	wrongRequest := shadowFor(parentUID, member)
	wrongRequest.Status.Allocation.Devices.Results[0].Request = "req-1"
	if err := validateShadowOwnership(wrongRequest, parent, member, driverName, requestName); err == nil {
		t.Fatal("shadow allocated for a different request must be rejected, got nil")
	}

	twoResults := shadowFor(parentUID, member)
	twoResults.Status.Allocation.Devices.Results = append(twoResults.Status.Allocation.Devices.Results,
		resourceapi.DeviceRequestAllocationResult{Request: requestName, Driver: member.Driver, Pool: member.Pool, Device: member.Device})
	if err := validateShadowOwnership(twoResults, parent, member, driverName, requestName); err == nil {
		t.Fatal("shadow with more than one allocation result must be rejected, got nil")
	}
}
