// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type physicalDeviceKey struct {
	SourceName string
	Device     string
}

// bindingWatcher watches ResourceClaims and maintains a physical device
// reservation map to detect cross-composition conflicts before binding.
type bindingWatcher struct {
	plugin *CompositePlugin

	mu          sync.Mutex
	// physicalDevice → claimUID that reserved it via DeviceReady
	reservations map[physicalDeviceKey]types.UID
	// claimUID → physical devices reserved by that claim
	claimDevices map[types.UID][]physicalDeviceKey
}

// StartBindingWatcher watches ResourceClaims allocated to this driver and sets
// binding conditions (DeviceReady or DeviceConflict) based on physical device
// availability. Runs independently of kubelet Prepare, breaking the PreBind
// chicken-and-egg deadlock.
func StartBindingWatcher(ctx context.Context, kubeClient kubernetes.Interface, plugin *CompositePlugin) {
	bw := &bindingWatcher{
		plugin:       plugin,
		reservations: make(map[physicalDeviceKey]types.UID),
		claimDevices: make(map[types.UID][]physicalDeviceKey),
	}

	factory := informers.NewSharedInformerFactory(kubeClient, 0)
	claimInformer := factory.Resource().V1().ResourceClaims()

	claimInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if claim, ok := obj.(*resourceapi.ResourceClaim); ok {
				bw.handleClaim(ctx, claim)
			}
		},
		UpdateFunc: func(_, obj interface{}) {
			if claim, ok := obj.(*resourceapi.ResourceClaim); ok {
				bw.handleClaim(ctx, claim)
			}
		},
		DeleteFunc: func(obj interface{}) {
			claim, ok := obj.(*resourceapi.ResourceClaim)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				claim, ok = tombstone.Obj.(*resourceapi.ResourceClaim)
				if !ok {
					return
				}
			}
			bw.releaseClaim(claim.UID)
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	klog.InfoS("binding-watcher: started")
	<-ctx.Done()
}

func (bw *bindingWatcher) handleClaim(ctx context.Context, claim *resourceapi.ResourceClaim) {
	// Claim deallocated — release reservations
	if claim.Status.Allocation == nil {
		bw.releaseClaim(claim.UID)
		return
	}

	// Only process claims with our driver + binding conditions
	hasOurDevices := false
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == bw.plugin.driverName && len(result.BindingConditions) > 0 {
			hasOurDevices = true
			break
		}
	}
	if !hasOurDevices {
		return
	}

	// Already processed — check if we already set conditions
	if len(claim.Status.Devices) > 0 {
		for _, ds := range claim.Status.Devices {
			if ds.Driver == bw.plugin.driverName {
				if apimeta.FindStatusCondition(ds.Conditions, "DeviceReady") != nil ||
					apimeta.FindStatusCondition(ds.Conditions, "DeviceConflict") != nil {
					return
				}
			}
		}
	}

	// Resolve all underlying physical devices for this claim
	var members []physicalDeviceKey
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != bw.plugin.driverName {
			continue
		}
		mapping := bw.plugin.deviceStore.Get(result.Pool, result.Device)
		if mapping == nil {
			klog.V(2).InfoS("binding-watcher: device not in store", "claim", claim.Name, "device", result.Device)
			continue
		}
		for _, m := range mapping.Members {
			members = append(members, physicalDeviceKey{SourceName: m.SourceName, Device: m.Device})
		}
	}

	// Check for conflicts against reservations and prepared devices
	conflict, conflictDevice := bw.tryReserve(claim.UID, members)
	if conflict {
		klog.InfoS("binding-watcher: conflict detected", "claim", claim.Name, "conflictDevice", conflictDevice)
		bw.writeDeviceConflict(ctx, claim, conflictDevice)
		return
	}

	bw.writeDeviceReadyAll(ctx, claim)
}

// tryReserve attempts to reserve all physical devices for a claim.
// Returns (true, conflicting device) if any device is already reserved or prepared.
// On conflict, no reservations are made (atomic — all or nothing).
func (bw *bindingWatcher) tryReserve(claimUID types.UID, members []physicalDeviceKey) (bool, string) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	// Check against existing reservations
	for _, key := range members {
		if existingUID, exists := bw.reservations[key]; exists && existingUID != claimUID {
			return true, fmt.Sprintf("%s/%s", key.SourceName, key.Device)
		}
	}

	// Check against prepared devices (runtime state from Prepare)
	bw.plugin.mu.Lock()
	defer bw.plugin.mu.Unlock()
	for _, key := range members {
		for _, prepared := range bw.plugin.preparedDevices {
			for _, pd := range prepared {
				if pd.SourceName == key.SourceName && pd.Device == key.Device {
					return true, fmt.Sprintf("%s/%s", key.SourceName, key.Device)
				}
			}
		}
	}

	// No conflict — register all
	for _, key := range members {
		bw.reservations[key] = claimUID
	}
	bw.claimDevices[claimUID] = append(bw.claimDevices[claimUID], members...)
	return false, ""
}

func (bw *bindingWatcher) releaseClaim(claimUID types.UID) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	for _, key := range bw.claimDevices[claimUID] {
		if bw.reservations[key] == claimUID {
			delete(bw.reservations, key)
		}
	}
	delete(bw.claimDevices, claimUID)
}

func (bw *bindingWatcher) writeDeviceReadyAll(ctx context.Context, claim *resourceapi.ResourceClaim) {
	// Re-fetch claim to get latest ResourceVersion
	latest, err := bw.plugin.claimMgr.Client().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "binding-watcher: failed to re-fetch claim for DeviceReady", "claim", claim.Name)
		bw.releaseClaim(claim.UID)
		return
	}

	var deviceStatuses []resourceapi.AllocatedDeviceStatus
	for _, result := range latest.Status.Allocation.Devices.Results {
		if result.Driver != bw.plugin.driverName {
			continue
		}
		deviceStatuses = append(deviceStatuses, resourceapi.AllocatedDeviceStatus{
			Driver: result.Driver,
			Pool:   result.Pool,
			Device: result.Device,
			Conditions: []metav1.Condition{
				{
					Type:               "DeviceReady",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "DeviceAvailable",
					Message:            "Device is available for this composition",
				},
			},
		})
	}

	latest.Status.Devices = deviceStatuses
	if _, err := bw.plugin.claimMgr.Client().ResourceClaims(claim.Namespace).UpdateStatus(ctx, latest, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "binding-watcher: failed to write DeviceReady", "claim", claim.Name)
		bw.releaseClaim(claim.UID)
	} else {
		klog.InfoS("binding-watcher: DeviceReady", "claim", claim.Name, "devices", len(deviceStatuses))
	}
}

func (bw *bindingWatcher) writeDeviceConflict(ctx context.Context, claim *resourceapi.ResourceClaim, conflictDevice string) {
	// Re-fetch claim to get latest ResourceVersion (avoids optimistic concurrency conflict)
	latest, err := bw.plugin.claimMgr.Client().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "binding-watcher: failed to re-fetch claim for DeviceConflict", "claim", claim.Name)
		return
	}

	var deviceStatuses []resourceapi.AllocatedDeviceStatus
	for _, result := range latest.Status.Allocation.Devices.Results {
		if result.Driver != bw.plugin.driverName {
			continue
		}
		deviceStatuses = append(deviceStatuses, resourceapi.AllocatedDeviceStatus{
			Driver: result.Driver,
			Pool:   result.Pool,
			Device: result.Device,
			Conditions: []metav1.Condition{
				{
					Type:               "DeviceConflict",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "UnderlyingDeviceAlreadyReserved",
					Message:            fmt.Sprintf("Physical device %s is reserved by another composition's claim", conflictDevice),
				},
			},
		})
	}

	latest.Status.Devices = deviceStatuses
	if _, err := bw.plugin.claimMgr.Client().ResourceClaims(claim.Namespace).UpdateStatus(ctx, latest, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "binding-watcher: failed to write DeviceConflict", "claim", claim.Name)
	} else {
		klog.InfoS("binding-watcher: DeviceConflict", "claim", claim.Name, "conflictDevice", conflictDevice)
	}
}
