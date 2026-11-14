// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"sync"
	"time"
)

// Tuning knobs for layerSelector.
const (
	// lossDowngradeThreshold triggers an immediate downgrade. Loss means a
	// queue is building, and every frame sent above capacity adds latency,
	// so this path never waits.
	lossDowngradeThreshold = 0.10

	// lossUpgradeThreshold gates upgrades. The gap between the two loss
	// thresholds is a hysteresis band: a link hovering between them keeps
	// its current layer instead of bouncing.
	lossUpgradeThreshold = 0.02

	// upgradeMargin requires the bandwidth estimate to exceed a layer's
	// bitrate by this factor before switching up to it. It absorbs
	// estimation error and leaves room for audio, retransmissions and RTCP.
	// Together with the downgrade logic this forms the bandwidth
	// hysteresis: a layer is dropped when the estimate falls below its
	// bitrate, but only re-entered above 1.25x its bitrate.
	upgradeMargin = 1.25

	// A better layer must keep qualifying for a dwell period before we
	// switch up to it. The dwell scales inversely with headroom: a link
	// that barely qualifies waits maxUpgradeDwell, while one with ample
	// headroom (a genuine recovery) upgrades after minUpgradeDwell.
	minUpgradeDwell = 500 * time.Millisecond
	maxUpgradeDwell = 4 * time.Second
)

// simulcastLayer describes one simulcast stream the remote sends us.
type simulcastLayer struct {
	rid string
	// bitrate is the layer's average bitrate in bits per second. Approximate
	// values are fine, they only need to track the encoder's real output.
	bitrate int
}

// layerSelector decides which simulcast layer to forward. It combines two
// signals instead of switching on loss alone:
//
//   - Packet loss is the fast, reactive signal. Crossing
//     lossDowngradeThreshold downgrades immediately to protect latency.
//   - The congestion controller's bandwidth estimate drives recovery.
//     Upgrades require headroom (upgradeMargin) sustained for a dwell
//     period, so a single good RTCP interval doesn't flip us up, but the
//     dwell shrinks as headroom grows, so a genuine recovery is quick.
//
// layerSelector is safe for concurrent use.
type layerSelector struct {
	mu      sync.Mutex
	layers  []simulcastLayer // ascending bitrate
	current int

	upgradeCandidate int
	candidateSince   time.Time
}

// newLayerSelector returns a layerSelector that starts on the lowest layer.
// Starting low avoids an initial overshoot that would add queuing delay;
// the adaptive dwell lets us climb back within about a second once the
// estimate shows real headroom.
func newLayerSelector(layers []simulcastLayer) *layerSelector {
	return &layerSelector{layers: layers}
}

// update folds the latest network signals into the forwarding decision and
// returns the index of the layer that should be forwarded now.
//
// lossRatio is the fraction of packets lost as reported by the receiver
// (RemoteInboundRTPStreamStats.FractionLost). bandwidthEstimate is the
// congestion controller's estimate of available send bandwidth in bits per
// second; pass 0 while no estimate is available yet.
func (s *layerSelector) update(lossRatio float64, bandwidthEstimate int, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if target, ok := s.downgradeTarget(lossRatio, bandwidthEstimate); ok {
		s.current = target
		s.upgradeCandidate = target
		s.candidateSince = now

		return s.current
	}

	return s.maybeUpgrade(lossRatio, bandwidthEstimate, now)
}

// currentRID returns the RID of the layer that should be forwarded.
func (s *layerSelector) currentRID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.layers[s.current].rid
}

// downgradeTarget computes the layer to drop to. The second return value
// reports whether a downgrade should happen at all.
func (s *layerSelector) downgradeTarget(lossRatio float64, bandwidthEstimate int) (int, bool) {
	lossy := lossRatio >= lossDowngradeThreshold
	overEstimate := bandwidthEstimate > 0 && s.layers[s.current].bitrate > bandwidthEstimate
	if !lossy && !overEstimate {
		return s.current, false
	}

	target := s.current
	if bandwidthEstimate > 0 {
		// Drop straight to the highest layer the estimate sustains instead
		// of stepping down one layer per RTCP interval. The upgrade margin
		// provides the hysteresis that keeps this from oscillating.
		target = 0
		for i := range s.layers {
			if s.layers[i].bitrate <= bandwidthEstimate {
				target = i
			}
		}
		if target > s.current {
			target = s.current
		}
	}
	// Always react to loss, even if the estimate is missing or stale.
	if lossy && target == s.current {
		target = s.current - 1
	}
	if target < 0 {
		target = 0
	}

	return target, target != s.current
}

// maybeUpgrade handles the recovery path. Callers must hold s.mu.
func (s *layerSelector) maybeUpgrade(lossRatio float64, bandwidthEstimate int, now time.Time) int {
	target := s.current
	if bandwidthEstimate > 0 {
		for i := s.current + 1; i < len(s.layers); i++ {
			if float64(s.layers[i].bitrate)*upgradeMargin <= float64(bandwidthEstimate) {
				target = i
			}
		}
	}

	if target == s.current || lossRatio > lossUpgradeThreshold {
		// Nothing better qualifies right now. Reset the dwell clock so a
		// later recovery still has to prove itself continuously.
		s.upgradeCandidate = s.current
		s.candidateSince = now

		return s.current
	}

	if target != s.upgradeCandidate {
		s.upgradeCandidate = target
		s.candidateSince = now

		return s.current
	}

	// The same better layer has qualified for a while. The dwell we demand
	// scales inversely with headroom: barely enough bandwidth must prove
	// itself, abundant bandwidth (a real recovery) upgrades almost at once
	// and may skip intermediate layers in a single step.
	headroom := float64(bandwidthEstimate) / float64(s.layers[target].bitrate)
	dwell := time.Duration(float64(maxUpgradeDwell) / headroom)
	if dwell < minUpgradeDwell {
		dwell = minUpgradeDwell
	}
	if now.Sub(s.candidateSince) < dwell {
		return s.current
	}

	s.current = target

	return s.current
}
