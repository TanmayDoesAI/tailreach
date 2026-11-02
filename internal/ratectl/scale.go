package ratectl

import "time"

// Scaler picks the capture resolution for the bitrate the link carries. At a low bitrate
// full-size text turns to mush; fewer, sharper pixels read better (Parsec and Jump do the
// same). The factor is of the size the viewer asked for, so it never goes above it.
//
// It only steps down while the link has shown a limit (limited): a still screen sends
// little and the controller then sits at its start rate without the link being the
// reason. Steps are slow and sticky so the picture doesn't keep changing size: each one
// costs a keyframe, and the viewer sees the text re-flow its sharpness.
type Scaler struct {
	HEVC bool // HEVC holds detail at about 30% fewer bits: lower thresholds

	level        int       // index into scaleLevels
	changed      time.Time // last change: none again within scaleDwell
	below, above time.Time // since when the rate has been past a step down / step up threshold (zero = not)
}

var scaleLevels = []float64{1, 0.75, 0.5}

// H.264 thresholds, bits/s. Below scaleDown[i] leaves level i for i+1; above scaleUp[i]
// goes back from level i+1 to i. The gaps between them are the hysteresis.
var (
	scaleDown = []int64{4_000_000, 1_500_000}
	scaleUp   = []int64{6_000_000, 2_500_000}
)

const (
	scaleDwell     = 4 * time.Second // at least this long at a level
	scaleDownAfter = 2 * time.Second // below a threshold this long: step down (most 5G dips are over sooner)
	scaleUpAfter   = 6 * time.Second // above one this long: step back up, one level at a time
	hevcScale      = 70              // % of the H.264 thresholds
)

func NewScaler() *Scaler { return &Scaler{} }

// Factor is the current scale, 0..1 of the viewer's size.
func (s *Scaler) Factor() float64 { return scaleLevels[s.level] }

// Step takes the encoder bitrate (four times a second) and returns the capture scale.
func (s *Scaler) Step(now time.Time, rate int64, limited bool) float64 {
	if !limited {
		rate = 1 << 62 // no known limit: the rate is low because the screen is still
	}
	th := func(v int64) int64 {
		if s.HEVC {
			return v * hevcScale / 100
		}
		return v
	}
	settled := now.Sub(s.changed) >= scaleDwell

	// Down: straight to the level this rate calls for (a deep dip skips 75%).
	want := s.level
	for want < len(scaleDown) && rate < th(scaleDown[want]) {
		want++
	}
	if want > s.level {
		s.above = time.Time{}
		if s.below.IsZero() {
			s.below = now
		}
		if settled && now.Sub(s.below) >= scaleDownAfter {
			s.set(now, want)
		}
		return s.Factor()
	}
	s.below = time.Time{}

	// Up: one level at a time.
	if s.level > 0 && rate > th(scaleUp[s.level-1]) {
		if s.above.IsZero() {
			s.above = now
		}
		if settled && now.Sub(s.above) >= scaleUpAfter {
			s.set(now, s.level-1)
		}
	} else {
		s.above = time.Time{}
	}
	return s.Factor()
}

func (s *Scaler) set(now time.Time, level int) {
	s.level, s.changed = level, now
	s.below, s.above = time.Time{}, time.Time{}
}
