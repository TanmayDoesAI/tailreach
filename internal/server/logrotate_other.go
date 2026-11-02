//go:build !darwin

package server

// Elsewhere tailreach's output goes wherever its init system sends it.
func rotateOutput(string) {}
