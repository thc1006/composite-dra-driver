// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

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
