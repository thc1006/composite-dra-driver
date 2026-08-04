// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/openshift-psap/composite-dra-driver/pkg/metrics"
	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

func openStateStore(t *testing.T) *store.StateStore {
	t.Helper()
	ss, err := store.NewStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	return ss
}

func nsObj(uid string) kubeletplugin.NamespacedObject {
	return kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "claim"},
		UID:            types.UID(uid),
	}
}

func newPluginWithFakes(ss *store.StateStore) (*CompositePlugin, *fakePreparer, *fakeClaimMgr) {
	fp := &fakePreparer{}
	fc := &fakeClaimMgr{}
	p := &CompositePlugin{
		grpcClient:   fp,
		claimMgr:     fc,
		stateStore:   ss,
		recorder:     record.NewFakeRecorder(16),
		shadowClaims: make(map[types.UID][]shadowRecord),
	}
	return p, fp, fc
}

// When every shadow unprepares cleanly, unprepareClaim clears both the in-memory record
// and the durable checkpoint.
func TestUnprepareClaimAllSuccessClearsState(t *testing.T) {
	ss := openStateStore(t)
	p, _, fc := newPluginWithFakes(ss)

	const uid = "uid-all"
	shadows := []shadowRecord{createdShadow("drv-a", "s-a", "ua"), createdShadow("drv-b", "s-b", "ub")}
	p.shadowClaims[types.UID(uid)] = shadows
	if err := p.persistShadows(uid, "ns", shadows); err != nil {
		t.Fatalf("persist: %v", err)
	}

	if err := p.unprepareClaim(context.Background(), nsObj(uid)); err != nil {
		t.Fatalf("unprepare: %v", err)
	}

	if _, ok := p.shadowClaims[types.UID(uid)]; ok {
		t.Errorf("in-memory record not cleared after a full unprepare")
	}
	if !sortedEqual(fc.deleted, []string{"s-a", "s-b"}) {
		t.Errorf("deleted = %v, want both shadows deleted", fc.deleted)
	}
	recs, err := ss.ListAll()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("durable checkpoint not cleared: %d records", len(recs))
	}
}

// When one shadow's Unprepare fails, only that shadow survives, in both the in-memory
// map and the durable checkpoint, and unprepareClaim returns an error.
func TestUnprepareClaimPartialFailureKeepsRemaining(t *testing.T) {
	ss := openStateStore(t)
	p, fp, fc := newPluginWithFakes(ss)
	fp.unprepareErr = map[string]error{"s-b": errors.New("socket timeout")}

	const uid = "uid-part"
	shadows := []shadowRecord{createdShadow("drv-a", "s-a", "ua"), createdShadow("drv-b", "s-b", "ub")}
	p.shadowClaims[types.UID(uid)] = shadows
	if err := p.persistShadows(uid, "ns", shadows); err != nil {
		t.Fatalf("persist: %v", err)
	}

	if err := p.unprepareClaim(context.Background(), nsObj(uid)); err == nil {
		t.Fatalf("want an error when a shadow fails to unprepare")
	}

	// The succeeded shadow must actually be torn down, not silently dropped.
	if !sortedEqual(fp.unprepared, []string{"s-a", "s-b"}) {
		t.Errorf("unprepared = %v, want both attempted", fp.unprepared)
	}
	if !sortedEqual(fc.deleted, []string{"s-a"}) {
		t.Errorf("deleted = %v, want only the succeeded s-a", fc.deleted)
	}

	got := p.shadowClaims[types.UID(uid)]
	if len(got) != 1 || got[0].info.Name != "s-b" {
		t.Errorf("in-memory remaining = %v, want only s-b", got)
	}
	recs, err := ss.ListAll()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Shadows) != 1 || recs[0].Shadows[0].Name != "s-b" {
		t.Errorf("durable checkpoint = %+v, want only s-b", recs)
	}
}

// A Delete failure (the Unprepare having succeeded) also keeps the shadow, so a retry
// can re-run the idempotent delete.
func TestUnprepareClaimDeleteFailureKeepsShadow(t *testing.T) {
	ss := openStateStore(t)
	p, _, fc := newPluginWithFakes(ss)
	fc.deleteErr = map[string]error{"s-a": errors.New("conflict")}

	const uid = "uid-del"
	shadows := []shadowRecord{createdShadow("drv-a", "s-a", "ua"), createdShadow("drv-b", "s-b", "ub")}
	p.shadowClaims[types.UID(uid)] = shadows
	if err := p.persistShadows(uid, "ns", shadows); err != nil {
		t.Fatalf("persist: %v", err)
	}

	if err := p.unprepareClaim(context.Background(), nsObj(uid)); err == nil {
		t.Fatalf("want an error when a shadow fails to delete")
	}
	// s-b tore down cleanly; only s-a (Delete failed) is kept, in the map and durably.
	if !sortedEqual(fc.deleted, []string{"s-a", "s-b"}) {
		t.Errorf("deleted = %v, want both Deletes attempted", fc.deleted)
	}
	got := p.shadowClaims[types.UID(uid)]
	if len(got) != 1 || got[0].info.Name != "s-a" {
		t.Errorf("in-memory remaining = %v, want only s-a", got)
	}
	recs, err := ss.ListAll()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Shadows) != 1 || recs[0].Shadows[0].Name != "s-a" {
		t.Errorf("durable checkpoint = %+v, want only s-a", recs)
	}
}

// With no in-memory record, unprepareClaim still lists the shadows on the API server
// and unprepares each before deleting, so a shadow kept by a Prepare rollback (or
// surviving a restart with no checkpoint) releases its underlying resource rather than
// being deleted with a dangling preparation.
func TestUnprepareClaimFallbackUnpreparesBeforeDelete(t *testing.T) {
	ss := openStateStore(t)
	p, fp, fc := newPluginWithFakes(ss)
	fc.listForComposite = []shadow.AdoptedShadow{
		{Info: shadow.ShadowClaimInfo{Namespace: "ns", Name: "orphan-a", UID: "oa"}, Driver: "drv-a"},
	}

	// No entry in p.shadowClaims for this uid, so the len(shadows)==0 fallback runs.
	if err := p.unprepareClaim(context.Background(), nsObj("uid-orphan")); err != nil {
		t.Fatalf("unprepare: %v", err)
	}
	if !sortedEqual(fp.unprepared, []string{"orphan-a"}) {
		t.Errorf("unprepared = %v, want orphan-a unprepared before delete", fp.unprepared)
	}
	if !sortedEqual(fc.deleted, []string{"orphan-a"}) {
		t.Errorf("deleted = %v, want orphan-a deleted", fc.deleted)
	}
}

// A fallback shadow whose Unprepare fails is kept (not deleted) and the error surfaces.
func TestUnprepareClaimFallbackKeepsShadowOnUnprepareFailure(t *testing.T) {
	ss := openStateStore(t)
	p, fp, fc := newPluginWithFakes(ss)
	fp.unprepareErr = map[string]error{"orphan-a": errors.New("socket timeout")}
	fc.listForComposite = []shadow.AdoptedShadow{
		{Info: shadow.ShadowClaimInfo{Namespace: "ns", Name: "orphan-a", UID: "oa"}, Driver: "drv-a"},
	}

	if err := p.unprepareClaim(context.Background(), nsObj("uid-orphan")); err == nil {
		t.Fatalf("want an error when a fallback shadow fails to unprepare")
	}
	if !sortedEqual(fp.unprepared, []string{"orphan-a"}) {
		t.Errorf("unprepared = %v, want orphan-a attempted", fp.unprepared)
	}
	if len(fc.deleted) != 0 {
		t.Errorf("deleted = %v, want none (shadow kept after a failed Unprepare)", fc.deleted)
	}
}

// The active gauges are recomputed from the authoritative map, so a restart that
// repopulates the map while the gauges are zero and then unprepares does not drive them
// negative.
func TestActiveMetricsRecomputeDoesNotGoNegative(t *testing.T) {
	const comp = "comp-neg-test"
	mk := func(name, uid string) shadowRecord {
		return shadowRecord{driverName: "drv", composition: comp, info: &shadow.ShadowClaimInfo{Namespace: "ns", Name: name, UID: uid}, created: true}
	}

	ss := openStateStore(t)
	seed, _, _ := newPluginWithFakes(ss)
	if err := seed.persistShadows("uid-neg", "ns", []shadowRecord{mk("s-a", "ua"), mk("s-b", "ub")}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// A fresh plugin starts with the gauges effectively zero for this composition;
	// restoreFromState rebuilds the map and must Set the gauges to match.
	p, _, _ := newPluginWithFakes(ss)
	p.restoreFromState()
	if got := testutil.ToFloat64(metrics.ShadowClaimsActive.WithLabelValues(comp)); got != 2 {
		t.Fatalf("after restore, shadow gauge = %v, want 2", got)
	}

	if err := p.unprepareClaim(context.Background(), nsObj("uid-neg")); err != nil {
		t.Fatalf("unprepare: %v", err)
	}
	if got := testutil.ToFloat64(metrics.ShadowClaimsActive.WithLabelValues(comp)); got != 0 {
		t.Errorf("after unprepare, shadow gauge = %v, want 0 (must not go negative)", got)
	}
	if got := testutil.ToFloat64(metrics.ClaimsActive.WithLabelValues(comp)); got != 0 {
		t.Errorf("after unprepare, claim gauge = %v, want 0", got)
	}
}
