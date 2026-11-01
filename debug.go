package main

import "log"

// Debug logging is compiled in only by `make DEBUG=1` (the "debug" build tag). In a normal
// build debugLog is the constant false, so the compiler drops every dlog call and every
// `if debugLog` block: they cost nothing, not even formatting. The lines go to the same
// log as everything else (~/Library/Application Support/tailreach/tailreach.log), marked "·".
func dlog(format string, args ...any) {
	if debugLog {
		log.Printf("· "+format, args...)
	}
}
