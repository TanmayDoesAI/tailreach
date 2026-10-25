package main

import "time"

// rateCtl steers the encoder on the WebRTC path. It is built for links that drop and
// come back often (5G): cut fast on real congestion, return to the last rate that
// ran clean as soon as the dip clears, and creep above it only while the path stays
// clean.
//
// Congestion is judged from what the path actually does: the uplink queue (round trip
// of a probe on the unordered data channel, minus its recent floor) and packet loss
// (from transport-wide feedback). GCC's own estimate is not used: on an
// application-limited screen stream it sits far below what the link carries.
type rateCtl struct {
	min, max int64
	rate     int64

	good     int64     // the last rate that ran clean for goodAfter: where a dip recovers to
	verified int64     // highest rate actually sent while clean: bounds how far we probe above it
	verAt    time.Time // when verified was last raised (it fades after verifyTTL)

	cutAt int64 // the rate congestion last showed up at: probe gently near it
	cutQ  int64 // queue reading at the last cut

	cleanSince time.Time // zero while congested
	lastCut    time.Time
	lastUp     time.Time
	lastJump   time.Time // last recovery jump to good: a cut right after it means good is stale
	jumpFrom   int64     // the rate that was clean before that jump
}

type rateSample struct {
	now     time.Time
	queueMs int64   // standing queue on the path, ms (-1 = no probe reading)
	loss    float64 // fraction lost, 0..1
	sentBps int64   // what went out in the last second
}

const (
	queueCongested = 80  // ms of standing queue that counts as congestion (5G jitter sits below)
	queueSevere    = 300 // ms: cut harder
	lossCongested  = 0.05
	lossSevere     = 0.15
	cutEvery       = 400 * time.Millisecond // let a cut take effect before judging again
	goodAfter      = 2 * time.Second
	recoverAfter   = 600 * time.Millisecond // clean this long before jumping back up
	recoverEvery   = 400 * time.Millisecond
	probeAfter     = time.Second
	probeEvery     = time.Second
	verifyTTL      = 30 * time.Second
	probeHeadroom  = 2         // probe up to this many times the verified rate
	probeFloor     = 3_000_000 // ...or this, whichever is higher
)

func newRateCtl(min, max, start int64) *rateCtl {
	return &rateCtl{min: min, max: max, rate: start}
}

// step takes one sample (four a second) and returns the encoder bitrate.
func (c *rateCtl) step(s rateSample) int64 {
	congested := s.queueMs > queueCongested || s.loss > lossCongested
	severe := s.queueMs > queueSevere || s.loss > lossSevere

	if congested {
		news := !c.cleanSince.IsZero() || c.lastCut.IsZero() // first congested sample after a clean stretch
		c.cleanSince = time.Time{}
		// Follow-up cuts only while the queue is still growing: one that is
		// draining after the last cut needs no second cut.
		draining := !news && s.now.Sub(c.lastCut) < time.Second && s.queueMs <= c.cutQ && s.loss <= lossCongested
		if s.now.Sub(c.lastCut) >= cutEvery && !draining {
			if news {
				c.cutAt = c.rate
				if c.rate*10 >= c.good*9 {
					// It broke at (or above) the good rate itself, not in a dip well
					// below it: good was optimistic. Recover to just under it.
					c.good = c.rate * 85 / 100
				}
			}
			// A deep queue means we're sending several times what the path carries:
			// get under it in one or two steps. The way back up is fast, so
			// undershooting costs a second, overshooting costs seconds of lag.
			f := int64(75)
			if severe {
				f = 25
			}
			if !c.lastJump.IsZero() && s.now.Sub(c.lastJump) < 1500*time.Millisecond {
				// Jumped back up and it didn't hold: the path is worse than before the
				// dip. What ran clean just before the jump is the new good rate.
				// (cutAt keeps the rate that failed, so the climb back is only gentle
				// near that, not from here.)
				c.good = max(c.min, c.jumpFrom)
				c.lastJump = time.Time{}
			}
			c.rate = max(c.min, c.rate*f/100)
			c.lastCut, c.cutQ = s.now, s.queueMs
		}
		return c.rate
	}

	if c.cleanSince.IsZero() {
		c.cleanSince = s.now
	}
	clean := s.now.Sub(c.cleanSince)
	if clean >= goodAfter && c.rate > c.good {
		c.good = c.rate // only a failed jump back (above) lowers it
	}
	if s.queueMs >= 0 && s.sentBps > c.verified {
		c.verified, c.verAt = s.sentBps, s.now
	} else if s.now.Sub(c.verAt) > verifyTTL {
		c.verified, c.verAt = max(s.sentBps, c.verified*9/10), s.now
	}

	switch {
	case c.rate < c.good && clean >= recoverAfter && s.now.Sub(c.lastUp) >= recoverEvery:
		// A dip has cleared: straight back toward the rate that held before it.
		c.jumpFrom = c.rate
		c.rate = min(c.good, c.rate*2)
		c.lastUp, c.lastJump = s.now, s.now
	case c.rate >= c.good && clean >= probeAfter && s.now.Sub(c.lastUp) >= probeEvery && s.queueMs >= 0:
		ceiling := min(c.max, max(int64(probeFloor), c.verified*probeHeadroom))
		next := c.rate * 12 / 10
		if c.cutAt > 0 {
			// Fast up to just under where it last broke, then edge past it.
			if edge := c.cutAt * 95 / 100; c.rate < edge {
				next = min(next, edge)
			} else {
				next = c.rate * 105 / 100
			}
		}
		if c.rate < ceiling {
			c.rate = min(ceiling, next)
			c.lastUp = s.now
		}
		if c.good > c.cutAt*11/10 {
			c.cutAt = 0 // held clean well above it: that limit is gone
		}
	}
	return c.rate
}

// rttFloor turns probe round trips into a standing-queue reading: the latest round
// trip (averaged over two samples) minus the best one in the last floorWindow. When
// the path itself changes (Wi-Fi to 5G), every sample is higher and that would read
// as a queue forever, so a floor that even the best recent samples stay well above
// for rebaseAfter is replaced by the recent best.
type rttFloor struct {
	samples  []rttSample
	elevated time.Time
}

type rttSample struct {
	at  time.Time
	rtt int64
}

const (
	floorWindow = 10 * time.Second
	rebaseAfter = 4 * time.Second
	recentSpan  = 2 * time.Second
)

func (f *rttFloor) add(now time.Time, rtt int64) {
	f.samples = append(f.samples, rttSample{now, rtt})
	i := 0
	for i < len(f.samples) && now.Sub(f.samples[i].at) > floorWindow {
		i++
	}
	f.samples = f.samples[i:]

	floor, recent := f.min(now, floorWindow), f.min(now, recentSpan)
	if recent-floor > queueCongested {
		if f.elevated.IsZero() {
			f.elevated = now
		} else if now.Sub(f.elevated) >= rebaseAfter {
			j := 0
			for j < len(f.samples) && now.Sub(f.samples[j].at) > recentSpan {
				j++
			}
			f.samples = f.samples[j:]
			f.elevated = time.Time{}
		}
	} else {
		f.elevated = time.Time{}
	}
}

func (f *rttFloor) min(now time.Time, span time.Duration) int64 {
	m := int64(-1)
	for _, s := range f.samples {
		if now.Sub(s.at) <= span && (m < 0 || s.rtt < m) {
			m = s.rtt
		}
	}
	return m
}

// queue is the standing queue in ms, or -1 with no recent sample.
func (f *rttFloor) queue(now time.Time) int64 {
	n := len(f.samples)
	if n == 0 || now.Sub(f.samples[n-1].at) > 1500*time.Millisecond {
		return -1
	}
	last := f.samples[n-1].rtt
	if n >= 2 {
		last = (last + f.samples[n-2].rtt) / 2
	}
	return max(0, last-f.min(now, floorWindow))
}
