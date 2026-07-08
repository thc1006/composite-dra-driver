// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package synthesizer

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

const maxDevicesPerSlice = 128

// ResourcePublisher is the interface for publishing composite ResourceSlices.
type ResourcePublisher interface {
	Publish(driverName, nodeName string, compositeDevices []CompositeDevice) error
}

// HelperPublisher publishes via kubeletplugin.Helper.PublishResources().
type HelperPublisher struct {
	publishFn func(resources resourceslice.DriverResources) error
}

func NewHelperPublisher(publishFn func(resources resourceslice.DriverResources) error) *HelperPublisher {
	return &HelperPublisher{publishFn: publishFn}
}

func (p *HelperPublisher) Publish(driverName, nodeName string, compositeDevices []CompositeDevice) error {
	groups := groupByComposition(compositeDevices)
	pools := make(map[string]resourceslice.Pool, len(groups))

	for compName, devices := range groups {
		poolName := PoolName(driverName, nodeName, compName)
		slices := buildSlices(devices)
		pools[poolName] = resourceslice.Pool{Slices: slices}
		klog.InfoS("publisher: pool published", "pool", poolName, "count", len(devices), "slices", len(slices))
	}

	if len(pools) == 0 {
		poolName := fmt.Sprintf("%s-%s", driverName, nodeName)
		pools[poolName] = resourceslice.Pool{Slices: []resourceslice.Slice{{}}}
	}

	resources := resourceslice.DriverResources{Pools: pools}
	return p.publishFn(resources)
}

// PoolName computes the pool name for a given composition.
func PoolName(driverName, nodeName, compositionName string) string {
	return fmt.Sprintf("%s-%s-%s", driverName, nodeName, compositionName)
}

func groupByComposition(devices []CompositeDevice) map[string][]CompositeDevice {
	groups := make(map[string][]CompositeDevice)
	for _, d := range devices {
		name := d.Mapping.CompositionName
		groups[name] = append(groups[name], d)
	}
	return groups
}

func buildSlices(devices []CompositeDevice) []resourceslice.Slice {
	var slices []resourceslice.Slice
	for i := 0; i < len(devices); i += maxDevicesPerSlice {
		end := i + maxDevicesPerSlice
		if end > len(devices) {
			end = len(devices)
		}
		batch := devices[i:end]
		devs := make([]resourceapi.Device, 0, len(batch))
		for _, cd := range batch {
			devs = append(devs, resourceapi.Device{
				Name:                    cd.Name,
				Attributes:              convertAttributes(cd.Attributes),
				BindingFailureConditions: []string{"DeviceConflict"},
			})
		}
		slices = append(slices, resourceslice.Slice{Devices: devs})
	}
	if len(slices) == 0 {
		slices = []resourceslice.Slice{{}}
	}
	return slices
}

// LogPublisher is a stub that logs instead of publishing (for testing).
type LogPublisher struct{}

func (p *LogPublisher) Publish(driverName, nodeName string, compositeDevices []CompositeDevice) error {
	groups := groupByComposition(compositeDevices)
	for compName, devices := range groups {
		klog.InfoS("publisher(log): devices for pool", "count", len(devices), "pool", PoolName(driverName, nodeName, compName))
	}
	return nil
}

// BuildResourceSlices is kept for unit testing — builds specs without publishing.
// Returns slices grouped by per-composition pools.
func BuildResourceSlices(driverName, nodeName string, compositeDevices []CompositeDevice) []resourceapi.ResourceSliceSpec {
	if len(compositeDevices) == 0 {
		return nil
	}

	groups := groupByComposition(compositeDevices)
	var allSpecs []resourceapi.ResourceSliceSpec

	for compName, devices := range groups {
		poolName := PoolName(driverName, nodeName, compName)
		var poolSpecs []resourceapi.ResourceSliceSpec

		for i := 0; i < len(devices); i += maxDevicesPerSlice {
			end := i + maxDevicesPerSlice
			if end > len(devices) {
				end = len(devices)
			}
			batch := devices[i:end]

			devs := make([]resourceapi.Device, 0, len(batch))
			for _, cd := range batch {
				devs = append(devs, resourceapi.Device{
					Name:                    cd.Name,
					Attributes:              convertAttributes(cd.Attributes),
					BindingFailureConditions: []string{"DeviceConflict"},
				})
			}

			spec := resourceapi.ResourceSliceSpec{
				Driver:   driverName,
				NodeName: &nodeName,
				Pool: resourceapi.ResourcePool{
					Name:               poolName,
					ResourceSliceCount: 1,
				},
				Devices: devs,
			}
			poolSpecs = append(poolSpecs, spec)
		}

		for i := range poolSpecs {
			poolSpecs[i].Pool.ResourceSliceCount = int64(len(poolSpecs))
		}
		allSpecs = append(allSpecs, poolSpecs...)
	}

	return allSpecs
}

func convertAttributes(attrs map[string]resourceapi.DeviceAttribute) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	result := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, len(attrs))
	for key, val := range attrs {
		result[resourceapi.QualifiedName(key)] = val
	}
	return result
}

