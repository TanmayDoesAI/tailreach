package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
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
// main one (nothing here moves the menu bar or the Dock, or the Mac's own displays). The
// one arrangement change is putting the extended display back where it was last left
// (vdisplay_place_darwin.go). The
// capture helper already lists every display and streams the one the viewer picks, so all
// this adds is creating and removing the display.
//
// The display lives in a small CHILD PROCESS (`tailreach vdisplay`, below), because the
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
	size     extSize // as asked for, so a Retina ask that came up 1x isn't reopened each time
	retina   bool    // it really came up in a Retina mode
	hz       float64 // the rate it's streamed at (the display itself runs at 60 Hz)
	viewers  int     // streams open right now
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

// extSize is the display to open: w x h pixels, and with hidpi half that in points at a
// backing scale of 2 (Retina: sharp, with normal-sized text).
type extSize struct {
	w, h  uint32
	hidpi bool
}

// Bounds. Few distinct sizes: every new one leaves a colour profile file behind in the
// system folder (the package docs say so). A Retina display is 480 x 320 to 1920 x 1200
// points (so at most 3840 x 2400 pixels: a 16" MacBook's screen fits).
const (
	ext1xMinW, ext1xMinH, ext1xMaxW, ext1xMaxH = 640, 480, 3840, 2160 // pixels
	ext2xMinW, ext2xMinH, ext2xMaxW, ext2xMaxH = 480, 320, 1920, 1200 // points
	extRetinaDPR                               = 1.5                  // from this pixel ratio up, Retina
)

// extSizeFor turns the viewer's ask into a bounded display size. cw x ch is its screen in
// CSS pixels and dpr its device pixel ratio; without them (an older viewer) w x h is the
// 1x pixel size it wants.
func extSizeFor(w, h, cw, ch, dpr float64) extSize {
	px := func(v float64) uint32 { return uint32(min(max(math.Round(v), 0), 1<<16)) }
	if cw > 0 && ch > 0 && dpr >= extRetinaDPR && dpr <= 8 {
		// Shrunk to fit keeping its shape (clicks are mapped by fraction of the screen, so
		// a stretched desktop would feel off), even sizes. The pixel size is twice the
		// points: that's the Retina mode virtualdisplay advertises for a HiDPI display.
		f := min(1, ext2xMaxW/cw, ext2xMaxH/ch)
		pw := clampU(px(cw*f), ext2xMinW, ext2xMaxW) &^ 1
		ph := clampU(px(ch*f), ext2xMinH, ext2xMaxH) &^ 1
		return extSize{2 * pw, 2 * ph, true}
	}
	if cw > 0 && ch > 0 && dpr > 0 && dpr <= 8 {
		w, h = cw*dpr, ch*dpr
	}
	return extSize{clampU(px(w), ext1xMinW, ext1xMaxW) &^ 1, clampU(px(h), ext1xMinH, ext1xMaxH) &^ 1, false}
}

// extendDisplay creates the virtual display (or reuses the one already open with these
// settings) and returns its display ID, and whether it came up Retina.
func extendDisplay(size extSize, hz float64) (uint32, bool, error) {
	if !extFlight.CompareAndSwap(false, true) {
		return 0, false, fmt.Errorf("busy: try again in a moment")
	}
	defer extFlight.Store(false)
	// The refresh rate is either 30 or 60.
	if hz < 45 {
		hz = 30
	} else {
		hz = 60
	}
	if err := vdDisabled(); err != nil {
		return 0, false, err
	}

	ext.mu.Lock()
	defer ext.mu.Unlock()
	// The display itself always runs at 60 Hz; hz is the frame rate it is streamed at. So a
	// new rate only restarts the capture (the viewer does that), and the display, with the
	// windows on it, stays where it is. A new size or Retina-ness is a new display (closed
	// and opened again), never a mode change.
	if ext.cmd != nil && ext.size == size {
		if ext.hz != hz {
			log.Printf("extended display %d: streaming at %.0f fps", ext.id, hz)
		}
		ext.hz = hz
		return ext.id, ext.retina, nil
	}
	if ext.cmd != nil && time.Since(ext.openedAt) < 2*time.Second {
		return 0, false, fmt.Errorf("changed too fast: try again in a moment")
	}
	closeExtendedLocked()
	id, err := startChildLocked(size, hz)
	if err != nil {
		return 0, false, err
	}
	if ext.viewers == 0 {
		armIdleLocked() // opened for a stream that has already ended
	}
	return id, ext.retina, nil
}

func startChildLocked(size extSize, hz float64) (uint32, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	args := []string{"vdisplay", "-w", strconv.Itoa(int(size.w)), "-h", strconv.Itoa(int(size.h)), "-hz", "60"}
	if size.hidpi {
		args = append(args, "-hidpi")
	}
	cmd := exec.Command(exe, args...)
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
		ID     uint32 `json:"id"`
		Retina bool   `json:"retina"`
		Error  string `json:"error"`
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
		// feature off (the rest of tailreach is unaffected) until the file is deleted.
		os.MkdirAll(DefaultStateDir(), 0o700)
		os.WriteFile(vdFile("vdisplay.disabled"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
		log.Printf("extended display: the display process crashed; feature turned off")
		return 0, vdDisabled()
	}

	ext.cmd, ext.stdin, ext.done = cmd, stdin, done
	ext.id, ext.size, ext.retina, ext.hz, ext.openedAt = r.ID, size, r.Retina, hz, time.Now()
	switch {
	case r.Retina:
		log.Printf("extended display %d opened: %dx%d points, Retina (%dx%d pixels), streamed at %.0f fps", r.ID, size.w/2, size.h/2, size.w, size.h, hz)
	case size.hidpi:
		log.Printf("extended display %d opened: %dx%d, 1x (macOS didn't start it Retina), streamed at %.0f fps", r.ID, size.w, size.h, hz)
	default:
		log.Printf("extended display %d opened: %dx%d, streamed at %.0f fps", r.ID, size.w, size.h, hz)
	}
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

// ---------- the child: `tailreach vdisplay -w W -h H -hz HZ [-hidpi]` ----------

func vdFile(name string) string { return filepath.Join(DefaultStateDir(), name) }

// vdDisabled says why the feature is off after a crash of the display process.
func vdDisabled() error {
	if _, err := os.Stat(vdFile("vdisplay.disabled")); err == nil {
		return fmt.Errorf("the extended display was turned off after a crash in the macOS display API; delete %s to try again", vdFile("vdisplay.disabled"))
	}
	return nil
}

// vdisplayChild owns one virtual display until its stdin closes (the parent asked, or died).
// It prints one JSON line: {"id":N,"retina":B} once the display is up, or {"error":"..."}.
func VDisplayChild(args []string) int {
	fs := flag.NewFlagSet("vdisplay", flag.ContinueOnError)
	w := fs.Uint("w", 1920, "width in pixels")
	h := fs.Uint("h", 1080, "height in pixels")
	hz := fs.Uint("hz", 30, "refresh rate")
	hidpi := fs.Bool("hidpi", false, "Retina: half the pixel size in points")
	if fs.Parse(args) != nil {
		return 2
	}
	out := json.NewEncoder(os.Stdout)
	fail := func(err error) int { out.Encode(map[string]any{"error": err.Error()}); return 1 }
	if err := vd.Available(); err != nil {
		return fail(fmt.Errorf("virtual displays aren't available here: %w", err))
	}
	d, err := vd.Open(extSpecFor(uint32(*w), uint32(*h), float64(*hz), *hidpi))
	if err != nil && *hidpi && (errors.Is(err, vd.ErrWrongMode) || errors.Is(err, vd.ErrRejected)) {
		// A 1x display of the same pixels is what the viewer got before Retina.
		log.Printf("extended display: Retina refused (%v), opening it 1x", err)
		d, err = vd.Open(extSpecFor(uint32(*w), uint32(*h), float64(*hz), false))
	}
	if err != nil {
		return fail(err)
	}
	restorePlace(d.ID())
	out.Encode(map[string]any{"id": d.ID(), "retina": d.ActiveMode().HiDPI()})
	// Note where it is every few seconds (and last thing), so a move is kept even if
	// tailreach is restarted while it's open.
	stop := make(chan struct{})
	go func() {
		for t := time.NewTicker(5 * time.Second); ; {
			select {
			case <-stop:
				return
			case <-t.C:
				rememberPlace(d.ID())
			}
		}
	}()
	io.Copy(io.Discard, os.Stdin) // returns when the parent closes it, or exits
	close(stop)
	rememberPlace(d.ID())
	d.Close()
	return 0
}

// Retina is asked for, not switched into: virtualdisplay never changes a display's mode
// (that would stop it closing), and with HiDPI it only advertises the Retina modes, half
// the pixels in points. Which mode a new display starts in is macOS's pick, made as for a
// plugged-in monitor, from the physical size it reports: so a Retina display says it is as
// dense as a Retina MacBook. A product ID of its own keeps it apart from the 1x display of
// the same pixel size, whose remembered 1x mode macOS would otherwise bring back.
const (
	extRetinaProduct = vd.DefaultProductID + 1
	extRetinaPPI     = 220 // pixels per inch, about a Retina MacBook's
)

func extSpecFor(w, h uint32, hz float64, hidpi bool) vd.Spec {
	s := vd.Spec{Name: extName, Width: w, Height: h, RefreshRate: hz}
	if hidpi {
		s.HiDPI = true
		s.ProductID = extRetinaProduct
		s.SizeMM = vd.Size{Width: float64(w) * 25.4 / extRetinaPPI, Height: float64(h) * 25.4 / extRetinaPPI}
	}
	return s
}
