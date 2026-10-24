package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Viewer stats recording: measures the stream as a real phone sees it. It can only
// be switched on from the Mac's control socket (op "recordViewer"); while it runs,
// the stats the Mac sends each second carry rec:true, the viewer answers with its
// own WebRTC receive stats ("vstats"), and both sides are appended, one JSON line
// per second each, to <stateDir>/viewer-stats.jsonl. Nothing here reaches the helper.

var viewerRec atomic.Int64 // unix ms the recording runs until; 0 = off

var vrecFile struct {
	sync.Mutex
	f *os.File
}

const vrecMaxBytes = 20 << 20

func recordingViewer() bool { return time.Now().UnixMilli() < viewerRec.Load() }

func viewerStatsPath(stateDir string) string {
	return filepath.Join(stateDir, "viewer-stats.jsonl")
}

// startViewerRec arms recording for secs seconds (0 stops it) and returns the log path.
func (a *app) startViewerRec(secs int) string {
	path := viewerStatsPath(a.opt.stateDir)
	vrecFile.Lock()
	defer vrecFile.Unlock()
	if vrecFile.f != nil {
		vrecFile.f.Close()
		vrecFile.f = nil
	}
	if secs <= 0 {
		viewerRec.Store(0)
		return path
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		viewerRec.Store(0)
		return ""
	}
	vrecFile.f = f
	viewerRec.Store(time.Now().Add(time.Duration(secs) * time.Second).UnixMilli())
	return path
}

// writeViewerRec appends one line tagged with its source ("mac" or "phone").
func writeViewerRec(src string, m map[string]any) {
	if !recordingViewer() {
		return
	}
	line := map[string]any{"ts": time.Now().UnixMilli(), "src": src}
	for k, v := range m {
		if k != "t" && k != "ts" && k != "src" {
			line[k] = v
		}
	}
	b, err := json.Marshal(line)
	if err != nil || len(b) > 8192 {
		return
	}
	vrecFile.Lock()
	defer vrecFile.Unlock()
	if vrecFile.f == nil {
		return
	}
	if st, err := vrecFile.f.Stat(); err == nil && st.Size() > vrecMaxBytes {
		return
	}
	vrecFile.f.Write(append(b, '\n'))
}
