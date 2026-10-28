//go:build !darwin

package main

// Virtual displays are macOS only; elsewhere there is nothing to place.
func rememberPlace(id uint32) {}
func restorePlace(id uint32)  {}
