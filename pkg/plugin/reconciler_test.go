// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"errors"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// The reconciler must delete a shadow only when its parent is truly gone. A
// same-named parent with a different UID is a replacement (orphan), NotFound is
// an orphan, but a transient API error must leave the shadow alone this cycle.
// A managed shadow whose owner reference no longer points at its labeled parent
// (empty or replaced) must be detected so the reconciler reclaims it instead of
// keeping it forever while adoption rejects it.
func TestOwnerRefMatches(t *testing.T) {
	refs := []metav1.OwnerReference{{Kind: "ResourceClaim", UID: types.UID("parent-a")}}
	if !ownerRefMatches(refs, "parent-a") {
		t.Fatal("matching UID: want true")
	}
	if ownerRefMatches(refs, "parent-b") {
		t.Fatal("different UID: want false")
	}
	if ownerRefMatches(nil, "parent-a") {
		t.Fatal("no owner references: want false")
	}
}

func TestClassifyOwner(t *testing.T) {
	const uid = "uid-parent"
	owner := func(u string) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID(u)}}
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "resourceclaims"}, "my-claim")

	tests := []struct {
		name  string
		owner *resourceapi.ResourceClaim
		err   error
		want  ownerStatus
	}{
		{"alive", owner(uid), nil, ownerAlive},
		{"replaced by same name different UID", owner("uid-new"), nil, ownerGone},
		{"not found", nil, notFound, ownerGone},
		{"transient error is not deletion", nil, errors.New("etcdserver: request timed out"), ownerUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyOwner(tt.owner, tt.err, uid); got != tt.want {
				t.Fatalf("classifyOwner = %v, want %v", got, tt.want)
			}
		})
	}
}
