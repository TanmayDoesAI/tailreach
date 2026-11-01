//go:build !darwin

package server

// Virtual displays are macOS only; elsewhere there is nothing to place.
func rememberPlace(id uint32) {}
func restorePlace(id uint32)  {}
