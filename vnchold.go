package main

import (
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// macOS locks the Mac again a few seconds after a Screen Sharing connection that unlocked
// it ends (its own safety rule). So the viewer's unlock connection (websockify?hold=1) is
// not hung up: the relay keeps it open, idle, while anyone is viewing, and closes it a
// minute after the last viewer leaves, when macOS locks the Mac again. Nothing is sent on
// it: no picture is asked for and no input goes in.

const vncHoldGrace = time.Minute // rides out a reconnect (Wi-Fi ↔ 5G) without relocking

var vncHeld struct {
	mu      sync.Mutex
	conn    net.Conn
	viewers int
	t       *time.Timer
}

// holdVNC keeps an unlock connection open. It replaces one held before.
func holdVNC(c net.Conn) {
	vncHeld.mu.Lock()
	defer vncHeld.mu.Unlock()
	if vncHeld.conn != nil {
		vncHeld.conn.Close()
	}
	vncHeld.conn = c
	log.Printf("Screen Sharing kept open while you view, so the Mac stays unlocked")
	go func() { // the answer to a last picture request, if any; ends when it's closed
		io.Copy(io.Discard, c)
		vncHeld.mu.Lock()
		if vncHeld.conn == c {
			vncHeld.conn = nil
		}
		vncHeld.mu.Unlock()
	}()
	if vncHeld.viewers == 0 {
		armVNCReleaseLocked()
	}
}

// releaseVNC ends the held connection now; macOS then locks the Mac.
func releaseVNC() {
	vncHeld.mu.Lock()
	defer vncHeld.mu.Unlock()
	releaseVNCLocked()
}

func releaseVNCLocked() {
	if vncHeld.t != nil {
		vncHeld.t.Stop()
		vncHeld.t = nil
	}
	if vncHeld.conn != nil {
		vncHeld.conn.Close()
		vncHeld.conn = nil
		log.Printf("Screen Sharing closed: macOS locks the Mac again")
	}
}

func armVNCReleaseLocked() {
	if vncHeld.conn == nil {
		return
	}
	if vncHeld.t != nil {
		vncHeld.t.Stop()
	}
	vncHeld.t = time.AfterFunc(vncHoldGrace, func() {
		vncHeld.mu.Lock()
		defer vncHeld.mu.Unlock()
		if vncHeld.viewers == 0 {
			releaseVNCLocked()
		}
	})
}

func vncViewerJoined() {
	vncHeld.mu.Lock()
	defer vncHeld.mu.Unlock()
	vncHeld.viewers++
	if vncHeld.t != nil {
		vncHeld.t.Stop()
		vncHeld.t = nil
	}
}

func vncViewerLeft() {
	vncHeld.mu.Lock()
	defer vncHeld.mu.Unlock()
	if vncHeld.viewers--; vncHeld.viewers <= 0 {
		vncHeld.viewers = 0
		armVNCReleaseLocked()
	}
}
