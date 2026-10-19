package main

import (
	"errors"
	"net"
)

// The local settings window is macOS/Linux only for now.
func listenControl(string) (net.Listener, error) { return nil, errors.New("not supported on Windows") }
func sameUser(net.Conn) bool                     { return false }
