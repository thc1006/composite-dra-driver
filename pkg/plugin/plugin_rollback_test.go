// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1"

	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

// fakePreparer records prepare/unprepare calls and returns configurable errors keyed
// by shadow claim name. It satisfies shadowPreparer.
type fakePreparer struct {
	mu           sync.Mutex
	unprepareErr map[string]error
	prepared     []string
	unprepared   []string
}

func (f *fakePreparer) Prepare(_ context.Context, _ string, claim *shadow.ShadowClaimInfo) (*drapbv1.NodePrepareResourceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, claim.Name)
	return &drapbv1.NodePrepareResourceResponse{}, nil
}

func (f *fakePreparer) Unprepare(_ context.Context, _ string, claim *shadow.ShadowClaimInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unprepared = append(f.unprepared, claim.Name)
	return f.unprepareErr[claim.Name]
}

// fakeClaimMgr records create and delete calls and returns configurable errors keyed
// by shadow claim name. It satisfies shadowClaimManager.
type fakeClaimMgr struct {
	mu               sync.Mutex
	deleteErr        map[string]error
	created          []string
	deleted          []string
	listForComposite []shadow.AdoptedShadow
}

func (f *fakeClaimMgr) Create(_ context.Context, _ *resourceapi.ResourceClaim, member *store.DeviceMember, _ string, _ []byte) (*shadow.ShadowClaimInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, member.Device)
	return &shadow.ShadowClaimInfo{Namespace: "ns", Name: "shadow-" + member.Device, UID: "uid-" + member.Device}, nil
}

func (f *fakeClaimMgr) Get(context.Context, *resourceapi.ResourceClaim, *store.DeviceMember, string) (*shadow.ShadowClaimInfo, error) {
	return nil, nil
}

func (f *fakeClaimMgr) Delete(_ context.Context, info *shadow.ShadowClaimInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, info.Name)
	return f.deleteErr[info.Name]
}

func (f *fakeClaimMgr) ListForCompositeClaim(_ context.Context, _ string, _ string) ([]shadow.AdoptedShadow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listForComposite, nil
}

func createdShadow(driver, name, uid string) shadowRecord {
	return shadowRecord{
		driverName:  driver,
		composition: "comp",
		info:        &shadow.ShadowClaimInfo{Namespace: "ns", Name: name, UID: uid},
		created:     true,
	}
}

func sortedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// Phase 2 rollback unprepares every created shadow and deletes only those that
// unprepared cleanly. A shadow whose Unprepare fails is kept so a Prepare retry can
// adopt it and drive its teardown again, instead of leaking the underlying resource
// with no handle left to unprepare it (#64).
func TestCleanupShadowsPhase2KeepsShadowOnUnprepareFailure(t *testing.T) {
	fp := &fakePreparer{unprepareErr: map[string]error{"shadow-bad": errors.New("socket timeout")}}
	fc := &fakeClaimMgr{}
	p := &CompositePlugin{grpcClient: fp, claimMgr: fc}

	shadows := []shadowRecord{
		createdShadow("drv-a", "shadow-good", "uid-good"),
		createdShadow("drv-b", "shadow-bad", "uid-bad"),
	}
	p.cleanupShadows(context.Background(), shadows, true)

	if !sortedEqual(fp.unprepared, []string{"shadow-good", "shadow-bad"}) {
		t.Errorf("unprepared = %v, want both shadows attempted", fp.unprepared)
	}
	if !sortedEqual(fc.deleted, []string{"shadow-good"}) {
		t.Errorf("deleted = %v, want only shadow-good; shadow-bad must be kept after a failed Unprepare", fc.deleted)
	}
}

// Phase 1 rollback deletes created shadows directly, without an unprepare, because no
// gRPC Prepare has run yet.
func TestCleanupShadowsPhase1DeletesWithoutUnprepare(t *testing.T) {
	fp := &fakePreparer{}
	fc := &fakeClaimMgr{}
	p := &CompositePlugin{grpcClient: fp, claimMgr: fc}

	p.cleanupShadows(context.Background(), []shadowRecord{createdShadow("drv-a", "shadow-a", "uid-a")}, false)

	if len(fp.unprepared) != 0 {
		t.Errorf("phase 1 rollback called Unprepare on %v, want none", fp.unprepared)
	}
	if !sortedEqual(fc.deleted, []string{"shadow-a"}) {
		t.Errorf("deleted = %v, want shadow-a", fc.deleted)
	}
}

// A rollback batch that mixes an adopted shadow (created=false, possibly in use by a
// running workload) with created shadows tears down only the created ones: the adopted
// shadow is neither unprepared nor deleted, a created shadow that unprepares cleanly is
// deleted, and one whose Unprepare fails is kept. The mixed input keeps this from
// passing vacuously if cleanup were a no-op.
func TestCleanupShadowsPhase2MixedAdoptedAndCreated(t *testing.T) {
	fp := &fakePreparer{unprepareErr: map[string]error{"shadow-bad": errors.New("socket timeout")}}
	fc := &fakeClaimMgr{}
	p := &CompositePlugin{grpcClient: fp, claimMgr: fc}

	adopted := shadowRecord{driverName: "drv-x", composition: "comp", info: &shadow.ShadowClaimInfo{Namespace: "ns", Name: "shadow-adopted", UID: "uid-x"}}
	shadows := []shadowRecord{
		adopted,
		createdShadow("drv-a", "shadow-good", "uid-good"),
		createdShadow("drv-b", "shadow-bad", "uid-bad"),
	}
	p.cleanupShadows(context.Background(), shadows, true)

	if !sortedEqual(fp.unprepared, []string{"shadow-good", "shadow-bad"}) {
		t.Errorf("unprepared = %v, want only the two created shadows (adopted must be skipped)", fp.unprepared)
	}
	if !sortedEqual(fc.deleted, []string{"shadow-good"}) {
		t.Errorf("deleted = %v, want only shadow-good", fc.deleted)
	}
}

// A Delete failure on one created shadow must not abort the rollback of the others.
func TestCleanupShadowsPhase2ContinuesPastDeleteFailure(t *testing.T) {
	fp := &fakePreparer{}
	fc := &fakeClaimMgr{deleteErr: map[string]error{"shadow-a": errors.New("conflict")}}
	p := &CompositePlugin{grpcClient: fp, claimMgr: fc}

	shadows := []shadowRecord{
		createdShadow("drv-a", "shadow-a", "uid-a"),
		createdShadow("drv-b", "shadow-b", "uid-b"),
	}
	p.cleanupShadows(context.Background(), shadows, true)

	if !sortedEqual(fp.unprepared, []string{"shadow-a", "shadow-b"}) {
		t.Errorf("unprepared = %v, want both", fp.unprepared)
	}
	if !sortedEqual(fc.deleted, []string{"shadow-a", "shadow-b"}) {
		t.Errorf("deleted = %v, want both Deletes attempted despite the first failing", fc.deleted)
	}
}
