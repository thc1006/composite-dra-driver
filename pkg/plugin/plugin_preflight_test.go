// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

func work(compositePool, compositeDevice, driver, pool, device string) *memberWork {
	return &memberWork{
		member:      store.DeviceMember{Driver: driver, Pool: pool, Device: device},
		allocResult: resourceapi.DeviceRequestAllocationResult{Pool: compositePool, Device: compositeDevice},
	}
}

func TestRejectDuplicateMembersDistinctMembers(t *testing.T) {
	// Two composite devices, each with its own underlying member: allowed.
	w := []*memberWork{
		work("cpool", "comp0", "gpu.example", "pool-a", "gpu0"),
		work("cpool", "comp0", "nic.example", "pool-b", "nic0"),
		work("cpool", "comp1", "gpu.example", "pool-a", "gpu1"),
		work("cpool", "comp1", "nic.example", "pool-b", "nic1"),
	}
	if err := rejectDuplicateMembers(w); err != nil {
		t.Fatalf("distinct members should be accepted, got: %v", err)
	}
}

func TestRejectDuplicateMembersSharedUnderlyingDevice(t *testing.T) {
	// comp0 and comp1 both contain gpu0. One ExactCount request allocating two
	// composite devices that overlap on gpu0 must be rejected before any shadow
	// is created, otherwise the second work item adopts the first's shadow.
	w := []*memberWork{
		work("cpool", "comp0", "gpu.example", "pool-a", "gpu0"),
		work("cpool", "comp0", "nic.example", "pool-b", "nic0"),
		work("cpool", "comp1", "gpu.example", "pool-a", "gpu0"),
		work("cpool", "comp1", "nic.example", "pool-b", "nic1"),
	}
	err := rejectDuplicateMembers(w)
	if err == nil {
		t.Fatal("a shared underlying member must be rejected")
	}
	for _, want := range []string{"comp0", "comp1", "gpu.example", "pool-a", "gpu0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got: %v", want, err)
		}
	}
}

func TestRejectDuplicateMembersEmpty(t *testing.T) {
	if err := rejectDuplicateMembers(nil); err != nil {
		t.Fatalf("empty work list should be accepted, got: %v", err)
	}
}

// A shared underlying member must be caught before prepareClaim creates or prepares
// any shadow. Driving the whole method, not just the helper, pins the guard ahead of
// the side effects, so a later reordering of the create and prepare phases cannot slip
// a Create in before the check.
func TestPrepareClaimRejectsDuplicateBeforeSideEffects(t *testing.T) {
	const driverName = "composite.example"

	// comp0 and comp1 both resolve to gpu0, so the two composite devices overlap on
	// one underlying member.
	ds := store.NewDeviceStore()
	ds.Put("cpool", "comp0", &store.DeviceMapping{
		CompositionName: "gpu-nic",
		Members: []store.DeviceMember{
			{SourceName: "gpu", Driver: "gpu.example", Pool: "pool-a", Device: "gpu0"},
			{SourceName: "nic", Driver: "nic.example", Pool: "pool-b", Device: "nic0"},
		},
	})
	ds.Put("cpool", "comp1", &store.DeviceMapping{
		CompositionName: "gpu-nic",
		Members: []store.DeviceMember{
			{SourceName: "gpu", Driver: "gpu.example", Pool: "pool-a", Device: "gpu0"},
			{SourceName: "nic", Driver: "nic.example", Pool: "pool-b", Device: "nic1"},
		},
	})

	fp := &fakePreparer{}
	fc := &fakeClaimMgr{}
	p := &CompositePlugin{
		driverName:   driverName,
		deviceStore:  ds,
		claimMgr:     fc,
		grpcClient:   fp,
		recorder:     record.NewFakeRecorder(10),
		shadowClaims: map[types.UID][]shadowRecord{},
	}

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "my-claim", UID: types.UID("uid-parent")},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Request: "req-0", Driver: driverName, Pool: "cpool", Device: "comp0"},
						{Request: "req-1", Driver: driverName, Pool: "cpool", Device: "comp1"},
					},
				},
			},
		},
	}

	_, err := p.prepareClaim(context.Background(), claim)
	if err == nil {
		t.Fatal("prepareClaim must reject a claim whose composite devices share an underlying member")
	}
	if !strings.Contains(err.Error(), "share underlying member") {
		t.Fatalf("rejection must come from the duplicate-member guard, got: %v", err)
	}
	if len(fc.created) != 0 {
		t.Errorf("no shadow claim should be created when the preflight rejects, got %v", fc.created)
	}
	if len(fp.prepared) != 0 {
		t.Errorf("no shadow should be prepared when the preflight rejects, got %v", fp.prepared)
	}
}
