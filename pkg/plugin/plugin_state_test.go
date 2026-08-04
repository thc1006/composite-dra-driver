// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

func newShadowRecord(driver, composition, ns, name, uid string) shadowRecord {
	return shadowRecord{
		driverName:  driver,
		composition: composition,
		info:        &shadow.ShadowClaimInfo{Namespace: ns, Name: name, UID: uid},
	}
}

// A partial unprepare persists only the shadows that still need teardown, so a
// restart recovers exactly those and re-drives them -- not the ones already torn
// down, and not the stale full set left from Prepare. Deleting the checkpoint on
// full success leaves nothing to recover. This is the durable half of keeping the
// recovery state on a partial failure.
func TestUnprepareRemainingSurvivesRestart(t *testing.T) {
	const uid = "composite-uid-1"
	path := filepath.Join(t.TempDir(), "state.db")

	ss, err := store.NewStateStore(path)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	p := &CompositePlugin{stateStore: ss, shadowClaims: make(map[types.UID][]shadowRecord)}

	// Prepare persisted the full set of three shadows.
	full := []shadowRecord{
		newShadowRecord("drv-a", "comp", "ns", "shadow-a", "uid-a"),
		newShadowRecord("drv-b", "comp", "ns", "shadow-b", "uid-b"),
		newShadowRecord("drv-c", "comp", "ns", "shadow-c", "uid-c"),
	}
	if err := p.persistShadows(uid, "ns", full); err != nil {
		t.Fatalf("persist full: %v", err)
	}

	// Unprepare tore down two; only the failed one remains and is re-persisted,
	// overwriting the full set on disk.
	remaining := []shadowRecord{newShadowRecord("drv-b", "comp", "ns", "shadow-b", "uid-b")}
	if err := p.persistShadows(uid, "ns", remaining); err != nil {
		t.Fatalf("persist remaining: %v", err)
	}
	if err := ss.Close(); err != nil {
		t.Fatalf("close state store: %v", err)
	}

	// Restart: a fresh plugin must recover exactly the remaining shadow.
	ss2, err := store.NewStateStore(path)
	if err != nil {
		t.Fatalf("reopen state store: %v", err)
	}
	p2 := NewCompositePlugin("driver", nil, nil, nil, nil, ss2, nil)
	got := p2.shadowClaims[types.UID(uid)]
	if len(got) != 1 {
		t.Fatalf("restored %d shadows, want 1 (only the remaining one)", len(got))
	}
	if got[0].info.Name != "shadow-b" || got[0].info.UID != "uid-b" || got[0].driverName != "drv-b" {
		t.Fatalf("restored wrong shadow: %+v", got[0].info)
	}

	// Deleting the checkpoint on full success clears it; a later restart recovers nothing.
	if err := p2.deleteShadowState(uid); err != nil {
		t.Fatalf("delete shadow state: %v", err)
	}
	if err := ss2.Close(); err != nil {
		t.Fatalf("close state store 2: %v", err)
	}
	ss3, err := store.NewStateStore(path)
	if err != nil {
		t.Fatalf("reopen state store 3: %v", err)
	}
	defer ss3.Close()
	p3 := NewCompositePlugin("driver", nil, nil, nil, nil, ss3, nil)
	if got := p3.shadowClaims[types.UID(uid)]; len(got) != 0 {
		t.Fatalf("after delete, restored %d shadows, want 0", len(got))
	}
}
