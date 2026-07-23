// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
	"k8s.io/klog/v2"

	"github.com/openshift-psap/composite-dra-driver/pkg/metrics"
)

const (
	MutatedAnnotation = "composite.dra/mutated"
)

// MutatorConfig holds the webhook configuration.
type MutatorConfig struct {
	// ResourceMappings maps synthetic resource names to DeviceClass names.
	// e.g., "composite.dra/gpu-nic-pair" -> "composite-gpu-nic-pair"
	ResourceMappings map[string]string
}

// Mutator generates ResourceClaimTemplates for composite devices.
type Mutator struct {
	cfg    MutatorConfig
	client resourceclient.ResourceV1Interface
}

func NewMutator(cfg MutatorConfig, client resourceclient.ResourceV1Interface) *Mutator {
	return &Mutator{cfg: cfg, client: client}
}

type resourceMatch struct {
	ResourceName    string
	DeviceClassName string
	Count           int
	ContainerIdx    int
}

// Mutate processes a pod and returns JSON patch operations.
// Returns nil if no mutation is needed.
func (m *Mutator) Mutate(ctx context.Context, pod *corev1.Pod, namespace string) ([]PatchOp, error) {
	if pod.Annotations[MutatedAnnotation] == "true" {
		metrics.WebhookSkippedTotal.WithLabelValues("already_mutated").Inc()
		return nil, nil
	}

	matches := m.findResourceRequests(pod)
	if len(matches) == 0 {
		metrics.WebhookSkippedTotal.WithLabelValues("no_resource_request").Inc()
		return nil, nil
	}

	// Group matches by composition (resource name → device class)
	type compositionRequest struct {
		resourceName    string
		deviceClassName string
		totalCount      int
		containerIdxs   []int
	}

	compMap := make(map[string]*compositionRequest)
	for _, match := range matches {
		cr, ok := compMap[match.ResourceName]
		if !ok {
			cr = &compositionRequest{
				resourceName:    match.ResourceName,
				deviceClassName: match.DeviceClassName,
			}
			compMap[match.ResourceName] = cr
		}
		cr.totalCount += match.Count
		cr.containerIdxs = append(cr.containerIdxs, match.ContainerIdx)
	}

	// Sort for deterministic output
	var compKeys []string
	for k := range compMap {
		compKeys = append(compKeys, k)
	}
	sort.Strings(compKeys)

	// Create one ResourceClaimTemplate per composition type
	var claims []claimInfo

	for _, resName := range compKeys {
		cr := compMap[resName]
		templateName := compositionTemplateName(podPrefix(pod), cr.deviceClassName)

		claimSpec := BuildClaimSpec(cr.deviceClassName, cr.totalCount)
		template := &resourceapi.ResourceClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      templateName,
				Namespace: namespace,
				Labels: map[string]string{
					ManagedByLabel: ManagedByValue,
				},
			},
			Spec: resourceapi.ResourceClaimTemplateSpec{
				Spec: claimSpec,
			},
		}

		_, err := m.client.ResourceClaimTemplates(namespace).Create(ctx, template, metav1.CreateOptions{})
		if err != nil {
			if errors.IsAlreadyExists(err) {
				klog.V(2).InfoS("webhook: claim template already exists (idempotent)", "namespace", namespace, "template", templateName)
			} else {
				return nil, fmt.Errorf("create claim template %s: %w", templateName, err)
			}
		} else {
			metrics.WebhookTemplatesCreatedTotal.WithLabelValues(cr.deviceClassName).Inc()
			klog.InfoS("webhook: created ResourceClaimTemplate", "namespace", namespace, "template", templateName, "count", cr.totalCount, "deviceClass", cr.deviceClassName)
		}

		claims = append(claims, claimInfo{
			templateName:    templateName,
			resourceName:    cr.resourceName,
			deviceClassName: cr.deviceClassName,
			pairCount:       cr.totalCount,
			containerIdxs:   cr.containerIdxs,
		})
	}

	patches := m.buildPatches(pod, matches, claims)

	seenCompositions := make(map[string]struct{}, len(claims))
	for _, claim := range claims {
		if _, seen := seenCompositions[claim.deviceClassName]; seen {
			continue
		}
		seenCompositions[claim.deviceClassName] = struct{}{}
		metrics.WebhookMutationsTotal.WithLabelValues(claim.deviceClassName).Inc()
	}

	return patches, nil
}

func podPrefix(pod *corev1.Pod) string {
	if pod.Name != "" {
		return pod.Name
	}
	return strings.TrimRight(pod.GenerateName, "-")
}

func compositionTemplateName(podName, deviceClassName string) string {
	name := fmt.Sprintf("%s-%s", podName, deviceClassName)
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// findResourceRequests scans all containers for any configured synthetic resource.
func (m *Mutator) findResourceRequests(pod *corev1.Pod) []resourceMatch {
	var matches []resourceMatch
	for i := range pod.Spec.Containers {
		for resName, devClass := range m.cfg.ResourceMappings {
			if qty, ok := pod.Spec.Containers[i].Resources.Requests[corev1.ResourceName(resName)]; ok {
				count, ok := qty.AsInt64()
				if ok && count > 0 {
					matches = append(matches, resourceMatch{
						ResourceName:    resName,
						DeviceClassName: devClass,
						Count:           int(count),
						ContainerIdx:    i,
					})
				}
			}
		}
	}
	return matches
}

// PatchOp is a JSON Patch operation.
type PatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

type claimInfo struct {
	templateName    string
	resourceName    string
	deviceClassName string
	pairCount       int
	containerIdxs   []int
}

func (m *Mutator) buildPatches(pod *corev1.Pod, matches []resourceMatch, claims []claimInfo) []PatchOp {
	var patches []PatchOp

	// Add mutated annotation
	if pod.Annotations == nil {
		patches = append(patches, PatchOp{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: map[string]string{MutatedAnnotation: "true"},
		})
	} else {
		patches = append(patches, PatchOp{
			Op:    "add",
			Path:  "/metadata/annotations/composite.dra~1mutated",
			Value: "true",
		})
	}

	// Remove all synthetic resources from containers
	removed := make(map[string]bool)
	for _, match := range matches {
		key := fmt.Sprintf("%d/%s", match.ContainerIdx, match.ResourceName)
		if removed[key] {
			continue
		}
		removed[key] = true
		escapedRes := jsonPatchEscape(match.ResourceName)
		patches = append(patches, PatchOp{
			Op:   "remove",
			Path: fmt.Sprintf("/spec/containers/%d/resources/requests/%s", match.ContainerIdx, escapedRes),
		})
		resName := corev1.ResourceName(match.ResourceName)
		if _, ok := pod.Spec.Containers[match.ContainerIdx].Resources.Limits[resName]; ok {
			patches = append(patches, PatchOp{
				Op:   "remove",
				Path: fmt.Sprintf("/spec/containers/%d/resources/limits/%s", match.ContainerIdx, escapedRes),
			})
		}
	}

	// Add resourceClaims and claim refs per composition
	for i, claim := range claims {
		claimName := fmt.Sprintf("composite-%s", claim.deviceClassName)
		if len(claimName) > 63 {
			claimName = strings.TrimRight(claimName[:63], "-")
		}

		claimRef := corev1.PodResourceClaim{
			Name:                      claimName,
			ResourceClaimTemplateName: &claim.templateName,
		}

		if i == 0 && pod.Spec.ResourceClaims == nil {
			patches = append(patches, PatchOp{
				Op:    "add",
				Path:  "/spec/resourceClaims",
				Value: []corev1.PodResourceClaim{claimRef},
			})
		} else {
			patches = append(patches, PatchOp{
				Op:    "add",
				Path:  "/spec/resourceClaims/-",
				Value: claimRef,
			})
		}

		// Add claim references to containers
		pairIdx := 0
		for _, containerIdx := range claim.containerIdxs {
			resName := corev1.ResourceName(claim.resourceName)
			qty := pod.Spec.Containers[containerIdx].Resources.Requests[resName]
			count, _ := qty.AsInt64()

			var claimRefs []corev1.ResourceClaim
			for j := 0; j < int(count); j++ {
				claimRefs = append(claimRefs, corev1.ResourceClaim{
					Name:    claimName,
					Request: fmt.Sprintf("pair-%d", pairIdx),
				})
				pairIdx++
			}

			if pod.Spec.Containers[containerIdx].Resources.Claims == nil {
				patches = append(patches, PatchOp{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/containers/%d/resources/claims", containerIdx),
					Value: claimRefs,
				})
			} else {
				for _, ref := range claimRefs {
					patches = append(patches, PatchOp{
						Op:    "add",
						Path:  fmt.Sprintf("/spec/containers/%d/resources/claims/-", containerIdx),
						Value: ref,
					})
				}
			}
		}
	}

	return patches
}

// jsonPatchEscape escapes / and ~ in JSON Patch paths per RFC 6901.
func jsonPatchEscape(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '~':
			result = append(result, '~', '0')
		case '/':
			result = append(result, '~', '1')
		default:
			result = append(result, s[i])
		}
	}
	return string(result)
}

// PatchesToJSON serializes patches to JSON.
func PatchesToJSON(patches []PatchOp) ([]byte, error) {
	return json.Marshal(patches)
}

// Keep resource import used
var _ = resource.MustParse
