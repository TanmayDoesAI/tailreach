package ratectl

import (
	"testing"
	"time"
)

// rateAt is the encoder bitrate over time and whether the link has shown a limit.
type rateAt func(t time.Duration) (int64, bool)

func steady(r int64, limited bool) rateAt {
	return func(time.Duration) (int64, bool) { return r, limited }
}

// phases: rate r (limited) from each start time on.
func phases(p ...struct {
	from time.Duration
	r    int64
}) rateAt {
	return func(t time.Duration) (int64, bool) {
		r := p[0].r
		for _, x := range p {
			if t >= x.from {
				r = x.r
			}
		}
		return r, true
	}
}

type ph = struct {
	from time.Duration
	r    int64
}

type scaleChange struct {
	at time.Duration
	f  float64
}

func runScaler(hevc bool, rate rateAt, d time.Duration) []scaleChange {
	s := NewScaler()
	s.HEVC = hevc
	start := time.Unix(1_000_000, 0)
	var out []scaleChange
	f := s.Factor()
	for t := time.Duration(0); t < d; t += tick {
		r, lim := rate(t)
		if g := s.Step(start.Add(t), r, lim); g != f {
			f = g
			out = append(out, scaleChange{t, g})
		}
	}
	return out
}

func TestScaler(t *testing.T) {
	sec := time.Second
	cases := []struct {
		name string
		hevc bool
		rate rateAt
		d    time.Duration
		want []scaleChange // each change, at (roughly) when
	}{
		{"fast link stays full", false, steady(15_000_000, true), 30 * sec, nil},
		{"still screen at the start rate, no limit seen", false, steady(3_000_000, false), 30 * sec, nil},
		{"5G at 3 Mbit/s: 75%", false, steady(3_000_000, true), 30 * sec,
			[]scaleChange{{2 * sec, 0.75}}},
		{"deep dip skips straight to 50%", false, phases(ph{0, 10_000_000}, ph{5 * sec, 800_000}), 15 * sec,
			[]scaleChange{{7 * sec, 0.5}}},
		{"a dip shorter than 2 s changes nothing", false,
			phases(ph{0, 10_000_000}, ph{5 * sec, 800_000}, ph{6500 * time.Millisecond, 10_000_000}), 30 * sec, nil},
		{"dip then recovery: back up one level at a time", false,
			phases(ph{0, 10_000_000}, ph{5 * sec, 800_000}, ph{10 * sec, 12_000_000}), 40 * sec,
			[]scaleChange{{7 * sec, 0.5}, {16 * sec, 0.75}, {22 * sec, 1}}},
		{"partial recovery stops at 75%", false,
			phases(ph{0, 800_000}, ph{10 * sec, 3_000_000}), 40 * sec,
			[]scaleChange{{2 * sec, 0.5}, {16 * sec, 0.75}}},
		{"rate between the thresholds holds the level", false,
			phases(ph{0, 3_000_000}, ph{10 * sec, 5_000_000}), 40 * sec,
			[]scaleChange{{2 * sec, 0.75}}},
		{"HEVC keeps full res at 3 Mbit/s", true, steady(3_000_000, true), 30 * sec, nil},
		{"HEVC at 2 Mbit/s: 75%", true, steady(2_000_000, true), 30 * sec,
			[]scaleChange{{2 * sec, 0.75}}},
		{"a dip right after a step up waits out the dwell", false,
			phases(ph{0, 3_000_000}, ph{3 * sec, 8_000_000}, ph{9500 * time.Millisecond, 3_000_000}), 30 * sec,
			[]scaleChange{{2 * sec, 0.75}, {9 * sec, 1}, {13 * sec, 0.75}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runScaler(c.hevc, c.rate, c.d)
			if len(got) != len(c.want) {
				t.Fatalf("changes %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i].f != c.want[i].f || got[i].at < c.want[i].at-tick || got[i].at > c.want[i].at+tick {
					t.Fatalf("changes %v, want %v", got, c.want)
				}
			}
		})
	}
}

// However the rate swings, the picture changes size at most once per dwell.
func TestScalerNoFlap(t *testing.T) {
	swings := map[string]rateAt{
		"square 1 s": func(t time.Duration) (int64, bool) {
			if t/time.Second%2 == 0 {
				return 1_000_000, true
			}
			return 8_000_000, true
		},
		"square 3 s": func(t time.Duration) (int64, bool) {
			if t/(3*time.Second)%2 == 0 {
				return 1_000_000, true
			}
			return 8_000_000, true
		},
		"square 7 s": func(t time.Duration) (int64, bool) {
			if t/(7*time.Second)%2 == 0 {
				return 1_000_000, true
			}
			return 8_000_000, true
		},
		"around a threshold": func(t time.Duration) (int64, bool) {
			return 4_000_000 + int64(t/tick%3-1)*200_000, true
		},
	}
	for name, r := range swings {
		got := runScaler(false, r, 120*time.Second)
		for i := 1; i < len(got); i++ {
			if d := got[i].at - got[i-1].at; d < scaleDwell {
				t.Errorf("%s: changes %v and %v only %v apart", name, got[i-1], got[i], d)
			}
		}
		if len(got) > 120/4 {
			t.Errorf("%s: %d changes in 2 minutes", name, len(got))
		}
	}
}
