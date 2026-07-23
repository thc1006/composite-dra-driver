// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package shadow

import (
	"strings"
	"testing"

	"github.com/openshift-psap/composite-dra-driver/pkg/store"
)

func TestShadowClaimName_MaxLength(t *testing.T) {
	tests := []struct {
		name          string
		compositeName string
		member        *store.DeviceMember
	}{
		{
			name:          "short name",
			compositeName: "my-claim",
			member:        &store.DeviceMember{SourceName: "gpu", Device: "dev0"},
		},
		{
			name:          "long source and device names",
			compositeName: "composite-claim-abc123",
			member:        &store.DeviceMember{SourceName: "very-long-gpu-driver-name", Device: "device-with-a-really-long-identifier-name"},
		},
		{
			name:          "realistic long claim",
			compositeName: "llm-d-pd-d-x2-p-tp1-d-tp4-p-x2-kserve-759d8cccc5-composite-gpu-nic-pair",
			member:        &store.DeviceMember{SourceName: "nic-driver", Device: "mlx5-0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shadowClaimName(tt.compositeName, tt.member)
			if len(got) > 63 {
				t.Errorf("name too long: %d chars: %s", len(got), got)
			}
			if strings.HasSuffix(got, "-") {
				t.Errorf("name ends with dash: %s", got)
			}
		})
	}
}
