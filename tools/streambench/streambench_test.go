package main

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	got := pct([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.5)
	if got != 5 {
		t.Errorf("p50 of 1..10 = %v, want 5", got)
	}
	if pct(nil, 0.5) != 0 {
		t.Error("empty should be 0")
	}
	if got := pct([]float64{3}, 0.99); got != 3 {
		t.Errorf("single = %v", got)
	}
}

func TestSeqTrackerLossAndWrap(t *testing.T) {
	var s seqTracker
	for i := 0; i < 100; i++ {
		s.push(uint16(i))
	}
	if s.lossPct() != 0 {
		t.Errorf("loss = %v, want 0", s.lossPct())
	}
	s.push(103) // 3 missing: 100, 101, 102
	if got := s.lossPct(); got < 2.8 || got > 2.95 {
		t.Errorf("loss = %v, want ~2.9", got)
	}
	// Wrap: highest = 65534, then 1 (wrap +3 → 2 missing: 65535, 0).
	var w seqTracker
	for i := 65530; i < 65535; i++ {
		w.push(uint16(i))
	}
	w.push(1)
	if w.gaps != 2 {
		t.Errorf("wrap gaps = %d, want 2", w.gaps)
	}
}

func TestNALTypes(t *testing.T) {
	// Single NAL: IDR (5) and non-IDR (1).
	if !hasIDR([]byte{0x65, 0x00}) {
		t.Error("single NAL type 5 should be IDR")
	}
	if hasIDR([]byte{0x41, 0x00}) {
		t.Error("single NAL type 1 is not IDR")
	}
	// FU-A carrying an IDR: indicator type 28, FU header start|type 5.
	if !hasIDR([]byte{0x7c, 0x85}) {
		t.Error("FU-A start of IDR should count")
	}
	if hasIDR([]byte{0x7c, 0x05}) {
		t.Error("FU-A continuation of IDR should not count as a new keyframe start")
	}
	// STAP-A with SPS(7)+PPS(8): no IDR.
	stap := []byte{0x78, 0x00, 0x02, 0x67, 0x11, 0x00, 0x01, 0x68}
	if hasIDR(stap) {
		t.Error("STAP-A with SPS/PPS only is not an IDR")
	}
	// STAP-A with PPS+IDR: 1-byte NALs.
	stap = []byte{0x78, 0x00, 0x01, 0x68, 0x00, 0x01, 0x65}
	if !hasIDR(stap) {
		t.Error("STAP-A containing an IDR should count")
	}
}

func TestRewriteOfferAndAnswer(t *testing.T) {
	offer := "v=0\r\na=candidate:1 1 udp 2130706431 192.0.2.10 53825 typ host\r\n"
	got, ok := rewriteOffer(offer, 53825, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 9), Port: 60001})
	if !ok || got != "v=0\r\na=candidate:1 1 udp 2130706431 192.168.1.9 60001 typ host\r\n" {
		t.Errorf("rewriteOffer = %q ok=%v", got, ok)
	}
	if _, ok := rewriteOffer("v=0\r\n", 53825, &net.UDPAddr{}); ok {
		t.Error("no candidate should report ok=false")
	}
	answer := "a=candidate:8 1 udp 1694498815 192.0.2.20 50050 typ host ufrag abc\r\n"
	got, ok = rewriteAnswer(answer, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 30), Port: 41111})
	if !ok || got != "a=candidate:8 1 udp 1694498815 192.0.2.30 41111 typ host ufrag abc\r\n" {
		t.Errorf("rewriteAnswer = %q ok=%v", got, ok)
	}
}

func TestTokenBucketDelaysAndTailDrop(t *testing.T) {
	s := newScheduler()
	p := newPath(0, 0, 0, 8000, 1000, nil, s) // 1 KB/s, 1 KB queue
	now := time.Now()
	// First 1000 bytes pass instantly (burst = queue size).
	if d := p.apply(500, now); d < 0 {
		t.Fatalf("first packet dropped: %v", d)
	}
	if d := p.apply(500, now); d < 0 {
		t.Fatalf("second packet dropped: %v", d)
	}
	// Next packets must wait for tokens…
	d := p.apply(500, now)
	if d <= 0 {
		t.Errorf("third packet should be delayed, got %v", d)
	}
	// …and once the queue is full they tail-drop.
	drops := 0
	for i := 0; i < 10; i++ {
		if p.apply(500, now) < 0 {
			drops++
		}
	}
	if drops == 0 {
		t.Error("expected tail drops when the queue is full")
	}
}

func TestTokenBucketRefillKeepsOrder(t *testing.T) {
	s := newScheduler()
	p := newPath(0, 0, 0, 80_000, 4000, nil, s) // 10 KB/s, 4 KB queue = burst
	now := time.Now()
	var delays []time.Duration
	for i := 0; i < 6; i++ {
		d := p.apply(1000, now) // first four spend the burst, the rest queue up in order
		if d < 0 {
			t.Fatalf("unexpected drop")
		}
		delays = append(delays, d)
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] < delays[i-1] {
			t.Errorf("packets reordered: %v", delays)
		}
	}
	if delays[4] < 50*time.Millisecond { // the 5th has no credit: ~1000 B / 10 KB/s
		t.Errorf("5th packet should wait ~100ms: %v", delays)
	}
	if delays[5] <= delays[4] {
		t.Errorf("6th should wait longer than 5th: %v", delays)
	}
}

func TestBlackoutDrop(t *testing.T) {
	s := newScheduler()
	b := blackout{start: time.Now().Add(-time.Second), dur: time.Hour}
	p := newPath(0, 0, 0, 0, 1000, []blackout{b}, s)
	if d := p.apply(10, time.Now()); d >= 0 {
		t.Errorf("packet during blackout should drop, got %v", d)
	}
}

func TestRateChangeSchedule(t *testing.T) {
	s := newScheduler()
	p := newPath(0, 0, 0, 1_000_000, 4000, nil, s)
	p.mu.Lock()
	p.changes = []rateChange{{at: time.Now().Add(-time.Minute), bps: 8_000}}
	p.mu.Unlock()
	p.apply(100, time.Now())
	if p.bps != 8_000 {
		t.Errorf("rate change not applied: %d", p.bps)
	}
}

// TestLatNormalizeCentersOnMedian checks the RTC-style latency normalization.
func TestLatNormalizeCentersOnMedian(t *testing.T) {
	l := latTracker{raw: []float64{110.0, 115.0, 130.0}}
	l.normalizeRTC()
	if l.floor() != 115 {
		t.Errorf("base = %v, want 115 (median)", l.floor())
	}
	sum := l.summary()
	if sum["p50"] != 0 {
		t.Errorf("p50 = %v, want 0", sum["p50"])
	}
}

func TestSchedFlag(t *testing.T) {
	var f schedFlag
	if err := f.Set("5s:1.5Mbps,12s:300kbps"); err != nil {
		t.Fatal(err)
	}
	if len(f) != 2 || f[0].at != 5*time.Second || f[0].bps != 1_500_000 || f[1].bps != 300_000 {
		t.Errorf("parsed %+v", f)
	}
	if err := f.Set("bogus"); err == nil {
		t.Error("bogus input should error")
	}
}

func TestReportSecondMerging(t *testing.T) {
	var r report
	r.second(3).Frames = 10
	r.second(3).Bytes = 100
	if len(r.Series) != 1 || r.Series[0].Frames != 10 || r.Series[0].Bytes != 100 {
		t.Errorf("series = %+v", r.Series)
	}
	s := fmt.Sprint(r)
	if s == "" {
		t.Error("report should stringify")
	}
}

// A tail-dropped datagram must not use link time: after an overload ends, the
// bottleneck carries its full rate again at once (no debt from what it dropped).
func TestDropsCostNoTokens(t *testing.T) {
	now := time.Now()
	p := newPath(0, 0, 0, 1_000_000, 10_000, nil, nil) // 1 Mbit/s, 10 kB buffer
	p.last = now
	for i := 0; i < 200; i++ { // 200 kB offered at once: burst, buffer, then drops
		if _, queued := p.admit(1000, now); queued {
			defer p.sent(1000)
		}
	}
	later := now.Add(200 * time.Millisecond) // credit for 25 kB: the buffer drained long ago
	p.queue = 0
	if d, _ := p.admit(1000, later); d < 0 || d > 20*time.Millisecond {
		t.Fatalf("after the overload: delay %v, want a prompt send", d)
	}
}

// Datagrams that go straight out on credit never enter the queue, so they must not
// be released from it: otherwise the queue runs negative and never tail-drops.
func TestQueueOnlyCountsWaiting(t *testing.T) {
	now := time.Now()
	p := newPath(0, 0, 0, 1_000_000, 10_000, nil, nil)
	p.last = now
	for i := 0; i < 10; i++ {
		if _, queued := p.admit(1000, now); queued {
			t.Fatalf("datagram %d went out on credit but counts as queued", i)
		}
	}
	if _, queued := p.admit(1000, now); !queued {
		t.Fatal("datagram past the burst should wait in the queue")
	}
}
