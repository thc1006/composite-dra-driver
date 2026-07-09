// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

// StartBindingWatcher watches ResourceClaims allocated to this driver and sets
// binding conditions (DeviceReady or DeviceConflict) based on device availability.
// This runs independently of the kubelet Prepare path, breaking the chicken-and-egg
// deadlock between scheduler PreBind and kubelet Prepare.
func StartBindingWatcher(ctx context.Context, kubeClient kubernetes.Interface, plugin *CompositePlugin) {
	factory := informers.NewSharedInformerFactory(kubeClient, 0)
	claimInformer := factory.Resource().V1().ResourceClaims()

	claimInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			claim, ok := obj.(*resourceapi.ResourceClaim)
			if !ok {
				return
			}
			plugin.handleClaimBinding(ctx, claim)
		},
		UpdateFunc: func(_, obj interface{}) {
			claim, ok := obj.(*resourceapi.ResourceClaim)
			if !ok {
				return
			}
			plugin.handleClaimBinding(ctx, claim)
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	klog.InfoS("binding-watcher: started")
	<-ctx.Done()
}

func (p *CompositePlugin) handleClaimBinding(ctx context.Context, claim *resourceapi.ResourceClaim) {
	if claim.Status.Allocation == nil {
		return
	}

	// Only process claims allocated to our driver
	hasOurDevices := false
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == p.driverName && len(result.BindingConditions) > 0 {
			hasOurDevices = true
			break
		}
	}
	if !hasOurDevices {
		return
	}

	// Check if we already set conditions on this claim
	if len(claim.Status.Devices) > 0 {
		for _, ds := range claim.Status.Devices {
			if ds.Driver == p.driverName {
				cond := apimeta.FindStatusCondition(ds.Conditions, "DeviceReady")
				if cond != nil {
					return
				}
				cond = apimeta.FindStatusCondition(ds.Conditions, "DeviceConflict")
				if cond != nil {
					return
				}
			}
		}
	}

	// Check each allocated device for conflicts
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != p.driverName {
			continue
		}

		mapping := p.deviceStore.Get(result.Pool, result.Device)
		if mapping == nil {
			klog.V(2).InfoS("binding-watcher: device not in store, skipping", "claim", claim.Name, "device", result.Device)
			continue
		}

		conflict := p.checkDeviceConflict(mapping)
		if conflict {
			p.writeBindingCondition(ctx, claim, result, "DeviceConflict", metav1.ConditionTrue,
				"UnderlyingDeviceAlreadyAllocated",
				fmt.Sprintf("Device %s contains member(s) already prepared by another composition", result.Device))
			return
		}
	}

	// No conflicts — set DeviceReady on all our devices
	p.writeDeviceReadyAll(ctx, claim)
}

func (p *CompositePlugin) checkDeviceConflict(mapping *store.DeviceMapping) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, prepared := range p.preparedDevices {
		for _, pd := range prepared {
			for _, member := range mapping.Members {
				if pd.SourceName == member.SourceName && pd.Device == member.Device {
					return true
				}
			}
		}
	}
	return false
}

func (p *CompositePlugin) writeDeviceReadyAll(ctx context.Context, claim *resourceapi.ResourceClaim) {
	var deviceStatuses []resourceapi.AllocatedDeviceStatus
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != p.driverName {
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

	claimCopy := claim.DeepCopy()
	claimCopy.Status.Devices = deviceStatuses
	if _, err := p.claimMgr.Client().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claimCopy, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "binding-watcher: failed to write DeviceReady", "claim", claim.Name)
	} else {
		klog.InfoS("binding-watcher: set DeviceReady for claim", "claim", claim.Name, "devices", len(deviceStatuses))
	}
}

func (p *CompositePlugin) writeBindingCondition(ctx context.Context, claim *resourceapi.ResourceClaim, result resourceapi.DeviceRequestAllocationResult, condType string, status metav1.ConditionStatus, reason, message string) {
	deviceStatus := resourceapi.AllocatedDeviceStatus{
		Driver: result.Driver,
		Pool:   result.Pool,
		Device: result.Device,
		Conditions: []metav1.Condition{
			{
				Type:               condType,
				Status:             status,
				LastTransitionTime: metav1.Now(),
				Reason:             reason,
				Message:            message,
			},
		},
	}

	claimCopy := claim.DeepCopy()
	claimCopy.Status.Devices = append(claimCopy.Status.Devices, deviceStatus)
	if _, err := p.claimMgr.Client().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claimCopy, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "binding-watcher: failed to write condition", "claim", claim.Name, "condition", condType)
	} else {
		klog.InfoS("binding-watcher: set condition for claim", "claim", claim.Name, "condition", condType, "device", result.Device)
	}
}
