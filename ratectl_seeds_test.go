package main

import (
	"os"
	"testing"
	"time"
)

// Every scenario under many jitter seeds (SEEDS=1 go test -run Seeds).
func TestRateSeeds(t *testing.T) {
	if os.Getenv("SEEDS") == "" {
		t.Skip()
	}
	dips := func(t time.Duration) int64 {
		if t > 5*time.Second && (t%(8*time.Second)) < 2*time.Second {
			return 1_000_000
		}
		return 8_000_000
	}
	fails := map[string]int{}
	var p90s []int64
	for seed := int64(1); seed <= 30; seed++ {
		r := runSim(simLink{capacity: dips, baseRTT: flat(40), demand: flat(12_000_000), jitterMs: 40}, 60*time.Second, seed)
		for dip := 16 * time.Second; dip < 56*time.Second; dip += 8 * time.Second {
			end := dip + 2*time.Second
			if rates, _ := r.window(end+2*time.Second, end+3*time.Second); pct(rates, 0.5) < 4_000_000 {
				fails["5G slow recovery"]++
			}
			_, q := r.window(end, dip+8*time.Second)
			p90s = append(p90s, pct(q, 0.9))
			if pct(q, 0.9) > 100 {
				fails["5G queue between dips"]++
			}
		}
		w := runSim(simLink{capacity: flat(40_000_000), baseRTT: flat(8), demand: flat(12_000_000), jitterMs: 10}, 30*time.Second, seed)
		if rates, q := w.window(10*time.Second, 30*time.Second); pct(rates, 0.5) < 10_000_000 || pct(q, 0.95) > 50 {
			fails["wifi"]++
		}
		pc := runSim(simLink{capacity: flat(30_000_000), baseRTT: func(t time.Duration) int64 {
			if t > 15*time.Second {
				return 60
			}
			return 10
		}, demand: flat(8_000_000), jitterMs: 25}, 40*time.Second, seed)
		if rates, _ := pc.window(30*time.Second, 40*time.Second); pct(rates, 0.5) < 4_000_000 {
			fails["path change"]++
		}
	}
	t.Logf("between-dip queue p90: median %d, p95 %d, max %d ms", pct(p90s, 0.5), pct(p90s, 0.95), maxOf(p90s))
	t.Logf("failures over 30 seeds (5G counts are per dip, 5 dips each): %v", fails)
}
