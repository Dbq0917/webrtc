// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var testLayers = []simulcastLayer{ //nolint:gochecknoglobals
	{rid: "q", bitrate: 200_000},
	{rid: "h", bitrate: 700_000},
	{rid: "f", bitrate: 2_000_000},
}

// drive selector to the top layer so downgrade behavior can be tested.
func topLayerSelector(t *testing.T, now time.Time) *layerSelector {
	t.Helper()

	selector := newLayerSelector(testLayers)
	assert.Equal(t, 0, selector.update(0, 20_000_000, now))
	// 20Mbps estimate against the 2Mbps top layer means a minimum dwell.
	assert.Equal(t, 2, selector.update(0, 20_000_000, now.Add(minUpgradeDwell)))

	return selector
}

func TestLayerSelectorDowngradesImmediatelyOnLoss(t *testing.T) {
	now := time.Now()
	selector := topLayerSelector(t, now)

	// A single bad report drops us without any dwell.
	layer := selector.update(0.20, 20_000_000, now.Add(2*minUpgradeDwell))
	assert.Equal(t, 1, layer)
}

func TestLayerSelectorDowngradeFollowsEstimate(t *testing.T) {
	now := time.Now()
	selector := topLayerSelector(t, now)

	// The estimate can no longer sustain the top layer: drop directly to
	// the layer it does sustain, not blindly to the bottom.
	layer := selector.update(0, 800_000, now.Add(time.Second))
	assert.Equal(t, 1, layer)

	// Estimate collapses below every layer: now we do hit the bottom.
	layer = selector.update(0, 100_000, now.Add(2*time.Second))
	assert.Equal(t, 0, layer)
}

func TestLayerSelectorLossHysteresisBand(t *testing.T) {
	now := time.Now()
	selector := newLayerSelector(testLayers)

	// Loss between the two thresholds must neither downgrade nor upgrade,
	// even with a huge estimate and a long wait.
	for i := range 10 {
		layer := selector.update(0.05, 20_000_000, now.Add(time.Duration(i)*time.Hour))
		assert.Equal(t, 0, layer)
	}
}

func TestLayerSelectorUpgradeRequiresDwell(t *testing.T) {
	now := time.Now()
	selector := newLayerSelector(testLayers)

	// 2.6Mbps qualifies for the top layer (2Mbps * 1.25 = 2.5Mbps) with a
	// headroom of 1.3, so the dwell is 4s/1.3 ~= 3.1s.
	assert.Equal(t, 0, selector.update(0, 2_600_000, now))
	assert.Equal(t, 0, selector.update(0, 2_600_000, now.Add(2*time.Second)))
	assert.Equal(t, 2, selector.update(0, 2_600_000, now.Add(4*time.Second)))
}

func TestLayerSelectorFastRecoveryWithAmpleHeadroom(t *testing.T) {
	now := time.Now()
	selector := newLayerSelector(testLayers)

	// A genuine recovery (estimate far above the top layer) recovers after
	// minUpgradeDwell and skips intermediate layers in one step.
	assert.Equal(t, 0, selector.update(0, 20_000_000, now))
	assert.Equal(t, 0, selector.update(0, 20_000_000, now.Add(minUpgradeDwell/2)))
	assert.Equal(t, 2, selector.update(0, 20_000_000, now.Add(minUpgradeDwell)))
}

func TestLayerSelectorFlappingEstimateDoesNotOscillate(t *testing.T) {
	now := time.Now()
	selector := newLayerSelector(testLayers)

	// An estimate alternating around the top layer's margin keeps
	// resetting the dwell clock, so we never bounce up and back down.
	for i := range 20 {
		estimate := 2_600_000
		if i%2 == 1 {
			estimate = 2_400_000
		}
		layer := selector.update(0, estimate, now.Add(time.Duration(i)*time.Second))
		assert.Equal(t, 0, layer)
	}
}

func TestLayerSelectorUpgradeResetByLossSpike(t *testing.T) {
	now := time.Now()
	selector := newLayerSelector(testLayers)

	assert.Equal(t, 0, selector.update(0, 20_000_000, now))
	// A loss spike inside the dwell window restarts it.
	assert.Equal(t, 0, selector.update(0.05, 20_000_000, now.Add(minUpgradeDwell)))
	assert.Equal(t, 0, selector.update(0, 20_000_000, now.Add(minUpgradeDwell+time.Millisecond)))
	assert.Equal(t, 0, selector.update(0, 20_000_000, now.Add(2*minUpgradeDwell)))
	assert.Equal(t, 2, selector.update(0, 20_000_000, now.Add(2*minUpgradeDwell+2*time.Millisecond)))
}

func TestLayerSelectorCurrentRID(t *testing.T) {
	now := time.Now()
	selector := newLayerSelector(testLayers)
	assert.Equal(t, "q", selector.currentRID())

	selector.update(0, 20_000_000, now)
	selector.update(0, 20_000_000, now.Add(minUpgradeDwell))
	assert.Equal(t, "f", selector.currentRID())
}
