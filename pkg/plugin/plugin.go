// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/openshift-psap/composite-dra-driver/pkg/metrics"
	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

// PreparedDevice identifies an underlying device currently prepared by the composite driver.
type PreparedDevice struct {
	SourceName string
	Device     string
}

// CompositePlugin implements kubeletplugin.DRAPlugin for the composite driver.
type CompositePlugin struct {
	driverName      string
	deviceStore     *store.DeviceStore
	claimMgr        *shadow.ClaimManager
	paramsResolver  *shadow.DeviceParamsResolver
	grpcClient      *GRPCClient
	stateStore      *store.StateStore
	recorder        record.EventRecorder
	onStateChange   func()

	mu              sync.Mutex
	shadowClaims    map[types.UID][]shadowRecord
	preparedDevices map[types.UID][]PreparedDevice
	preparedComps   map[types.UID]string
}

type shadowRecord struct {
	driverName  string
	composition string
	info        *shadow.ShadowClaimInfo
}

var _ kubeletplugin.DRAPlugin = (*CompositePlugin)(nil)

func NewCompositePlugin(
	driverName string,
	deviceStore *store.DeviceStore,
	claimMgr *shadow.ClaimManager,
	paramsResolver *shadow.DeviceParamsResolver,
	grpcClient *GRPCClient,
	stateStore *store.StateStore,
	recorder record.EventRecorder,
) *CompositePlugin {
	p := &CompositePlugin{
		driverName:     driverName,
		deviceStore:    deviceStore,
		claimMgr:       claimMgr,
		paramsResolver: paramsResolver,
		grpcClient:     grpcClient,
		stateStore:     stateStore,
		recorder:       recorder,
		shadowClaims:    make(map[types.UID][]shadowRecord),
		preparedDevices: make(map[types.UID][]PreparedDevice),
		preparedComps:   make(map[types.UID]string),
	}

	if stateStore != nil {
		p.restoreFromState()
	}

	return p
}

// SetOnStateChange registers a callback invoked after Prepare/Unprepare
// completes, allowing the synthesizer to recompute with updated device state.
func (p *CompositePlugin) SetOnStateChange(fn func()) {
	p.onStateChange = fn
}

// PreparedDevicesByComposition returns a snapshot of underlying devices
// currently prepared, grouped by composition name.
func (p *CompositePlugin) PreparedDevicesByComposition() map[string][]PreparedDevice {
	p.mu.Lock()
	defer p.mu.Unlock()

	result := make(map[string][]PreparedDevice)
	for uid, devs := range p.preparedDevices {
		comp := p.preparedComps[uid]
		result[comp] = append(result[comp], devs...)
	}
	return result
}

func (p *CompositePlugin) notifyStateChange() {
	if p.onStateChange != nil {
		go p.onStateChange()
	}
}

func (p *CompositePlugin) PrepareResourceClaims(
	ctx context.Context,
	claims []*resourceapi.ResourceClaim,
) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		devices, err := p.prepareClaim(ctx, claim)
		results[claim.UID] = kubeletplugin.PrepareResult{Devices: devices, Err: err}
	}
	return results, nil
}

func (p *CompositePlugin) UnprepareResourceClaims(
	ctx context.Context,
	claims []kubeletplugin.NamespacedObject,
) (map[types.UID]error, error) {
	results := make(map[types.UID]error)
	for _, claim := range claims {
		results[claim.UID] = p.unprepareClaim(ctx, claim)
	}
	return results, nil
}

func (p *CompositePlugin) HandleError(ctx context.Context, err error, msg string) {
	runtime.HandleErrorWithContext(ctx, err, msg)
}

// memberWork holds the inputs and outputs for one member's parallel prepare.
type memberWork struct {
	pairIdx     int
	memberIdx   int
	member      store.DeviceMember
	allocResult resourceapi.DeviceRequestAllocationResult
	opaqueConfig []byte

	shadow   shadowRecord
	cdiIDs   []string
	err      error
}

func (p *CompositePlugin) prepareClaim(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
) ([]kubeletplugin.Device, error) {
	prepareStart := time.Now()

	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim %s/%s not allocated", claim.Namespace, claim.Name)
	}

	p.recorder.Eventf(claim, corev1.EventTypeNormal, "PrepareStarted", "Preparing composite resource claim")

	var composition string
	var work []*memberWork
	pairOrdinal := 0

	for _, allocResult := range claim.Status.Allocation.Devices.Results {
		if allocResult.Driver != p.driverName {
			continue
		}

		mapping := p.deviceStore.Get(allocResult.Pool, allocResult.Device)
		if mapping == nil {
			return nil, fmt.Errorf("unknown composite device %s/%s", allocResult.Pool, allocResult.Device)
		}
		if composition == "" {
			composition = mapping.CompositionName
		}

		for memberIdx, member := range mapping.Members {
			var opaqueConfig []byte
			if p.paramsResolver != nil {
				opaqueConfig = p.paramsResolver.ResolveForDevice(member.SourceName, member.Attributes, pairOrdinal)
				if opaqueConfig == nil {
					metrics.DeviceParamsErrorsTotal.WithLabelValues(composition).Inc()
				}
			}
			work = append(work, &memberWork{
				pairIdx:      pairOrdinal,
				memberIdx:    memberIdx,
				member:       member,
				allocResult:  allocResult,
				opaqueConfig: opaqueConfig,
			})
		}
		pairOrdinal++
	}

	// Phase 1: Create all shadow claims in parallel
	shadowStart := time.Now()
	var wg sync.WaitGroup
	for _, w := range work {
		wg.Add(1)
		go func(w *memberWork) {
			defer wg.Done()
			shadowInfo, err := p.claimMgr.Create(ctx, claim, &w.member, w.allocResult.Request, w.opaqueConfig)
			if err != nil {
				if errors.IsAlreadyExists(err) {
					klog.V(2).InfoS("plugin: shadow claim already exists, fetching existing", "driver", w.member.Driver, "device", w.member.Device)
					shadowInfo, err = p.claimMgr.Get(ctx, claim, &w.member)
					if err != nil {
						w.err = fmt.Errorf("get existing shadow for %s/%s: %w", w.member.Driver, w.member.Device, err)
						return
					}
				} else {
					w.err = fmt.Errorf("create shadow for %s/%s: %w", w.member.Driver, w.member.Device, err)
					return
				}
			}
			w.shadow = shadowRecord{driverName: w.member.Driver, composition: composition, info: shadowInfo}
		}(w)
	}
	wg.Wait()

	metrics.PrepareShadowCreateDurationSeconds.WithLabelValues(composition).Observe(time.Since(shadowStart).Seconds())

	var shadows []shadowRecord
	var firstErr error
	for _, w := range work {
		if w.shadow.info != nil {
			shadows = append(shadows, w.shadow)
		}
		if w.err != nil && firstErr == nil {
			firstErr = w.err
			p.recorder.Eventf(claim, corev1.EventTypeWarning, "PrepareFailed",
				"Shadow claim creation failed for %s/%s: %v", w.member.Driver, w.member.Device, w.err)
		}
	}
	if firstErr != nil {
		p.cleanupShadows(ctx, shadows)
		return nil, firstErr
	}

	// Phase 2: Call gRPC prepare on all underlying drivers in parallel
	grpcStart := time.Now()
	for _, w := range work {
		wg.Add(1)
		go func(w *memberWork) {
			defer wg.Done()
			resp, err := p.grpcClient.Prepare(ctx, w.shadow.driverName, w.shadow.info)
			if err != nil {
				metrics.GRPCErrorsTotal.WithLabelValues(composition, w.shadow.driverName).Inc()
				w.err = fmt.Errorf("prepare %s via gRPC: %w", w.shadow.driverName, err)
				return
			}
			for _, dev := range resp.Devices {
				w.cdiIDs = append(w.cdiIDs, dev.CdiDeviceIds...)
			}
		}(w)
	}
	wg.Wait()

	metrics.PrepareGRPCDurationSeconds.WithLabelValues(composition).Observe(time.Since(grpcStart).Seconds())

	for _, w := range work {
		if w.err != nil {
			p.recorder.Eventf(claim, corev1.EventTypeWarning, "PrepareFailed",
				"gRPC prepare failed for driver %s: %v", w.shadow.driverName, w.err)
			if isDeviceConflict(w.err) {
				p.reportDeviceConflict(ctx, claim, w.allocResult)
			}
			p.cleanupShadows(ctx, shadows)
			return nil, w.err
		}
	}

	// Assemble results grouped by composite device
	devicesByPair := make(map[int]*kubeletplugin.Device)
	for _, w := range work {
		key := w.pairIdx
		dev, ok := devicesByPair[key]
		if !ok {
			dev = &kubeletplugin.Device{
				Requests: []string{w.allocResult.Request},
				PoolName: w.allocResult.Pool,
				DeviceName: w.allocResult.Device,
			}
			devicesByPair[key] = dev
		}
		dev.CDIDeviceIDs = append(dev.CDIDeviceIDs, w.cdiIDs...)
	}

	var allDevices []kubeletplugin.Device
	for i := 0; i < pairOrdinal; i++ {
		if dev, ok := devicesByPair[i]; ok {
			allDevices = append(allDevices, *dev)
		}
	}

	var prepared []PreparedDevice
	for _, w := range work {
		prepared = append(prepared, PreparedDevice{SourceName: w.member.SourceName, Device: w.member.Device})
	}

	p.mu.Lock()
	p.shadowClaims[claim.UID] = shadows
	p.preparedDevices[claim.UID] = prepared
	p.preparedComps[claim.UID] = composition
	p.mu.Unlock()

	p.persistShadows(claim, shadows)

	elapsed := time.Since(prepareStart)
	metrics.PrepareDurationSeconds.WithLabelValues(composition).Observe(elapsed.Seconds())
	metrics.ClaimsActive.WithLabelValues(composition).Inc()
	metrics.ShadowClaimsActive.WithLabelValues(composition).Add(float64(len(shadows)))

	p.recorder.Eventf(claim, corev1.EventTypeNormal, "PrepareCompleted",
		"Prepared %d composite devices with %d shadow claims in %s", len(allDevices), len(shadows), elapsed.Round(time.Millisecond))

	klog.InfoS("plugin: prepared claim", "namespace", claim.Namespace, "claim", claim.Name, "compositeDevices", len(allDevices), "shadowClaims", len(shadows))

	p.notifyStateChange()

	return allDevices, nil
}

func (p *CompositePlugin) unprepareClaim(
	ctx context.Context,
	claim kubeletplugin.NamespacedObject,
) error {
	p.mu.Lock()
	shadows := p.shadowClaims[claim.UID]
	delete(p.shadowClaims, claim.UID)
	delete(p.preparedDevices, claim.UID)
	delete(p.preparedComps, claim.UID)
	p.mu.Unlock()

	var errs []error
	shadowCount := len(shadows)
	for _, sr := range shadows {
		if err := p.grpcClient.Unprepare(ctx, sr.driverName, sr.info); err != nil {
			klog.ErrorS(err, "plugin: unprepare shadow failed", "driver", sr.driverName, "shadow", sr.info.Name)
			errs = append(errs, err)
		}
		if err := p.claimMgr.Delete(ctx, sr.info.Namespace, sr.info.Name); err != nil {
			klog.ErrorS(err, "plugin: delete shadow claim failed", "shadow", sr.info.Name)
			errs = append(errs, err)
		}
	}

	if len(shadows) == 0 {
		if err := p.claimMgr.DeleteForCompositeClaim(ctx, claim.Namespace, string(claim.UID)); err != nil {
			klog.ErrorS(err, "plugin: cleanup orphaned shadows failed", "uid", claim.UID)
		}
	}

	p.deleteShadowState(string(claim.UID))

	if len(errs) > 0 {
		return fmt.Errorf("%d errors during unprepare: %v", len(errs), errs)
	}

	if shadowCount > 0 {
		metrics.ClaimsActive.WithLabelValues(shadows[0].composition).Dec()
		metrics.ShadowClaimsActive.WithLabelValues(shadows[0].composition).Sub(float64(shadowCount))
	}

	claimRef := &corev1.ObjectReference{
		APIVersion: "resource.k8s.io/v1",
		Kind:       "ResourceClaim",
		Namespace:  claim.Namespace,
		Name:       claim.Name,
		UID:        claim.UID,
	}
	p.recorder.Eventf(claimRef, corev1.EventTypeNormal, "UnprepareCompleted",
		"Cleaned up %d shadow claims", shadowCount)

	klog.InfoS("plugin: unprepared claim", "namespace", claim.Namespace, "claim", claim.Name, "shadowClaims", shadowCount)

	p.notifyStateChange()

	return nil
}

func (p *CompositePlugin) cleanupShadows(ctx context.Context, shadows []shadowRecord) {
	for _, sr := range shadows {
		_ = p.grpcClient.Unprepare(ctx, sr.driverName, sr.info)
		_ = p.claimMgr.Delete(ctx, sr.info.Namespace, sr.info.Name)
	}
}

func (p *CompositePlugin) persistShadows(claim *resourceapi.ResourceClaim, shadows []shadowRecord) {
	if p.stateStore == nil {
		return
	}
	entries := make([]store.ShadowEntry, len(shadows))
	for i, sr := range shadows {
		entries[i] = store.ShadowEntry{
			DriverName:  sr.driverName,
			Namespace:   sr.info.Namespace,
			Name:        sr.info.Name,
			UID:         sr.info.UID,
			Composition: sr.composition,
		}
	}
	if err := p.stateStore.SaveShadows(store.ShadowRecord{
		CompositeClaimUID: string(claim.UID),
		Namespace:         claim.Namespace,
		Shadows:           entries,
	}); err != nil {
		klog.ErrorS(err, "plugin: persist shadow state failed", "uid", claim.UID)
	}
}

func (p *CompositePlugin) deleteShadowState(compositeClaimUID string) {
	if p.stateStore == nil {
		return
	}
	if err := p.stateStore.DeleteShadows(compositeClaimUID); err != nil {
		klog.ErrorS(err, "plugin: delete shadow state failed", "uid", compositeClaimUID)
	}
}

func (p *CompositePlugin) restoreFromState() {
	records, err := p.stateStore.ListAll()
	if err != nil {
		klog.ErrorS(err, "plugin: restore state failed")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rec := range records {
		uid := types.UID(rec.CompositeClaimUID)
		var shadows []shadowRecord
		for _, entry := range rec.Shadows {
			shadows = append(shadows, shadowRecord{
				driverName:  entry.DriverName,
				composition: entry.Composition,
				info: &shadow.ShadowClaimInfo{
					Namespace: entry.Namespace,
					Name:      entry.Name,
					UID:       entry.UID,
				},
			})
		}
		p.shadowClaims[uid] = shadows
	}
	klog.InfoS("plugin: restored shadow claim records from state", "count", len(records))
}

func isDeviceConflict(err error) bool {
	return strings.Contains(err.Error(), "already allocated to different claim")
}

// reportDeviceConflict writes a DeviceConflict=True condition to the claim's
// device status, signaling the scheduler to deallocate and re-allocate.
// Requires DRADeviceBindingConditions + DRAResourceClaimDeviceStatus feature gates.
func (p *CompositePlugin) reportDeviceConflict(ctx context.Context, claim *resourceapi.ResourceClaim, allocResult resourceapi.DeviceRequestAllocationResult) {
	deviceStatus := resourceapi.AllocatedDeviceStatus{
		Driver: allocResult.Driver,
		Pool:   allocResult.Pool,
		Device: allocResult.Device,
		Conditions: []metav1.Condition{
			{
				Type:               "DeviceConflict",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "UnderlyingDeviceAlreadyAllocated",
				Message:            fmt.Sprintf("Device %s/%s is already allocated to another composition's claim", allocResult.Pool, allocResult.Device),
			},
		},
	}

	claimCopy := claim.DeepCopy()
	claimCopy.Status.Devices = append(claimCopy.Status.Devices, deviceStatus)

	if _, err := p.claimMgr.Client().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claimCopy, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "plugin: failed to report DeviceConflict condition", "claim", claim.Name, "device", allocResult.Device)
	} else {
		klog.InfoS("plugin: reported DeviceConflict condition for scheduler retry", "claim", claim.Name, "device", allocResult.Device)
	}
}
