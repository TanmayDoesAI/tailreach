package main

import (
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
)

// simLink is a fluid model of a bottleneck: the encoder sends min(rate, demand), the
// link drains at capacity, anything above it queues (tail drop past maxQueue), and a
// probe sees base RTT + queue + jitter, one tick late, like the real probe.
type simLink struct {
	capacity func(t time.Duration) int64 // bits/s
	baseRTT  func(t time.Duration) int64 // ms
	demand   func(t time.Duration) int64 // what the content needs, bits/s
	jitterMs int64
}

type simResult struct {
	t       []time.Duration
	rate    []int64
	queueMs []int64
	sent    []int64
	lossPct []float64
}

const tick = 250 * time.Millisecond

func runSim(l simLink, d time.Duration, seed int64) simResult {
	rng := rand.New(rand.NewSource(seed))
	c := newRateCtl(500_000, 20_000_000, 3_000_000)
	var f rttFloor
	start := time.Unix(1_000_000, 0)
	var r simResult
	queueBits, loss, prevQ := 0.0, 0.0, int64(-1)
	sentWin := []int64{}
	for t := time.Duration(0); t < d; t += tick {
		now := start.Add(t)
		capBps := float64(l.capacity(t))
		send := float64(min(c.rate, l.demand(t)))
		queueBits += (send - capBps) * tick.Seconds()
		loss = 0
		maxQ := capBps * 0.5 // half a second of buffer
		if queueBits > maxQ {
			loss = (queueBits - maxQ) / (send * tick.Seconds())
			queueBits = maxQ
		}
		queueBits = max(queueBits, 0)
		qMs := int64(queueBits / capBps * 1000)
		jit := int64(0)
		if l.jitterMs > 0 {
			jit = rng.Int63n(l.jitterMs)
		}
		f.add(now, l.baseRTT(t)+qMs+jit)

		sentWin = append(sentWin, int64(send))
		if len(sentWin) > 4 {
			sentWin = sentWin[1:]
		}
		var sentBps int64
		for _, s := range sentWin {
			sentBps += s
		}
		sentBps /= int64(len(sentWin))

		c.step(rateSample{now: now, queueMs: prevQ, loss: loss, sentBps: sentBps})
		prevQ = f.queue(now)

		r.t = append(r.t, t)
		r.rate = append(r.rate, c.rate)
		r.queueMs = append(r.queueMs, qMs)
		r.sent = append(r.sent, int64(send))
		r.lossPct = append(r.lossPct, loss*100)
	}
	return r
}

func flat(v int64) func(time.Duration) int64 { return func(time.Duration) int64 { return v } }

func (r simResult) window(from, to time.Duration) (rates, queues []int64) {
	for i, t := range r.t {
		if t >= from && t < to {
			rates = append(rates, r.rate[i])
			queues = append(queues, r.queueMs[i])
		}
	}
	return
}

func maxOf(xs []int64) int64 {
	m := int64(0)
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

func pct(xs []int64, p float64) int64 {
	s := append([]int64(nil), xs...)
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s[int(p*float64(len(s)-1))]
}

// Wi-Fi with room to spare and heavy content: climb to a sharp rate, no queue.
func TestRateWiFiClimbs(t *testing.T) {
	r := runSim(simLink{capacity: flat(40_000_000), baseRTT: flat(8), demand: flat(12_000_000), jitterMs: 10}, 30*time.Second, 1)
	rates, queues := r.window(10*time.Second, 30*time.Second)
	if pct(rates, 0.5) < 10_000_000 {
		t.Errorf("rate p50 after 10 s = %d, want ≥ 10M", pct(rates, 0.5))
	}
	if pct(queues, 0.95) > 50 {
		t.Errorf("queue p95 = %d ms, want ≤ 50", pct(queues, 0.95))
	}
}

// 5G that keeps dipping: 8 Mbit/s with a 2 s dip to 1 Mbit/s every 8 s. The queue must
// stay bounded through each dip and the rate must be back within 2 s of each recovery.
func TestRate5GDips(t *testing.T) {
	capFn := func(t time.Duration) int64 {
		if t > 5*time.Second && (t%(8*time.Second)) < 2*time.Second {
			return 1_000_000
		}
		return 8_000_000
	}
	r := runSim(simLink{capacity: capFn, baseRTT: flat(40), demand: flat(12_000_000), jitterMs: 30}, 60*time.Second, 2)
	// An 8× drop fills the bottleneck before any signal can come back; what matters is
	// that the queue drains within the dip and none stands between dips.
	for dip := 16 * time.Second; dip < 56*time.Second; dip += 8 * time.Second {
		end := dip + 2*time.Second
		if _, q := r.window(dip+1500*time.Millisecond, dip+2*time.Second); maxOf(q) > 100 {
			t.Errorf("dip at %v: queue still %d ms after 1.5 s", dip, maxOf(q))
		}
		if _, q := r.window(end, dip+8*time.Second); pct(q, 0.9) > 80 {
			t.Errorf("between dips after %v: queue p90 %d ms, want ≤ 80", end, pct(q, 0.9))
		}
		rates, _ := r.window(end+2*time.Second, end+3*time.Second)
		if pct(rates, 0.5) < 4_000_000 {
			t.Errorf("2 s after the dip ending at %v: rate %d, want ≥ 4M (half the path)", end, pct(rates, 0.5))
		}
	}
}

// The path gets permanently worse (8 → 2 Mbit/s): settle near it, queue drains.
func TestRateStepDown(t *testing.T) {
	capFn := func(t time.Duration) int64 {
		if t > 15*time.Second {
			return 2_000_000
		}
		return 8_000_000
	}
	r := runSim(simLink{capacity: capFn, baseRTT: flat(40), demand: flat(12_000_000), jitterMs: 20}, 45*time.Second, 3)
	rates, queues := r.window(25*time.Second, 45*time.Second)
	if q := pct(queues, 0.9); q > 150 {
		t.Errorf("queue p90 after the drop = %d ms, want ≤ 150", q)
	}
	if p := pct(rates, 0.5); p < 1_000_000 || p > 3_000_000 {
		t.Errorf("rate p50 after the drop = %d, want 1–3M", p)
	}
}

// Wi-Fi → 5G without a reconnect: the round trip jumps 10 → 60 ms with plenty of
// bandwidth. That is not a queue; the rate must not stay at the floor.
func TestRatePathChange(t *testing.T) {
	baseFn := func(t time.Duration) int64 {
		if t > 15*time.Second {
			return 60
		}
		return 10
	}
	r := runSim(simLink{capacity: flat(30_000_000), baseRTT: baseFn, demand: flat(8_000_000), jitterMs: 15}, 40*time.Second, 4)
	rates, _ := r.window(30*time.Second, 40*time.Second)
	if p := pct(rates, 0.5); p < 4_000_000 {
		t.Errorf("rate p50 10 s after the path change = %d, want ≥ 4M", p)
	}
}

// A still screen, then heavy motion: probing above a rate that was never used must not
// overshoot into a deep queue when the content suddenly needs it.
func TestRateLightThenHeavy(t *testing.T) {
	demand := func(t time.Duration) int64 {
		if t > 20*time.Second {
			return 15_000_000
		}
		return 400_000
	}
	r := runSim(simLink{capacity: flat(4_000_000), baseRTT: flat(30), demand: demand, jitterMs: 20}, 40*time.Second, 5)
	_, queues := r.window(20*time.Second, 40*time.Second)
	if q := maxOf(queues); q > 500 {
		t.Errorf("queue max after motion starts = %d ms, want ≤ 500", q)
	}
	if q := pct(queues[8:], 0.9); q > 150 {
		t.Errorf("queue p90 after motion settles = %d ms, want ≤ 150", q)
	}
}

// A blackout (tunnel, lift): no capacity for 3 s. Come back fast afterwards.
func TestRateBlackout(t *testing.T) {
	capFn := func(t time.Duration) int64 {
		if t > 15*time.Second && t < 18*time.Second {
			return 1 // effectively nothing
		}
		return 8_000_000
	}
	r := runSim(simLink{capacity: capFn, baseRTT: flat(40), demand: flat(12_000_000), jitterMs: 20}, 30*time.Second, 6)
	rates, _ := r.window(21*time.Second, 23*time.Second)
	if p := pct(rates, 0.5); p < 4_000_000 {
		t.Errorf("rate 3–5 s after the blackout = %d, want ≥ 4M", p)
	}
}

func TestRttFloorQueue(t *testing.T) {
	var f rttFloor
	now := time.Unix(0, 0)
	for i := 0; i < 20; i++ {
		f.add(now, 40)
		now = now.Add(tick)
	}
	f.add(now, 240)
	f.add(now.Add(tick), 240)
	if q := f.queue(now.Add(tick)); q != 200 {
		t.Fatalf("queue = %d, want 200", q)
	}
	if q := f.queue(now.Add(5 * time.Second)); q != -1 {
		t.Fatalf("stale queue = %d, want -1", q)
	}
}

func TestNackLimiter(t *testing.T) {
	var rate atomic.Int64
	rate.Store(960_000) // budget: 30 packets a second
	l := &nackLimiter{rateBps: &rate, sent: map[uint16]nackRec{}}
	now := time.Unix(0, 0)
	ask := func(seqs ...uint16) []uint16 {
		nk := &rtcp.TransportLayerNack{Nacks: rtcp.NackPairsFromSequenceNumbers(seqs)}
		l.trim(nk, now)
		var out []uint16
		for _, p := range nk.Nacks {
			out = append(out, p.PacketList()...)
		}
		return out
	}
	if got := ask(1, 2, 3); len(got) != 3 {
		t.Fatalf("first ask: resend %v, want all three", got)
	}
	if got := ask(1, 2, 3); len(got) != 0 {
		t.Fatalf("same packets within a round trip: resend %v, want none", got)
	}
	now = now.Add(200 * time.Millisecond)
	if got := ask(1); len(got) != 1 {
		t.Fatalf("second resend: %v, want it", got)
	}
	now = now.Add(200 * time.Millisecond)
	if got := ask(1); len(got) != 0 {
		t.Fatalf("third resend of one packet: %v, want none", got)
	}
	now = now.Add(time.Second)
	var many []uint16
	for i := uint16(100); i < 400; i++ {
		many = append(many, i)
	}
	if got := ask(many...); len(got) > 31 {
		t.Fatalf("a storm of 300 asks: resent %d, want ≤ the 30/s budget", len(got))
	}
}
