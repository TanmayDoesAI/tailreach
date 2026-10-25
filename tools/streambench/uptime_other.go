//go:build !darwin

package main

import "time"

// The bench needs the helper's host-clock timestamps, so it only really runs on the
// Mac it shares with the device; elsewhere numbers are wall-clock approximations.
func hostNowMs() float64 {
	return float64(time.Now().UnixNano()) / 1e6
}
