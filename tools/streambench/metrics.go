package main

// Metric collection: capture-to-delivery latency per frame, frame pacing, loss,
// input round trips, keyframe recovery, and the per-second series the summary is
// computed from.

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ---- percentiles ----

func pct(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	i := int(p*float64(len(s))+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

// ---- latency (WebSocket path: exact; RTC path: relative to the run's floor) ----

type latTracker struct {
	raw  []float64 // ms; in rtc mode relative to an unknown RTP base
	base float64   // ms subtracted (median in rtc mode: the startup burst would fake a floor)
}

// normalizeRTC centers the series on its median: the RTP timestamp base is unknown,
// and at cold start the capture pipeline flushes a backlog burst whose raw latencies
// would otherwise look like a huge negative floor.
func (l *latTracker) normalizeRTC() {
	if len(l.raw) == 0 {
		return
	}
	l.base = pct(l.raw, 0.5)
	for i := range l.raw {
		l.raw[i] -= l.base
	}
}

func (l *latTracker) floor() float64 { return l.base }

func (l *latTracker) summary() map[string]float64 {
	return map[string]float64{
		"p05": pct(l.raw, 0.05), "p50": pct(l.raw, 0.50), "p95": pct(l.raw, 0.95),
		"p99": pct(l.raw, 0.99), "max": pct(l.raw, 1.0),
	}
}

// ---- RTP sequence / loss (wrap-aware) ----

type seqTracker struct {
	baseSet bool
	base    uint16
	highest uint16
	count   int64 // distinct packets seen
	gaps    int64 // missing sequence numbers
	dupes   int64
	seen    map[uint16]bool
}

func (s *seqTracker) push(seq uint16) {
	if !s.baseSet {
		s.baseSet, s.base, s.highest = true, seq, seq
		if s.seen == nil {
			s.seen = map[uint16]bool{}
		}
	}
	if s.seen[seq] {
		s.dupes++
		return
	}
	s.seen[seq] = true
	s.count++
	// Extend the highest with wrap-around: a jump > 32768 backwards is a wrap.
	d := int16(seq - s.highest)
	if d > 0 {
		s.gaps += int64(d) - 1
		s.highest = seq
	}
}

func (s *seqTracker) lossPct() float64 {
	if total := s.count + s.gaps; total > 0 {
		return float64(s.gaps) * 100 / float64(total)
	}
	return 0
}

// ---- H.264 keyframe detection from RTP payloads ----

// nalTypes returns the NAL types present in one RTP payload (single, STAP-A, FU-A start).
func nalTypes(p []byte) []byte {
	var out []byte
	if len(p) == 0 {
		return out
	}
	switch p[0] & 0x1f {
	case 28: // FU-A: the NAL type is in the FU header's low 5 bits
		if len(p) > 1 {
			out = append(out, p[1]&0x1f)
		}
	case 24: // STAP-A: NALs are length-prefixed inside, starting right after the indicator
		for i := 1; i+2 <= len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			i += 2
			if i+n > len(p) || n == 0 {
				break
			}
			out = append(out, p[i]&0x1f)
			i += n
		}
	default:
		out = append(out, p[0]&0x1f)
	}
	return out
}

// hasIDR reports whether this payload carries the start of an H.264 IDR picture
// (single NAL, STAP-A member, or FU-A start fragment) — what a receiver that just
// requested a keyframe is waiting for.
func hasIDR(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	switch p[0] & 0x1f {
	case 5:
		return true
	case 28: // FU-A: only a start fragment begins a decodable keyframe
		return len(p) > 1 && p[1]&0x80 != 0 && p[1]&0x1f == 5
	default:
		for _, t := range nalTypes(p) {
			if t == 5 {
				return true
			}
		}
		return false
	}
}

// ---- per-second series and events ----

type secStat struct {
	T          int     `json:"t"`
	Frames     int     `json:"frames"`
	Bytes      int     `json:"bytes"`
	Br         int64   `json:"br,omitempty"`   // encoder bitrate the server reports
	Drops      int64   `json:"drops,omitempty"` // backlog drops the server reports
	Est        int64   `json:"est,omitempty"`   // the server's congestion estimate
	Act        int64   `json:"act,omitempty"`   // the server's measured send rate
	BL         int     `json:"bl,omitempty"`    // paced backlog, ms
	LatP95     float64 `json:"lat_p95_ms"`     // floor-relative in rtc mode
	InputRTT   float64 `json:"input_rtt_ms,omitempty"`
	ProxyDrops int64   `json:"proxy_drops,omitempty"` // datagrams the impaired downlink dropped this second
}

type report struct {
	Mode        string             `json:"mode"`
	DurationS   float64            `json:"duration_s"`
	FirstFrame  float64            `json:"first_frame_ms"`
	AvgFPS      float64            `json:"avg_fps"`
	FPS5       float64            `json:"fps_p5"`     // worst seconds: lag spikes
	Floor      float64            `json:"lat_floor_ms"`
	Latency    map[string]float64 `json:"latency_ms_floorrel"`
	GapP95     float64            `json:"frame_gap_p95_ms"`
	MBps       float64            `json:"mbps"`
	LossPct    float64            `json:"loss_pct"`
	NACKsent   int64              `json:"nacks_sent,omitempty"`
	InputRTT   map[string]float64 `json:"input_rtt_ms,omitempty"`
	KFRecover  []float64          `json:"kf_recovery_ms,omitempty"`
	Blackout   *blackoutReport    `json:"blackout,omitempty"`
	Series     []secStat          `json:"series"`
}

type blackoutReport struct {
	AtMs          float64 `json:"at_ms"`
	DurMs         float64 `json:"dur_ms"`
	FirstFrameMs  float64 `json:"first_frame_ms"`  // blackout end → first frame
	FPS80Ms       float64 `json:"fps80_ms"`        // → fps back to 80% of pre-level
	BrBefore      int64   `json:"br_before"`
	BrNadir       int64   `json:"br_nadir"`
	BrRecoverMs   float64 `json:"br_recover_ms"`   // → bitrate back to 80% of pre-level
}

// second returns second t's record, creating (and gap-filling) as needed. A record
// for a second older than the newest is only found if it exists; otherwise the
// caller's writes go to a scratch record (a late event for a second we never made).
func (r *report) second(t int) *secStat {
	if len(r.Series) == 0 {
		r.Series = append(r.Series, secStat{T: t})
		return &r.Series[0]
	}
	last := len(r.Series) - 1
	if r.Series[last].T == t {
		return &r.Series[last]
	}
	if t < r.Series[last].T {
		for i := last; i >= 0; i-- {
			if r.Series[i].T == t {
				return &r.Series[i]
			}
		}
		dead := secStat{} // nowhere to land: drop it
		return &dead
	}
	for i := r.Series[last].T + 1; i < t; i++ {
		r.Series = append(r.Series, secStat{T: i})
	}
	r.Series = append(r.Series, secStat{T: t})
	return &r.Series[len(r.Series)-1]
}

// seriesAt returns second t's record by index (zeros if beyond the end).
func (r *report) seriesAt(t int) secStat {
	if t < 0 || t >= len(r.Series) {
		return secStat{}
	}
	return r.Series[t]
}

func (r report) String() string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

func fmtMS(v float64) string { return fmt.Sprintf("%.1f", v) }
