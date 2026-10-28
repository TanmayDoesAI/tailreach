package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	vd "github.com/go-macos/virtualdisplay"
)

// Extended display: a virtual monitor the Mac treats like a real second screen, sized to
// the viewing browser. It is only ever an EXTRA screen: the Mac's own display stays the
// main one (nothing here moves the menu bar, the Dock or the display arrangement). The
// capture helper already lists every display and streams the one the viewer picks, so all
// this adds is creating and removing the display.
//
// The display lives in a small CHILD PROCESS (`mydevices vdisplay`, below), because the
// API behind it is private: if a macOS update changes it, the crash is in the child, not
// in the login item that all remote access goes through. The child runs in the GUI session
// (it is started by the login item, which creating a display needs), and ends when its
// stdin closes, so it can never outlive this process. A process exit also always removes
// the display, whatever state it is in.
// Not in the capture helper: that one exits a minute after the last viewer leaves, which
// would drag the windows back to the built-in screen.

const (
	extName = "Extended display" // stable: macOS remembers arrangement per name and size
	extIdle = 5 * time.Minute    // close the display this long after the last viewer leaves
)

var ext struct {
	mu       sync.Mutex
	cmd      *exec.Cmd // the child that owns the display; nil when there is none
	stdin    io.WriteCloser
	done     chan struct{} // closed when the child has exited
	id       uint32
	w, h     uint32
	hz       float64 // the rate it's streamed at (the display itself runs at 60 Hz)
	viewers  int // streams open right now
	idleT    *time.Timer
	openedAt time.Time
}

var extFlight atomic.Bool // an extend is being handled: others are turned away, not queued

// A display nobody is looking at still costs the Mac window-server work, so it closes by
// itself a few minutes after the last stream ends (long enough to ride out reconnects).
func extViewerJoined() {
	ext.mu.Lock()
	defer ext.mu.Unlock()
	ext.viewers++
	if ext.idleT != nil {
		ext.idleT.Stop()
		ext.idleT = nil
	}
}

func extViewerLeft() {
	ext.mu.Lock()
	defer ext.mu.Unlock()
	ext.viewers--
	if ext.viewers <= 0 {
		ext.viewers = 0
		armIdleLocked()
	}
}

// armIdleLocked closes the display after extIdle with no viewer.
func armIdleLocked() {
	if ext.cmd == nil {
		return
	}
	if ext.idleT != nil {
		ext.idleT.Stop()
	}
	ext.idleT = time.AfterFunc(extIdle, func() {
		ext.mu.Lock()
		defer ext.mu.Unlock()
		if ext.viewers == 0 && ext.cmd != nil {
			log.Printf("extended display: no viewer for %s, closing it", extIdle)
			closeExtendedLocked()
		}
	})
}

func clampU(v, lo, hi uint32) uint32 { return min(max(v, lo), hi) }

// extendDisplay creates the virtual display (or reuses the one already open with these
// settings) and returns its display ID.
func extendDisplay(w, h uint32, hz float64) (uint32, error) {
	if !extFlight.CompareAndSwap(false, true) {
		return 0, fmt.Errorf("busy: try again in a moment")
	}
	defer extFlight.Store(false)
	// Few distinct modes: every new size leaves a colour profile file behind in the system
	// folder (the package docs say so), and the refresh rate is either 30 or 60.
	w, h = clampU(w, 640, 3840)&^1, clampU(h, 480, 2160)&^1
	if hz < 45 {
		hz = 30
	} else {
		hz = 60
	}
	if err := vdDisabled(); err != nil {
		return 0, err
	}

	ext.mu.Lock()
	defer ext.mu.Unlock()
	// The display itself always runs at 60 Hz; hz is the frame rate it is streamed at. So a
	// new rate only restarts the capture (the viewer does that), and the display, with the
	// windows on it, stays where it is.
	if ext.cmd != nil && ext.w == w && ext.h == h {
		if ext.hz != hz {
			log.Printf("extended display %d: streaming at %.0f fps", ext.id, hz)
		}
		ext.hz = hz
		return ext.id, nil
	}
	if ext.cmd != nil && time.Since(ext.openedAt) < 2*time.Second {
		return 0, fmt.Errorf("changed too fast: try again in a moment")
	}
	closeExtendedLocked()
	id, err := startChildLocked(w, h, hz)
	if err != nil {
		return 0, err
	}
	if ext.viewers == 0 {
		armIdleLocked() // opened for a stream that has already ended
	}
	return id, nil
}

func startChildLocked(w, h uint32, hz float64) (uint32, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "vdisplay", "-w", strconv.Itoa(int(w)), "-h", strconv.Itoa(int(h)), "-hz", "60")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()

	type reply struct {
		ID    uint32 `json:"id"`
		Error string `json:"error"`
	}
	first := make(chan reply, 1)
	go func() {
		var r reply
		if line, err := bufio.NewReader(stdout).ReadBytes('\n'); err == nil {
			json.Unmarshal(line, &r)
		}
		first <- r
	}()
	var r reply
	select {
	case r = <-first:
	case <-time.After(15 * time.Second):
		stdin.Close()
		cmd.Process.Kill()
		<-done
		return 0, fmt.Errorf("the Mac didn't create the display in time")
	}
	if r.ID == 0 {
		stdin.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		if r.Error != "" {
			return 0, fmt.Errorf("%s", r.Error)
		}
		// The child ended without a word: it crashed inside the macOS display API. Keep the
		// feature off (the rest of mydevices is unaffected) until the file is deleted.
		os.MkdirAll(defaultStateDir(), 0o700)
		os.WriteFile(vdFile("vdisplay.disabled"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
		log.Printf("extended display: the display process crashed; feature turned off")
		return 0, vdDisabled()
	}

	ext.cmd, ext.stdin, ext.done = cmd, stdin, done
	ext.id, ext.w, ext.h, ext.hz, ext.openedAt = r.ID, w, h, hz, time.Now()
	log.Printf("extended display %d opened: %dx%d, streamed at %.0f fps", r.ID, w, h, hz)
	go func() { // the display vanishing on its own (the child died): forget it
		<-done
		ext.mu.Lock()
		defer ext.mu.Unlock()
		if ext.cmd == cmd {
			log.Printf("extended display %d ended on its own", r.ID)
			ext.cmd, ext.stdin, ext.done, ext.id = nil, nil, nil, 0
		}
	}()
	return r.ID, nil
}

func closeExtended() error {
	ext.mu.Lock()
	defer ext.mu.Unlock()
	return closeExtendedLocked()
}

// closeExtendedLocked ends the child, whose exit removes the display. If it doesn't leave
// in a few seconds it is killed (that removes the display as well).
func closeExtendedLocked() error {
	if ext.cmd == nil {
		return nil
	}
	cmd, stdin, done, id := ext.cmd, ext.stdin, ext.done, ext.id
	ext.cmd, ext.stdin, ext.done, ext.id = nil, nil, nil, 0
	if ext.idleT != nil {
		ext.idleT.Stop()
		ext.idleT = nil
	}
	stdin.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		<-done
	}
	if err := vd.WaitGone(3*time.Second, id); err != nil {
		log.Printf("extended display %d is still listed after its process ended (%v)", id, err)
		return fmt.Errorf("macOS still lists the extended display")
	}
	log.Printf("extended display closed")
	return nil
}

// extendedInfo is the open extended display's ID and the frame rate it's streamed at.
func extendedInfo() (id uint32, hz float64, ok bool) {
	ext.mu.Lock()
	defer ext.mu.Unlock()
	if ext.cmd == nil {
		return 0, 0, false
	}
	return ext.id, ext.hz, true
}

// extendedOpen reports whether the extended display exists right now.
func extendedOpen() bool {
	_, _, ok := extendedInfo()
	return ok
}

// extMaxBitrate: what desktop content needs. The relay's per-packet work grows with the
// bitrate, so the extended display doesn't get the 20-40 Mbit/s ceiling of "Crisp".
func extMaxBitrate(hz float64) int64 {
	if hz <= 30 {
		return 10_000_000
	}
	return 16_000_000
}

// ---------- the child: `mydevices vdisplay -w W -h H -hz HZ` ----------

func vdFile(name string) string { return filepath.Join(defaultStateDir(), name) }

// vdDisabled says why the feature is off after a crash of the display process.
func vdDisabled() error {
	if _, err := os.Stat(vdFile("vdisplay.disabled")); err == nil {
		return fmt.Errorf("the extended display was turned off after a crash in the macOS display API; delete %s to try again", vdFile("vdisplay.disabled"))
	}
	return nil
}

// vdisplayChild owns one virtual display until its stdin closes (the parent asked, or died).
// It prints one JSON line: {"id":N} once the display is up, or {"error":"..."}.
func vdisplayChild(args []string) int {
	fs := flag.NewFlagSet("vdisplay", flag.ContinueOnError)
	w := fs.Uint("w", 1920, "width in pixels")
	h := fs.Uint("h", 1080, "height in pixels")
	hz := fs.Uint("hz", 30, "refresh rate")
	if fs.Parse(args) != nil {
		return 2
	}
	out := json.NewEncoder(os.Stdout)
	fail := func(err error) int { out.Encode(map[string]any{"error": err.Error()}); return 1 }
	if err := vd.Available(); err != nil {
		return fail(fmt.Errorf("virtual displays aren't available here: %w", err))
	}
	d, err := vd.Open(vd.Spec{Name: extName, Width: uint32(*w), Height: uint32(*h), RefreshRate: float64(*hz)})
	if err != nil {
		return fail(err)
	}
	out.Encode(map[string]any{"id": d.ID()})
	io.Copy(io.Discard, os.Stdin) // returns when the parent closes it, or exits
	d.Close()
	return 0
}
