// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
	"k8s.io/klog/v2"

	"github.com/openshift-psap/composite-dra-driver/pkg/metrics"
	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
)

type ownerStatus int

const (
	ownerAlive   ownerStatus = iota // parent exists with the expected UID
	ownerGone                       // parent is gone, or replaced by a same-named claim
	ownerUnknown                    // lookup failed transiently; leave the shadow alone
)

// classifyOwner decides a shadow's fate from the result of fetching its parent
// composite claim. A same-named parent with a different UID means the original
// owner was deleted and replaced, so the shadow is an orphan. A transient error
// is not proof of deletion and must not trigger a delete.
func classifyOwner(owner *resourceapi.ResourceClaim, err error, expectedUID string) ownerStatus {
	switch {
	case err == nil && string(owner.UID) == expectedUID:
		return ownerAlive
	case err == nil, apierrors.IsNotFound(err):
		return ownerGone
	default:
		return ownerUnknown
	}
}

// StartReconciler periodically cleans up orphaned shadow claims whose
// parent composite claim no longer exists or is deallocated.
func StartReconciler(ctx context.Context, client resourceclient.ResourceV1Interface, driverName string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	klog.InfoS("reconciler: started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileOrphans(ctx, client, driverName)
		}
	}
}

// ownerRefMatches reports whether any owner reference points at the given UID.
func ownerRefMatches(refs []metav1.OwnerReference, uid string) bool {
	for _, ref := range refs {
		if string(ref.UID) == uid {
			return true
		}
	}
	return false
}

func reconcileOrphans(ctx context.Context, client resourceclient.ResourceV1Interface, driverName string) {
	labelSelector := "app.kubernetes.io/managed-by=" + driverName

	namespaces := []string{""}
	claims, err := client.ResourceClaims("").List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		klog.ErrorS(err, "reconciler: list shadow claims failed")
		return
	}
	_ = namespaces

	orphaned := 0
	for _, sc := range claims.Items {
		compositeUID := sc.Labels["composite-claim-uid"]
		if compositeUID == "" {
			continue
		}

		orphan := false
		if !ownerRefMatches(sc.OwnerReferences, compositeUID) {
			// A managed shadow (managed-by label) whose owner reference does not point
			// at its labeled parent is malformed; reclaim it rather than keep it forever
			// while adoption keeps rejecting it.
			klog.V(2).InfoS("reconciler: managed shadow has no matching owner reference, reclaiming", "namespace", sc.Namespace, "name", sc.Name, "compositeUID", compositeUID)
			orphan = true
		} else {
			for _, ref := range sc.OwnerReferences {
				if string(ref.UID) != compositeUID {
					continue
				}
				owner, err := client.ResourceClaims(sc.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
				switch classifyOwner(owner, err, compositeUID) {
				case ownerGone:
					orphan = true
				case ownerUnknown:
					klog.ErrorS(err, "reconciler: owner lookup failed, keeping shadow this cycle", "namespace", sc.Namespace, "name", sc.Name)
				}
				break
			}
		}

		if orphan {
			klog.V(2).InfoS("reconciler: deleting orphaned shadow claim", "namespace", sc.Namespace, "name", sc.Name, "compositeUID", compositeUID)
			if err := shadow.DeleteExact(ctx, client, sc.Namespace, sc.Name, string(sc.UID)); err != nil {
				klog.ErrorS(err, "reconciler: delete shadow claim failed", "namespace", sc.Namespace, "name", sc.Name)
			} else {
				orphaned++
			}
		}
	}

	if orphaned > 0 {
		metrics.ReconcilerClaimsCleanedTotal.Add(float64(orphaned))
		klog.InfoS("reconciler: cleaned up orphaned shadow claims", "count", orphaned)
	}
}
