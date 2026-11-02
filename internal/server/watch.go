package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Watching: while one device controls the Mac, other signed-in devices (the owner's own, or
// an invited guest's) can watch the same picture, e.g. to show someone what you're doing.
// A watcher is always view only, whatever it's otherwise allowed: nothing it sends reaches
// the Mac's input, clipboard or displays, it never changes what's captured, and it never
// gets the Mac's clipboard. Stop and Pause on the Mac end watchers too, and the panel on the
// Mac counts them.

const (
	maxWatchers         = 4
	closeNothingToWatch = 4004 // nobody is controlling the Mac (any more): connect normally
	watchKeyEvery       = 2 * time.Second
)

// Events a watcher gets: what keeps its picture right. Never "clip", "warn" or anything
// that's about controlling the Mac.
var watchEvents = map[string]bool{"started": true, "displays": true, "cursor": true, "lock": true}

var watching struct {
	sync.Mutex
	next  int
	names map[int]string
}

// controllerName is the device that has the Mac right now, or "".
func controllerName() string {
	streamReg.Lock()
	defer streamReg.Unlock()
	for _, s := range streamReg.m {
		return s.viewer
	}
	return ""
}

// joinWatchers adds a watcher, unless there are already maxWatchers.
func joinWatchers(viewer string) (leave func(), ok bool) {
	watching.Lock()
	defer watching.Unlock()
	if len(watching.names) >= maxWatchers {
		return nil, false
	}
	if watching.names == nil {
		watching.names = map[int]string{}
	}
	watching.next++
	id := watching.next
	watching.names[id] = viewer
	return func() {
		watching.Lock()
		delete(watching.names, id)
		watching.Unlock()
	}, true
}

func watcherCount() int {
	watching.Lock()
	defer watching.Unlock()
	return len(watching.names)
}

// tellPanel updates the Mac's "Screen shared" panel: "+2 watching".
func tellPanel(h *captureHelper) {
	h.send(map[string]any{"t": "watchers", "n": watcherCount()})
}

func (a *app) watchStream(ws *websocket.Conn, r *http.Request, viewer, guest string) {
	by := controllerName()
	if by == "" {
		ws.Close(closeNothingToWatch, "nobody is using the Mac")
		return
	}
	leave, ok := joinWatchers(viewer)
	if !ok {
		ws.Close(closeNothingToWatch, "too many devices are watching")
		return
	}
	defer leave()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer trackSession("watch", viewer, guest, cancel, false)() // Stop on the Mac, endGuest
	go func() {
		<-ctx.Done()
		if streamsBlocked() { // stopped or paused on the Mac: say so, so it doesn't retry
			ws.Close(closeStoppedOnMac, blockedReason())
		}
		ws.CloseNow()
	}()
	go a.watchAuth(ctx, r, cancel)

	h, err := warmHelper()
	if err != nil {
		ws.Close(websocket.StatusInternalError, "capture helper failed to start")
		return
	}
	log.Printf("▶ %s is watching (%s has the Mac)", viewer, by)
	a.audit("watch-start", viewer, "")
	if guest != "" { // the person at the Mac always knows a guest is looking
		a.audit("guest-watch-start", guest, "view only")
		notify("Guest watching: "+guest, "view only")
	}
	start := time.Now()
	defer func() {
		log.Printf("■ %s stopped watching after %s", viewer, time.Since(start).Round(time.Second))
		a.audit("watch-end", viewer, time.Since(start).Round(time.Second).String())
	}()
	defer func() { go tellPanel(h) }()

	out := make(chan outMsg, 128)
	push := func(m outMsg) bool {
		select {
		case out <- m:
			return true
		default:
			return false
		}
	}
	var keyMu sync.Mutex
	var lastKey time.Time
	askKey := func() { // keyframes are big and the controller gets them too: not too often
		keyMu.Lock()
		defer keyMu.Unlock()
		if time.Since(lastKey) >= watchKeyEvery {
			lastKey = time.Now()
			h.send(map[string]any{"t": "kf"})
		}
	}

	b, _ := json.Marshal(map[string]any{"t": "watching", "by": by})
	push(outMsg{typ: websocket.MessageText, data: b})

	waitKey := true
	stop := h.watch(func(typ byte, buf []byte) {
		switch typ {
		case 1: // decoder config
			push(outMsg{typ: websocket.MessageBinary, data: buf})
		case 2:
			key := len(buf) > 1 && buf[1] == 1
			if waitKey && !key {
				return
			}
			waitKey = false
			if len(out) >= backlogHigh || !push(outMsg{typ: websocket.MessageBinary, data: buf, key: key}) {
				waitKey = true // this watcher's link can't keep up: skip ahead to a keyframe
				go askKey()
			}
		case 3:
			var ev struct{ T string }
			json.Unmarshal(buf[1:], &ev)
			if ev.T == "started" {
				go tellPanel(h) // a restarted capture shows the panel afresh
			}
			if watchEvents[ev.T] {
				push(outMsg{typ: websocket.MessageText, data: buf[1:]})
			}
		}
	})
	defer stop()
	tellPanel(h)
	h.send(map[string]any{"t": "resend"}) // decoder config and a keyframe for this watcher

	// The controller left (not just a blip while it reconnects): tell the watcher.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		gone := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.dead:
				cancel()
				return
			case <-t.C:
			}
			if controllerName() == "" {
				gone++
			} else {
				gone = 0
			}
			if gone >= 5 {
				ws.Close(closeNothingToWatch, "the session you were watching ended")
				cancel()
				return
			}
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
			}
		}
	}()

	// browser → here: only what keeps the picture going. Nothing reaches the helper but a
	// rate-limited keyframe request.
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var c map[string]any
		if json.Unmarshal(data, &c) != nil {
			continue
		}
		switch c["t"] {
		case "kf", "resend":
			go askKey()
		case "ping":
			c["t"] = "pong"
			b, _ := json.Marshal(c)
			push(outMsg{typ: websocket.MessageText, data: b})
		}
	}
}
