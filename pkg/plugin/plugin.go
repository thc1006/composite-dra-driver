// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1"

	"github.com/openshift-psap/composite-dra-driver/pkg/metrics"
	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

// shadowPreparer prepares and unprepares a shadow claim on an underlying DRA driver.
// The plugin depends on this narrow interface (satisfied by *GRPCClient) so the
// prepare and rollback paths can be exercised without a live gRPC socket.
type shadowPreparer interface {
	Prepare(ctx context.Context, driverName string, claim *shadow.ShadowClaimInfo) (*drapbv1.NodePrepareResourceResponse, error)
	Unprepare(ctx context.Context, driverName string, claim *shadow.ShadowClaimInfo) error
}

// shadowClaimManager creates, adopts, and deletes shadow ResourceClaims. It is the
// narrow interface the plugin needs (satisfied by *shadow.ClaimManager), kept small
// so failure paths can be driven with a fake in tests.
type shadowClaimManager interface {
	Create(ctx context.Context, compositeClaim *resourceapi.ResourceClaim, member *store.DeviceMember, requestName string, opaqueConfig []byte) (*shadow.ShadowClaimInfo, error)
	Get(ctx context.Context, compositeClaim *resourceapi.ResourceClaim, member *store.DeviceMember, requestName string) (*shadow.ShadowClaimInfo, error)
	Delete(ctx context.Context, info *shadow.ShadowClaimInfo) error
	DeleteForCompositeClaim(ctx context.Context, namespace, compositeClaimUID string) error
}

// CompositePlugin implements kubeletplugin.DRAPlugin for the composite driver.
type CompositePlugin struct {
	driverName     string
	deviceStore    *store.DeviceStore
	claimMgr       shadowClaimManager
	paramsResolver *shadow.DeviceParamsResolver
	grpcClient     shadowPreparer
	stateStore     *store.StateStore
	recorder       record.EventRecorder

	mu           sync.Mutex
	shadowClaims map[types.UID][]shadowRecord
}

type shadowRecord struct {
	driverName  string
	composition string
	info        *shadow.ShadowClaimInfo
	created     bool // this Prepare attempt created the shadow, rather than adopting an existing one
}

var _ kubeletplugin.DRAPlugin = (*CompositePlugin)(nil)

func NewCompositePlugin(
	driverName string,
	deviceStore *store.DeviceStore,
	claimMgr shadowClaimManager,
	paramsResolver *shadow.DeviceParamsResolver,
	grpcClient shadowPreparer,
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
		shadowClaims:   make(map[types.UID][]shadowRecord),
	}

	if stateStore != nil {
		p.restoreFromState()
	}

	return p
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
	pairIdx      int
	memberIdx    int
	member       store.DeviceMember
	allocResult  resourceapi.DeviceRequestAllocationResult
	opaqueConfig []byte

	shadow shadowRecord
	cdiIDs []string
	err    error
}

// rejectDuplicateMembers fails closed when two composite devices in the same
// claim resolve to the same underlying (driver, pool, device) member. The shadow
// claim name is keyed on the member alone, so two work items for one member would
// collapse onto a single shadow: the second Create gets AlreadyExists and adopts
// the first, and the same underlying device (with its CDI IDs) ends up satisfying
// both composite devices while the caller believes it got two. Sharing an
// underlying device is not modeled yet, so reject the allocation before any shadow
// is created rather than silently over-allocating.
func rejectDuplicateMembers(work []*memberWork) error {
	type memberKey struct {
		driver, pool, device string
	}
	type occurrence struct {
		compositePool, compositeDevice string
	}
	seen := make(map[memberKey]occurrence, len(work))
	for _, w := range work {
		key := memberKey{driver: w.member.Driver, pool: w.member.Pool, device: w.member.Device}
		cur := occurrence{compositePool: w.allocResult.Pool, compositeDevice: w.allocResult.Device}
		if prev, found := seen[key]; found {
			return fmt.Errorf("composite devices %s/%s and %s/%s share underlying member %s/%s/%s; sharing an underlying device is not supported",
				prev.compositePool, prev.compositeDevice, cur.compositePool, cur.compositeDevice,
				key.driver, key.pool, key.device)
		}
		seen[key] = cur
	}
	return nil
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

	if err := rejectDuplicateMembers(work); err != nil {
		p.recorder.Eventf(claim, corev1.EventTypeWarning, "PrepareFailed", "%v", err)
		return nil, err
	}

	// Phase 1: Create all shadow claims in parallel
	shadowStart := time.Now()
	var wg sync.WaitGroup
	for _, w := range work {
		wg.Add(1)
		go func(w *memberWork) {
			defer wg.Done()
			created := true
			shadowInfo, err := p.claimMgr.Create(ctx, claim, &w.member, w.allocResult.Request, w.opaqueConfig)
			if err != nil {
				if errors.IsAlreadyExists(err) {
					klog.V(2).InfoS("plugin: shadow claim already exists, fetching existing", "driver", w.member.Driver, "device", w.member.Device)
					created = false
					shadowInfo, err = p.claimMgr.Get(ctx, claim, &w.member, w.allocResult.Request)
					if err != nil {
						w.err = fmt.Errorf("get existing shadow for %s/%s: %w", w.member.Driver, w.member.Device, err)
						return
					}
				} else {
					w.err = fmt.Errorf("create shadow for %s/%s: %w", w.member.Driver, w.member.Device, err)
					return
				}
			}
			w.shadow = shadowRecord{driverName: w.member.Driver, composition: composition, info: shadowInfo, created: created}
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
		// Phase 1 failure: no gRPC Prepare has run yet, so the created shadows can be
		// deleted directly.
		p.cleanupShadows(ctx, shadows, false)
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
			// Phase 2 failure: gRPC Prepare has run, so unprepare before deleting and keep
			// any shadow that fails to unprepare.
			p.cleanupShadows(ctx, shadows, true)
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
				Requests:   []string{w.allocResult.Request},
				PoolName:   w.allocResult.Pool,
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

	p.mu.Lock()
	p.shadowClaims[claim.UID] = shadows
	p.mu.Unlock()

	if err := p.persistShadows(string(claim.UID), claim.Namespace, shadows); err != nil {
		klog.ErrorS(err, "plugin: persist shadow state failed", "uid", claim.UID)
	}

	elapsed := time.Since(prepareStart)
	metrics.PrepareDurationSeconds.WithLabelValues(composition).Observe(elapsed.Seconds())
	metrics.ClaimsActive.WithLabelValues(composition).Inc()
	metrics.ShadowClaimsActive.WithLabelValues(composition).Add(float64(len(shadows)))

	p.recorder.Eventf(claim, corev1.EventTypeNormal, "PrepareCompleted",
		"Prepared %d composite devices with %d shadow claims in %s", len(allDevices), len(shadows), elapsed.Round(time.Millisecond))

	klog.InfoS("plugin: prepared claim", "namespace", claim.Namespace, "claim", claim.Name, "compositeDevices", len(allDevices), "shadowClaims", len(shadows))

	return allDevices, nil
}

func (p *CompositePlugin) unprepareClaim(
	ctx context.Context,
	claim kubeletplugin.NamespacedObject,
) error {
	p.mu.Lock()
	shadows := p.shadowClaims[claim.UID]
	p.mu.Unlock()

	var errs []error
	var remaining []shadowRecord
	shadowCount := len(shadows)
	for _, sr := range shadows {
		if err := p.grpcClient.Unprepare(ctx, sr.driverName, sr.info); err != nil {
			klog.ErrorS(err, "plugin: unprepare shadow failed", "driver", sr.driverName, "shadow", sr.info.Name)
			errs = append(errs, err)
			remaining = append(remaining, sr)
			continue
		}
		if err := p.claimMgr.Delete(ctx, sr.info); err != nil {
			klog.ErrorS(err, "plugin: delete shadow claim failed", "shadow", sr.info.Name)
			errs = append(errs, err)
			remaining = append(remaining, sr)
			continue
		}
		metrics.ShadowClaimsActive.WithLabelValues(sr.composition).Dec()
	}

	if len(shadows) == 0 {
		if err := p.claimMgr.DeleteForCompositeClaim(ctx, claim.Namespace, string(claim.UID)); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		// Keep the recovery state for the shadows that did not unprepare, so a retry
		// can drive them again. Persist it durably first, so a restart replays only the
		// remaining shadows and not the ones already unprepared and deleted.
		if len(remaining) > 0 {
			if err := p.persistShadows(string(claim.UID), claim.Namespace, remaining); err != nil {
				errs = append(errs, err)
			}
		}
		p.mu.Lock()
		p.shadowClaims[claim.UID] = remaining
		p.mu.Unlock()
		return fmt.Errorf("%d errors during unprepare: %v", len(errs), errs)
	}

	// Everything unprepared. Delete the durable checkpoint before the in-memory record,
	// and report a failure, so a restart cannot resurrect the claim from a stale
	// checkpoint. A retry re-runs the (now idempotent) unprepare and delete.
	if err := p.deleteShadowState(string(claim.UID)); err != nil {
		return fmt.Errorf("delete shadow state for claim %s: %w", claim.UID, err)
	}
	p.mu.Lock()
	delete(p.shadowClaims, claim.UID)
	p.mu.Unlock()

	if shadowCount > 0 {
		metrics.ClaimsActive.WithLabelValues(shadows[0].composition).Dec()
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
	return nil
}

// cleanupShadows rolls back the shadow claims a failed Prepare attempt created.
//
// Before gRPC Prepare has run (prepared=false) the shadows were never prepared on an
// underlying driver, so they are deleted directly. After gRPC Prepare has run
// (prepared=true) an underlying preparation may exist, so each shadow is unprepared
// first and deleted only when that succeeds. A shadow whose Unprepare fails is left in
// place: its API object and owner reference stay as the handle a Prepare retry adopts
// (Create -> AlreadyExists -> Get) to release the underlying resource, instead of
// deleting the shadow and leaking the resource with nothing left to unprepare it. This
// is the Prepare-rollback side of the resource leak tracked in #64.
//
// A kept shadow is recovered when the kubelet retries Prepare, which re-adopts it and
// re-drives the (idempotent, per the DRA contract) underlying Prepare. If the pod is
// instead deleted before a retry succeeds, teardown falls to the claim's Unprepare
// path rather than this one.
func (p *CompositePlugin) cleanupShadows(ctx context.Context, shadows []shadowRecord, prepared bool) {
	for _, sr := range shadows {
		// Only roll back shadows this Prepare attempt created. A shadow adopted via
		// AlreadyExists may already be prepared and in use by a running workload, so
		// tearing it down on a later member's failure would break that workload.
		if !sr.created {
			continue
		}
		if prepared {
			if err := p.grpcClient.Unprepare(ctx, sr.driverName, sr.info); err != nil {
				// A gRPC error is an ambiguous outcome: the underlying resource may still
				// be prepared. Keep the shadow so a retry can drive its Unprepare again.
				klog.ErrorS(err, "plugin: rollback unprepare failed, keeping shadow for retry", "driver", sr.driverName, "shadow", sr.info.Name)
				continue
			}
		}
		if err := p.claimMgr.Delete(ctx, sr.info); err != nil {
			klog.ErrorS(err, "plugin: rollback delete shadow failed", "shadow", sr.info.Name)
		}
	}
}

func (p *CompositePlugin) persistShadows(uid, namespace string, shadows []shadowRecord) error {
	if p.stateStore == nil {
		return nil
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
	return p.stateStore.SaveShadows(store.ShadowRecord{
		CompositeClaimUID: uid,
		Namespace:         namespace,
		Shadows:           entries,
	})
}

func (p *CompositePlugin) deleteShadowState(compositeClaimUID string) error {
	if p.stateStore == nil {
		return nil
	}
	return p.stateStore.DeleteShadows(compositeClaimUID)
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
