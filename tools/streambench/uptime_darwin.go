//go:build darwin

package main

import (
	"golang.org/x/sys/unix"
)

// hostNowMs: the current time in ms on the mach host time clock — the same clock
// VideoToolbox presentation timestamps are on. Wall clock cannot be used instead:
// mach time stops while the Mac sleeps, so boot + uptime drifts from unix time by
// the total time asleep.
func hostNowMs() float64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &ts)
	return float64(ts.Sec)*1e3 + float64(ts.Nsec)/1e6
}
