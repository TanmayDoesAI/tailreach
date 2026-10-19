package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The fast path: H.264 from mydevices-capture (kept warm, see helper.go), relayed to the
// browser's WebCodecs decoder.
//
// browser ⇄ WebSocket ⇄ mydevices ⇄ stdin/stdout ⇄ mydevices-capture (ScreenCaptureKit + VideoToolbox)
//
// Binary messages to the browser are [type][payload] (1 = decoder config, 2 = video).
// Text messages are JSON events. The browser sends JSON commands (input, start, keyframe).

const (
	minBitrate  = 1_000_000
	backlogHigh = 8 // queued frames before we call the link congested (~130 ms at 60 fps)
)

// Commands a browser may send through to the capture helper.
var allowedCmds = map[string]bool{
	"start": true, "kf": true, "abs": true, "rel": true, "btn": true, "click": true,
	"scroll": true, "key": true, "text": true, "media": true, "clip-set": true, "clip-get": true, "diag": true,
}

func helperPath() string {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	return filepath.Join(filepath.Dir(exe), "mydevices-capture")
}

func (a *app) fastAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := os.Stat(helperPath())
	return err == nil
}

type outMsg struct {
	typ  websocket.MessageType
	data []byte
	key  bool
}

func (a *app) relayStream(w http.ResponseWriter, r *http.Request) {
	if !a.fastAvailable() {
		http.Error(w, "fast stream not available on this device", http.StatusNotImplemented)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  []string{"*." + a.domain},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)

	// One viewer at a time: a new session replaces the old one.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	viewer := a.viewerName(r)
	defer trackSession("stream", viewer, cancel, true)()
	go func() { <-ctx.Done(); ws.CloseNow() }()

	log.Printf("▶ stream started for %s", viewer)
	a.audit("stream-start", viewer, "")
	start := time.Now()
	defer func() {
		log.Printf("■ stream ended for %s after %s", viewer, time.Since(start).Round(time.Second))
		a.audit("stream-end", viewer, time.Since(start).Round(time.Second).String())
	}()

	h, err := warmHelper()
	if err != nil {
		ws.Close(websocket.StatusInternalError, "capture helper failed to start")
		return
	}
	toHelper := h.send

	out := make(chan outMsg, 256)
	push := func(m outMsg) {
		select {
		case out <- m:
		case <-ctx.Done():
		}
	}
	var (
		bitrate    atomic.Int64
		maxBitrate atomic.Int64
		congested  atomic.Bool
		drops      atomic.Int64
		sentBytes  atomic.Int64
	)
	bitrate.Store(8_000_000)
	maxBitrate.Store(20_000_000)

	// WebRTC, once the browser has negotiated it and ICE is connected, carries the video.
	var rtc atomic.Pointer[rtcSession]
	var lastConfig atomic.Pointer[[]byte]
	defer func() {
		if s := rtc.Load(); s != nil {
			s.close()
		}
	}()
	viaRTC := func() *rtcSession {
		if s := rtc.Load(); s != nil && s.connected.Load() {
			return s
		}
		return nil
	}

	// helper → queue, dropping video while the link is congested
	waitKey := false
	detach := h.attach(func(typ byte, buf []byte) {
		if typ == 1 {
			cfg := buf
			lastConfig.Store(&cfg)
		}
		if s := viaRTC(); s != nil && typ != 3 {
			if typ == 1 {
				s.setConfig(buf)
			} else {
				s.writeFrame(buf) // paced, retransmitted and congestion-controlled by WebRTC
			}
			return
		}
		switch typ {
		case 3: // JSON event
			var ev struct{ T string }
			if json.Unmarshal(buf[1:], &ev) == nil && ev.T == "stop-request" {
				log.Printf("■ stopped from the Mac")
				h.kill() // Stop pressed on the Mac's panel
				cancel()
				return
			}
			push(outMsg{typ: websocket.MessageText, data: buf[1:]})
		case 1: // decoder config: never dropped
			push(outMsg{typ: websocket.MessageBinary, data: buf})
		case 2:
			key := len(buf) > 1 && buf[1] == 1
			if waitKey && !key {
				drops.Add(1)
				return
			}
			waitKey = false
			if len(out) >= backlogHigh {
				// The network can't keep up: skip ahead to a fresh keyframe at a lower bitrate.
				drops.Add(1)
				waitKey = true
				congested.Store(true)
				br := max(minBitrate, bitrate.Load()*6/10)
				bitrate.Store(br)
				toHelper(map[string]any{"t": "br", "bps": br})
				toHelper(map[string]any{"t": "kf"})
				return
			}
			push(outMsg{typ: websocket.MessageBinary, data: buf, key: key})
		}
	})
	defer detach()
	go func() {
		select {
		case <-h.dead:
			cancel()
		case <-ctx.Done():
		}
	}()

	// queue → browser
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-out:
				wctx, c := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Write(wctx, m.typ, m.data)
				c()
				if err != nil {
					return
				}
				sentBytes.Add(int64(len(m.data)))
			}
		}
	}()

	// Every second: report stats; after a calm stretch, nudge the bitrate back up.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		calm := 0
		path := a.connPath(ctx, r.RemoteAddr)
		for i := 1; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if s := viaRTC(); s != nil {
				// Steer the encoder to what congestion control says the path carries (with headroom).
				if t := int64(s.targetBitrate()) * 85 / 100; t > 0 {
					t = min(max(t, minBitrate), maxBitrate.Load())
					if d := t - bitrate.Load(); d > t/10 || -d > t/10 {
						bitrate.Store(t)
						toHelper(map[string]any{"t": "br", "bps": t})
					}
				}
				sentBytes.Add(s.sentBytes.Swap(0))
			} else if congested.Swap(false) {
				calm = 0
			} else if calm++; calm >= 3 && bitrate.Load() < maxBitrate.Load() {
				br := min(maxBitrate.Load(), bitrate.Load()*115/100)
				bitrate.Store(br)
				toHelper(map[string]any{"t": "br", "bps": br})
			}
			if i%5 == 0 {
				path = a.connPath(ctx, r.RemoteAddr)
			}
			stats, _ := json.Marshal(map[string]any{
				"t": "stats", "br": bitrate.Load(), "kbps": sentBytes.Swap(0) * 8 / 1000, "drops": drops.Swap(0), "path": path,
				"rtc": viaRTC() != nil,
			})
			select {
			case out <- outMsg{typ: websocket.MessageText, data: stats}:
			default:
			}
		}
	}()

	// browser → helper
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) && ctx.Err() == nil {
				log.Printf("stream read: %v", err)
			}
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var c map[string]any
		if json.Unmarshal(data, &c) != nil {
			continue
		}
		t, _ := c["t"].(string)
		switch {
		case t == "ping":
			c["t"] = "pong"
			b, _ := json.Marshal(c)
			select {
			case out <- outMsg{typ: websocket.MessageText, data: b}:
			default:
			}
		case t == "rtc-offer":
			sdp, _ := c["sdp"].(string)
			if old := rtc.Swap(nil); old != nil {
				old.close()
			}
			sess, answer, err := a.newRTC(sdp, int(maxBitrate.Load()),
				func() { toHelper(map[string]any{"t": "kf"}) },
				func() { // connected: switch video over with a fresh keyframe (SPS/PPS in front)
					if cfg := lastConfig.Load(); cfg != nil {
						if s := rtc.Load(); s != nil {
							s.setConfig(*cfg)
						}
					}
					toHelper(map[string]any{"t": "kf"})
					log.Printf("▶ %s: video over WebRTC", viewer)
				},
				func() { // failed: back to the WebSocket
					toHelper(map[string]any{"t": "resend"})
					push(outMsg{typ: websocket.MessageText, data: jsonMsg(map[string]any{"t": "rtc-failed"})})
				})
			if err != nil {
				log.Printf("webrtc: %v", err)
				push(outMsg{typ: websocket.MessageText, data: jsonMsg(map[string]any{"t": "rtc-failed"})})
				continue
			}
			rtc.Store(sess)
			push(outMsg{typ: websocket.MessageText, data: jsonMsg(map[string]any{"t": "rtc-answer", "sdp": answer})})
		case t == "rtc-off":
			if old := rtc.Swap(nil); old != nil {
				old.close()
			}
			toHelper(map[string]any{"t": "resend"}) // WebSocket decoder needs config + keyframe again
		case t == "start":
			if mb, ok := c["maxbr"].(float64); ok && mb >= minBitrate {
				maxBitrate.Store(min(int64(mb), 50_000_000))
			}
			c["br"] = min(bitrate.Load(), maxBitrate.Load())
			c["viewer"] = viewer
			toHelper(c)
		case allowedCmds[t]:
			limit := 4096
			if t == "clip-set" {
				limit = 256 << 10
			}
			if s, ok := c["s"].(string); ok && len(s) > limit {
				continue
			}
			toHelper(c)
		}
	}
}

// viewerName is the tailnet device on the other end (as forwarded by the hub, if any).
func (a *app) viewerName(r *http.Request) string {
	if v := r.Header.Get("X-Mydevices-Viewer"); v != "" && a.auth2 == nil && a.fromHub(r) {
		return v
	}
	if wi, err := a.lc.WhoIs(r.Context(), r.RemoteAddr); err == nil && wi.Node != nil {
		return wi.Node.ComputedName
	}
	return "unknown device"
}

// notify shows a macOS notification on this machine.
func notify(title, body string) {
	if runtime.GOOS != "darwin" {
		return
	}
	q := func(s string) string { return strings.NewReplacer(`\`, ``, `"`, ``).Replace(s) }
	exec.Command("osascript", "-e", `display notification "`+q(body)+`" with title "mydevices" subtitle "`+q(title)+`"`).Start()
}
