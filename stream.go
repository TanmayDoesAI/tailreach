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
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The fast path: H.264 from tailreach-capture (kept warm, see helper.go), relayed to the
// browser's WebCodecs decoder.
//
// browser ⇄ WebSocket ⇄ tailreach ⇄ stdin/stdout ⇄ tailreach-capture (ScreenCaptureKit + VideoToolbox)
//
// Binary messages to the browser are [type][payload] (1 = decoder config, 2 = video).
// Text messages are JSON events. The browser sends JSON commands (input, start, keyframe);
// once WebRTC is up, input arrives on its data channels instead (see rtc.go).

const (
	minBitrate    = 1_000_000 // WebSocket path, and the least a viewer may ask for as its maximum
	rtcMinBitrate = 500_000   // WebRTC path: the encoder's own floor
	rtcStartRate  = 3_000_000 // safe on any 5G; Wi-Fi climbs from here within seconds
	backlogHigh = 8 // queued frames before we call the link congested (~130 ms at 60 fps)
)

// Commands a browser may send through to the capture helper.
var allowedCmds = map[string]bool{
	"start": true, "kf": true, "abs": true, "rel": true, "btn": true, "click": true,
	"scroll": true, "key": true, "text": true, "media": true, "clip-set": true, "clip-get": true, "diag": true, "resend": true,
	"displays": true,
	"extend": true, "unextend": true, // handled in the server (vdisplay.go), never sent to the helper
}

// What a view-only guest may send: keeping the stream going, nothing that reaches the
// Mac's input, clipboard, files or displays. Everything else is dropped (deny by default).
var viewOnlyCmds = map[string]bool{
	"kf": true, "resend": true, "ping": true, "spong": true, "bye": true, "vstats": true, "dlog": true,
}

// mayForward is forward's first check: may this viewer send command t at all?
func mayForward(t string, viewOnly bool) bool {
	return !viewOnly || viewOnlyCmds[t]
}

func helperPath() string {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	return filepath.Join(filepath.Dir(exe), "tailreach-capture")
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
	if streamsBlocked() {
		http.Error(w, "stopped on the Mac", http.StatusLocked)
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

	// One viewer at a time. A different device doesn't silently kick the one that's
	// working (the two would fight, each reconnecting over the other): it is told who has
	// the Mac and asks again with ?take=1 if the person chooses to take over. The same
	// device reconnecting (a network blip, a second tab) just replaces its old stream.
	viewer := a.viewerName(r)
	guest, viewOnly := "", false
	if g := guestOf(r); g != nil {
		guest, viewOnly = g.Login, g.ViewOnly
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var kicked atomic.Bool               // another viewer took over: this stream may no longer touch the helper
	var detachNow atomic.Pointer[func()] // set once attached to the helper
	by, release := claimStream(viewer, guest != "", r.URL.Query().Get("take") == "1", func(by string) {
		kicked.Store(true)
		if d := detachNow.Load(); d != nil {
			(*d)()
		}
		go func() { ws.Close(closeTakenOver, by); cancel() }()
	})
	if by != "" {
		b, _ := json.Marshal(map[string]any{"t": "busy", "by": by, "extended": extendedOpen()})
		ws.Write(r.Context(), websocket.MessageText, b)
		ws.Close(closeBusy, by)
		return
	}
	defer release()
	defer trackSession("stream", viewer, guest, cancel, false)()
	extViewerJoined()
	defer extViewerLeft()
	vncViewerJoined()
	defer vncViewerLeft()
	go func() { <-ctx.Done(); ws.CloseNow() }()
	go a.watchAuth(ctx, r, cancel)

	log.Printf("▶ stream started for %s", viewer)
	a.audit("stream-start", viewer, "")
	if guest != "" { // the person at the Mac always knows a guest is looking
		mode := "can control this Mac"
		if viewOnly {
			mode = "view only"
		}
		a.audit("guest-stream-start", guest, mode)
		notify("Guest connected: "+guest, mode)
		defer a.audit("guest-stream-end", guest, "")
	}
	start := time.Now()
	defer func() {
		log.Printf("■ stream ended for %s after %s", viewer, time.Since(start).Round(time.Second))
		a.audit("stream-end", viewer, time.Since(start).Round(time.Second).String())
	}()

	if debugLog { // the viewer logs too, into this log (see "dlog" below)
		ws.Write(ctx, websocket.MessageText, []byte(`{"t":"debug"}`))
	}

	h, err := warmHelper()
	if err != nil {
		dlog("capture helper failed to start: %v", err)
		ws.Close(websocket.StatusInternalError, "capture helper failed to start")
		return
	}
	toHelper := func(v any) {
		if !kicked.Load() {
			h.send(v)
		}
	}

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
		holdWS     atomic.Bool // the viewer is connecting WebRTC: no video on the WebSocket meanwhile
		hevc       atomic.Bool // the viewer asked for HEVC (it checked it can decode it)
		sound      atomic.Bool // the viewer turned sound on (WebRTC only)
	)
	bitrate.Store(rtcStartRate)
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
		if typ == 4 { // sound: only ever over WebRTC, and only if the viewer asked
			if s := viaRTC(); s != nil {
				s.writeAudio(buf)
			}
			return
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
			json.Unmarshal(buf[1:], &ev)
			if ev.T == "clip" && viewOnly {
				return // the Mac's clipboard isn't for view-only guests
			}
			if ev.T == "cursor" {
				if s := viaRTC(); s != nil && s.sendMove(buf[1:]) {
					return // 60 times a second: newest wins, never queued behind anything
				}
			}
			if ev.T == "stop-request" {
				log.Printf("■ stopped from the Mac")
				a.audit("stopped-on-mac", viewer, "")
				blockStreams(stopHold) // the viewer must not simply reconnect
				h.kill()               // Stop pressed on the Mac's panel
				go closeExtended()     // and the extended display goes with it
				releaseVNC()           // and a Mac unlocked from the phone locks again
				ws.Close(closeStoppedOnMac, "stopped on the Mac")
				cancel()
				return
			}
			push(outMsg{typ: websocket.MessageText, data: buf[1:]})
		case 1: // decoder config: never dropped
			if holdWS.Load() {
				return // sent again (with a keyframe) if the viewer falls back to the WebSocket
			}
			push(outMsg{typ: websocket.MessageBinary, data: buf})
		case 2:
			if holdWS.Load() {
				return
			}
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
				dlog("websocket backlog: skipping to a keyframe at %d kbps", br/1000)
				bitrate.Store(br)
				toHelper(map[string]any{"t": "br", "bps": br})
				toHelper(map[string]any{"t": "kf"})
				return
			}
			push(outMsg{typ: websocket.MessageBinary, data: buf, key: key})
		}
	})
	detachOnce := sync.OnceFunc(detach)
	detachNow.Store(&detachOnce)
	defer detachOnce()
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

	// Four times a second on WebRTC: steer the encoder by what the path is doing (see
	// ratectl.go): cut fast on a queue or loss, return to the last good rate as soon as
	// a dip clears.
	var ctlState atomic.Value // map[string]any for the stats line
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		ctl := newRateCtl(rtcMinBitrate, maxBitrate.Load(), rtcStartRate)
		var cur *rtcSession
		var sent [4]int64 // sentTotal at each of the last four ticks
		var rrSeen int64  // the receiver report last taken into account
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s := viaRTC()
			if s == nil {
				continue
			}
			if s != cur { // a new connection (maybe a new network): fresh sent history
				cur = s
				for j := range sent {
					sent[j] = s.sentTotal.Load()
				}
			}
			total := s.sentTotal.Load()
			sentBps := (total - sent[i%4]) * 8 // over the last second
			sent[i%4] = total
			ctl.max = maxBitrate.Load()
			q, loss := s.queueMs(), s.newLoss(&rrSeen)
			br := ctl.step(rateSample{now: time.Now(), queueMs: q, loss: loss, sentBps: sentBps})
			s.rateBps.Store(br)
			ctlState.Store(map[string]any{"q": q, "loss": loss, "good": ctl.good, "cutAt": ctl.cutAt, "sentBps": sentBps})
			if d := br - bitrate.Load(); d > br/20 || -d > br/20 {
				dlog("rate %d → %d kbps (queue %d ms, loss %.0f%%, sent %d kbps, last good %d kbps)",
					bitrate.Load()/1000, br/1000, q, loss*100, sentBps/1000, ctl.good/1000)
				bitrate.Store(br)
				toHelper(map[string]any{"t": "br", "bps": br})
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
				"rec": recordingViewer(),
				"ctl": ctlState.Load(),
			})
			if recordingViewer() {
				var m map[string]any
				json.Unmarshal(stats, &m)
				writeViewerRec("mac", m)
			}
			select {
			case out <- outMsg{typ: websocket.MessageText, data: stats}:
			default:
			}
		}
	}()

	// One input command, from the WebSocket or a data channel. "start" and WebRTC
	// negotiation only come over the WebSocket, handled below.
	forward := func(c map[string]any, reply func([]byte)) {
		if kicked.Load() {
			return
		}
		t, _ := c["t"].(string)
		if !mayForward(t, viewOnly) {
			return // view-only guest: no input, clipboard, files or displays
		}
		switch {
		case t == "spong": // reply to the rate controller's path probe (rtc.go probeLoop)
			if s := rtc.Load(); s != nil {
				if at, ok := c["at"].(float64); ok {
					s.pong(at)
				}
			}
		case t == "bye": // the viewer is leaving (page hidden or closed): end now, not at a timeout
			dlog("viewer said bye")
			cancel()
		case t == "dlog" && debugLog: // the viewer's own debug lines (debug builds only)
			if s, ok := c["s"].(string); ok {
				log.Printf("· [viewer] %.300s", s)
			}
		case t == "ping":
			c["t"] = "pong"
			b, _ := json.Marshal(c)
			reply(b)
		case t == "vstats": // the phone's own receive stats, only while recording (vrec.go)
			writeViewerRec("phone", c)
		case t == "extend": // make a virtual second display sized to this browser (vdisplay.go)
			w, _ := c["w"].(float64)
			h, _ := c["h"].(float64)
			hz, _ := c["hz"].(float64)
			go func() {
				id, err := extendDisplay(uint32(max(w, 0)), uint32(max(h, 0)), hz)
				ev := map[string]any{"t": "extended", "id": id}
				if err != nil {
					log.Printf("extend: %v", err)
					ev = map[string]any{"t": "extended", "error": err.Error()}
				}
				b, _ := json.Marshal(ev)
				reply(b)
			}()
		case t == "unextend":
			go func() {
				ev := map[string]any{"t": "extended", "id": 0}
				if err := closeExtended(); err != nil {
					ev["note"] = err.Error()
				}
				b, _ := json.Marshal(ev)
				reply(b)
			}()
		case allowedCmds[t] && t != "start":
			delete(c, "ask") // diag's "ask" (permission prompts) is for `tailreach doctor` only
			limit := 4096
			if t == "clip-set" {
				limit = 256 << 10
			}
			if s, ok := c["s"].(string); ok && len(s) > limit {
				return
			}
			toHelper(c)
		}
	}
	onInput := func(msg []byte, reply func([]byte)) {
		var c map[string]any
		if ctx.Err() == nil && json.Unmarshal(msg, &c) == nil {
			forward(c, reply)
		}
	}
	replyWS := func(b []byte) {
		select {
		case out <- outMsg{typ: websocket.MessageText, data: b}:
		default:
		}
	}

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
		case t == "rtc-offer":
			dlog("webrtc offer from the viewer")
			sdp, _ := c["sdp"].(string)
			if old := rtc.Swap(nil); old != nil {
				old.close()
			}
			sess, answer, err := a.newRTC(sdp, hevc.Load(), sound.Load(),
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
					dlog("webrtc down: video goes back to the WebSocket while the stream lasts")
					toHelper(map[string]any{"t": "resend"})
					push(outMsg{typ: websocket.MessageText, data: jsonMsg(map[string]any{"t": "rtc-failed"})})
				}, onInput)
			if err != nil {
				log.Printf("webrtc: %v", err)
				push(outMsg{typ: websocket.MessageText, data: jsonMsg(map[string]any{"t": "rtc-failed"})})
				continue
			}
			rtc.Store(sess)
			push(outMsg{typ: websocket.MessageText, data: jsonMsg(map[string]any{"t": "rtc-answer", "sdp": answer})})
		case t == "rtc-off":
			dlog("viewer turned WebRTC off: video on the WebSocket")
			holdWS.Store(false)
			if old := rtc.Swap(nil); old != nil {
				old.close()
			}
			toHelper(map[string]any{"t": "resend"}) // WebSocket decoder needs config + keyframe again
		case t == "start":
			first, _ := c["rtcFirst"].(bool)
			holdWS.Store(first)
			delete(c, "rtcFirst")
			if c["codec"] != "hevc" {
				delete(c, "codec")
			}
			hevc.Store(c["codec"] == "hevc")
			if c["audio"] != true {
				delete(c, "audio")
			}
			sound.Store(c["audio"] == true)
			if mb, ok := c["maxbr"].(float64); ok && mb >= minBitrate {
				maxBitrate.Store(min(int64(mb), 50_000_000))
			}
			if id, hz, ok := extendedInfo(); ok {
				if d, _ := c["display"].(float64); uint32(d) == id {
					c["fps"] = hz // the display's own rate, whatever the viewer asked
					maxBitrate.Store(min(maxBitrate.Load(), extMaxBitrate(hz)))
				}
			}
			c["br"] = min(bitrate.Load(), maxBitrate.Load())
			c["viewer"] = viewer
			delete(c, "guest") // only ever set here, never by the viewer
			delete(c, "viewOnly")
			if guest != "" { // the Mac's panel names the guest
				c["guest"], c["viewOnly"] = guest, viewOnly
			}
			dlog("start: %vx%v at %v fps, display %v, max %d kbps, WebRTC first %v",
				c["w"], c["h"], c["fps"], c["display"], maxBitrate.Load()/1000, first)
			toHelper(c)
		default:
			forward(c, replyWS)
		}
	}
}

// viewerName is the tailnet device on the other end (as forwarded by the hub, if any).
// A guest is named by their login too, so logs, the settings window and "in use" say so.
func (a *app) viewerName(r *http.Request) string {
	if v := r.Header.Get("X-Tailreach-Viewer"); v != "" && a.auth2 == nil && a.fromHub(r) {
		return v
	}
	name := "unknown device"
	if wi, err := a.lc.WhoIs(r.Context(), r.RemoteAddr); err == nil && wi.Node != nil {
		name = wi.Node.ComputedName
	}
	if g := guestOf(r); g != nil {
		return "guest " + g.Login + " (" + name + ")"
	}
	return name
}

// notify shows a macOS notification on this machine.
func notify(title, body string) {
	if runtime.GOOS != "darwin" {
		return
	}
	q := func(s string) string { return strings.NewReplacer(`\`, ``, `"`, ``).Replace(s) }
	exec.Command("osascript", "-e", `display notification "`+q(body)+`" with title "tailreach" subtitle "`+q(title)+`"`).Start()
}

// Stop on the Mac's panel ends the session and refuses new ones for a while, so the
// viewer can't just reconnect. The viewer is told why and stops retrying.
const (
	closeStoppedOnMac websocket.StatusCode = 4001
	closeBusy         websocket.StatusCode = 4002 // another device has the Mac: asks before taking over
	closeTakenOver    websocket.StatusCode = 4003 // another device took over; the reason is its name
	stopHold                               = 2 * time.Minute
)

var streamBlock atomic.Int64 // unix nanos until which new streams are refused

func blockStreams(d time.Duration) { streamBlock.Store(time.Now().Add(d).UnixNano()) }
func streamsBlocked() bool         { return time.Now().UnixNano() < streamBlock.Load() }

// watchAuth re-checks the viewer's sign-in while a stream is open: a session that
// expires, is signed out or revoked (or whose device trust is revoked) ends the stream.
// Each check also counts as activity, so a long session in use doesn't idle out.
func (a *app) watchAuth(ctx context.Context, r *http.Request, cancel func()) {
	if a.auth2 == nil {
		return // agent behind a hub: the hub signs viewers in
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !a.stillSignedIn(r) {
			log.Printf("■ stream ended: sign-in no longer valid")
			cancel()
			return
		}
	}
}

// stillSignedIn re-checks a request's sign-in: the owner's password trust and session, or
// a guest's invite (removed, ended or changed means out) and their own session.
func (a *app) stillSignedIn(r *http.Request) bool {
	if g := guestOf(r); g != nil {
		inv, ok := a.invites.get(g.Login)
		return ok && inv.ViewOnly == g.ViewOnly && a.auth2.validFor(r, g.Login)
	}
	return a.passwordOK(r) && a.auth2.valid(r)
}

// The streams open right now, so a second device can be told about the first instead of
// kicking it.
var streamReg struct {
	sync.Mutex
	next int
	m    map[int]regStream
}

type regStream struct {
	viewer string
	guest  bool
	kick   func(by string)
}

// claimStream registers a stream unless a different device already has the Mac (and this
// isn't a takeover), in which case it returns that device's name. A guest can never take
// the Mac from the owner. Checking, kicking the old streams and registering happen under
// one lock, so two devices connecting at once can't both end up registered.
func claimStream(viewer string, guest, take bool, kick func(string)) (busyBy string, release func()) {
	streamReg.Lock()
	defer streamReg.Unlock()
	for _, s := range streamReg.m {
		if s.viewer != viewer && (!take || guest && !s.guest) {
			return s.viewer, nil
		}
	}
	for _, s := range streamReg.m {
		s.kick(viewer) // only flags, detaches and closes in the background: never takes this lock
	}
	if streamReg.m == nil {
		streamReg.m = map[int]regStream{}
	}
	streamReg.next++
	id := streamReg.next
	streamReg.m[id] = regStream{viewer, guest, kick}
	return "", func() {
		streamReg.Lock()
		delete(streamReg.m, id)
		streamReg.Unlock()
	}
}
