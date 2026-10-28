package main

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/ebitengine/purego"
	vd "github.com/go-macos/virtualdisplay"
)

// The extended display goes back on the side of the Mac's screen where it was last left.
// macOS's own memory of an arrangement isn't enough: it keys it on the display's identity
// and on the whole set of connected monitors, so a fresh identity or a monitor plugged in
// since puts the display on the right again. Only the extended display is ever moved, and
// only its position, never its mode (a mode change would stop it closing; see vd.Close).

// placement is where the extended display sat, relative to the main display.
type placement struct {
	Side   string  `json:"side"`   // "left" | "right" | "above" | "below"
	Offset float64 `json:"offset"` // along that edge, from the main display's top or left, in points
}

const placeFile = "vdisplay.place.json"

var cgPlace struct {
	once     sync.Once
	err      error
	begin    func(cfg *uintptr) int32
	origin   func(cfg uintptr, id uint32, x, y int32) int32
	complete func(cfg uintptr, option uint32) int32
	cancel   func(cfg uintptr) int32
}

func loadCGPlace() error {
	cgPlace.once.Do(func() {
		cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			cgPlace.err = err
			return
		}
		purego.RegisterLibFunc(&cgPlace.begin, cg, "CGBeginDisplayConfiguration")
		purego.RegisterLibFunc(&cgPlace.origin, cg, "CGConfigureDisplayOrigin")
		purego.RegisterLibFunc(&cgPlace.complete, cg, "CGCompleteDisplayConfiguration")
		purego.RegisterLibFunc(&cgPlace.cancel, cg, "CGCancelDisplayConfiguration")
	})
	return cgPlace.err
}

// placeOf reads where display id sits now. ok is false if it or the main display is missing.
func placeOf(id uint32) (p placement, self, main vd.DisplayInfo, ok bool) {
	list, err := vd.ActiveDisplays()
	if err != nil {
		return p, self, main, false
	}
	var haveSelf, haveMain bool
	for _, d := range list {
		if d.ID == id {
			self, haveSelf = d, true
		} else if d.Main {
			main, haveMain = d, true
		}
	}
	if !haveSelf || !haveMain {
		return p, self, main, false
	}
	switch {
	case self.X+self.Width <= main.X:
		p = placement{"left", self.Y - main.Y}
	case self.X >= main.X+main.Width:
		p = placement{"right", self.Y - main.Y}
	case self.Y+self.Height <= main.Y:
		p = placement{"above", self.X - main.X}
	default:
		p = placement{"below", self.X - main.X}
	}
	return p, self, main, true
}

// rememberPlace saves where the extended display is now, for next time.
func rememberPlace(id uint32) {
	p, _, _, ok := placeOf(id)
	if !ok {
		return
	}
	if old, err := os.ReadFile(vdFile(placeFile)); err == nil {
		var was placement
		if json.Unmarshal(old, &was) == nil && was == p {
			return
		}
	}
	b, _ := json.Marshal(p)
	os.WriteFile(vdFile(placeFile), b, 0o600)
}

// restorePlace moves a newly opened extended display to the saved side, if it isn't there.
func restorePlace(id uint32) {
	b, err := os.ReadFile(vdFile(placeFile))
	if err != nil {
		return
	}
	var want placement
	if json.Unmarshal(b, &want) != nil {
		return
	}
	now, self, main, ok := placeOf(id)
	if !ok || now.Side == want.Side {
		return
	}
	// Touching the main display on the wanted side, the same distance along the edge
	// (kept so they still share some of it); macOS tidies up anything left overlapping.
	clamp := func(v, lo, hi float64) float64 { return min(max(v, lo), hi) }
	var x, y float64
	switch want.Side {
	case "left", "right":
		y = main.Y + clamp(want.Offset, -self.Height+1, main.Height-1)
		x = main.X + main.Width
		if want.Side == "left" {
			x = main.X - self.Width
		}
	case "above", "below":
		x = main.X + clamp(want.Offset, -self.Width+1, main.Width-1)
		y = main.Y + main.Height
		if want.Side == "above" {
			y = main.Y - self.Height
		}
	default:
		return
	}
	if loadCGPlace() != nil {
		return
	}
	var cfg uintptr
	if cgPlace.begin(&cfg) != 0 {
		return
	}
	const configureForSession = 1
	if cgPlace.origin(cfg, id, int32(x), int32(y)) != 0 {
		cgPlace.cancel(cfg)
		return
	}
	cgPlace.complete(cfg, configureForSession)
}
