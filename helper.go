package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// One mydevices-capture process is kept warm and shared across viewer connections, so
// reconnecting (switching apps on the phone, a network blip) resumes instantly
// instead of starting a new capture from scratch.

const (
	captureLinger = 10 * time.Second // keep capturing this long after a viewer leaves
	helperLinger  = time.Minute      // keep the idle (near-zero CPU) process around this long
)

type captureHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	inMu  sync.Mutex
	dead  chan struct{}
	ended atomic.Bool // kill was called: on its way out, don't hand it to a new viewer

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
	if h := helperState.h; h != nil && !h.ended.Load() {
		select {
		case <-h.dead:
		default:
			return h, nil
		}
	}
	cmd := exec.Command(helperPath())
	// The helper refuses to capture unless it was started by us (see capture/main.swift).
	cmd.Env = append(os.Environ(), "MYDEVICES_PARENT="+strconv.Itoa(os.Getpid()))
	ownGroup(cmd)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	dlog("capture helper started (pid %d)", cmd.Process.Pid)
	h := &captureHelper{cmd: cmd, stdin: stdin, dead: make(chan struct{})}
	go h.read(stdout)
	go func() { err := cmd.Wait(); dlog("capture helper exited (%v)", err); close(h.dead) }()
	h.mu.Lock()
	h.scheduleIdle()
	h.mu.Unlock()
	helperState.h = h
	return h, nil
}

// helperDiag asks the capture helper whether macOS lets it record the screen and control
// input (for `mydevices doctor`). With ask, macOS also lists it in both Privacy panes and
// may show its prompt. It never borrows the helper from a live stream: busy while viewed.
func helperDiag(ask bool) (res map[string]any, busy bool, err error) {
	h, err := warmHelper()
	if err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	viewed := h.sink != nil
	h.mu.Unlock()
	if viewed {
		return nil, true, nil
	}
	got := make(chan map[string]any, 1)
	detach := h.attach(func(typ byte, buf []byte) {
		var m map[string]any
		if typ == 3 && json.Unmarshal(buf[1:], &m) == nil && m["t"] == "diag" {
			select {
			case got <- m:
			default:
			}
		}
	})
	defer detach()
	h.send(map[string]any{"t": "diag", "ask": ask})
	select {
	case m := <-got:
		return m, false, nil
	case <-time.After(5 * time.Second):
		return nil, false, errors.New("the capture helper didn't answer")
	}
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
			go h.send(map[string]any{"t": "release"}) // don't leave a button held down
		}
	}
}

// scheduleIdle stops capturing a little after the viewer leaves, and ends the
// process later still. Called with h.mu held.
func (h *captureHelper) scheduleIdle() {
	// The check and the action happen under one lock, so a viewer attaching at that
	// very moment can't have its fresh capture stopped or its helper killed.
	whenIdle := func(f func()) func() {
		return func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.sink == nil {
				f()
			}
		}
	}
	h.stopT = time.AfterFunc(captureLinger, whenIdle(func() { h.send(map[string]any{"t": "stop"}) }))
	h.killT = time.AfterFunc(helperLinger, whenIdle(h.kill))
}

// kill ends the helper. Closing its stdin lets it end its capture session properly and
// exit, which is what makes macOS take down its "recording" indicator; a helper killed
// outright can leave that indicator stuck. Force-killed only if it hasn't gone in 2 s.
func (h *captureHelper) kill() {
	if h.ended.Swap(true) {
		return
	}
	h.stdin.Close()
	go func() {
		select {
		case <-h.dead:
		case <-time.After(2 * time.Second):
			h.cmd.Process.Kill()
		}
	}()
}

// retireHelperOnExit: when mydevices is told to quit (logout, restart, Ctrl-C), it lets
// the helper end its capture session cleanly first. Otherwise launchd kills it outright
// along with us, which can leave macOS's recording indicator stuck.
func retireHelperOnExit() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ch
		helperState.Lock()
		h := helperState.h
		helperState.Unlock()
		if h != nil {
			h.kill()
			select {
			case <-h.dead:
			case <-time.After(2 * time.Second):
			}
		}
		os.Exit(0)
	}()
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
