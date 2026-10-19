package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// One mydevices-capture process is kept warm and shared across viewer connections, so
// reconnecting (switching apps on the phone, a network blip) resumes instantly
// instead of starting a new capture from scratch.

const (
	captureLinger = 15 * time.Second // keep capturing this long after a viewer leaves
	helperLinger  = 2 * time.Minute  // keep the idle process around this long
)

type captureHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	inMu  sync.Mutex
	dead  chan struct{}

	mu    sync.Mutex
	sink  func(typ byte, msg []byte) // the attached viewer, or nil
	gen   int
	stopT *time.Timer
	killT *time.Timer
}

var helperState struct {
	sync.Mutex
	h *captureHelper
}

// warmHelper returns the running helper, starting one if needed.
func warmHelper() (*captureHelper, error) {
	helperState.Lock()
	defer helperState.Unlock()
	if h := helperState.h; h != nil {
		select {
		case <-h.dead:
		default:
			return h, nil
		}
	}
	cmd := exec.Command(helperPath())
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &captureHelper{cmd: cmd, stdin: stdin, dead: make(chan struct{})}
	go h.read(stdout)
	go func() { cmd.Wait(); close(h.dead) }()
	h.mu.Lock()
	h.scheduleIdle()
	h.mu.Unlock()
	helperState.h = h
	return h, nil
}

func (h *captureHelper) send(v any) {
	b, _ := json.Marshal(v)
	h.inMu.Lock()
	h.stdin.Write(append(b, '\n'))
	h.inMu.Unlock()
}

// attach routes everything the helper says to sink until detach is called.
// A newer attach replaces an older one.
func (h *captureHelper) attach(sink func(byte, []byte)) (detach func()) {
	h.mu.Lock()
	h.gen++
	gen := h.gen
	h.sink = sink
	if h.stopT != nil {
		h.stopT.Stop()
	}
	if h.killT != nil {
		h.killT.Stop()
	}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.gen == gen {
			h.sink = nil
			h.scheduleIdle()
		}
	}
}

// scheduleIdle stops capturing a little after the viewer leaves, and ends the
// process later still. Called with h.mu held.
func (h *captureHelper) scheduleIdle() {
	idle := func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.sink == nil }
	h.stopT = time.AfterFunc(captureLinger, func() {
		if idle() {
			h.send(map[string]any{"t": "stop"})
		}
	})
	h.killT = time.AfterFunc(helperLinger, func() {
		if idle() {
			h.kill()
		}
	})
}

func (h *captureHelper) kill() {
	h.stdin.Close()
	h.cmd.Process.Kill()
}

// read parses [u32 len][u8 type][payload] frames and hands each one to the sink.
func (h *captureHelper) read(stdout io.Reader) {
	rd := bufio.NewReaderSize(stdout, 1<<20)
	hdr := make([]byte, 5)
	for {
		if _, err := io.ReadFull(rd, hdr); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		if n > 64<<20 {
			h.kill()
			return
		}
		buf := make([]byte, n+1)
		buf[0] = hdr[4]
		if _, err := io.ReadFull(rd, buf[1:]); err != nil {
			return
		}
		h.mu.Lock()
		sink := h.sink
		h.mu.Unlock()
		if sink != nil {
			sink(hdr[4], buf)
		}
	}
}

// connPath says how this request reaches us: "direct", or through a relay.
func (a *app) connPath(ctx context.Context, remoteAddr string) string {
	who, err := a.lc.WhoIs(ctx, remoteAddr)
	if err != nil || who.Node == nil {
		return ""
	}
	st, err := a.lc.Status(ctx)
	if err != nil {
		return ""
	}
	ps := st.Peer[who.Node.Key]
	switch {
	case ps == nil:
		return ""
	case ps.CurAddr != "":
		return "direct"
	case ps.PeerRelay != "":
		return "peer relay"
	case ps.Relay != "":
		return "relay " + ps.Relay
	}
	return ""
}
