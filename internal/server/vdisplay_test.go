package server

import (
	"testing"

	vd "github.com/go-macos/virtualdisplay"
)

func TestExtSizeFor(t *testing.T) {
	cases := []struct {
		name              string
		w, h, cw, ch, dpr float64
		want              extSize
	}{
		// An older viewer: pixels as asked, 1x, bounded as before.
		{"old viewer", 2360, 1640, 0, 0, 0, extSize{2360, 1640, false}},
		{"old viewer, too big", 8000, 5000, 0, 0, 0, extSize{3840, 2160, false}},
		{"old viewer, too small", 10, 10, 0, 0, 0, extSize{640, 480, false}},
		{"old viewer, nothing", 0, 0, 0, 0, 0, extSize{640, 480, false}},
		{"old viewer, odd", 1367, 769, 0, 0, 0, extSize{1366, 768, false}},
		// A 1x laptop: unchanged.
		{"1x laptop", 0, 0, 1366, 768, 1, extSize{1366, 768, false}},
		{"zoomed 1x", 0, 0, 1093, 614, 1.25, extSize{1366, 768, false}},
		// Retina: points as the CSS size, pixels twice that.
		{"iPad", 0, 0, 1180, 820, 2, extSize{2360, 1640, true}},
		{"iPad at 3x is still 2x", 0, 0, 1180, 820, 3, extSize{2360, 1640, true}},
		{"threshold", 0, 0, 1280, 800, 1.5, extSize{2560, 1600, true}},
		{"16 inch MacBook", 0, 0, 1728, 1117, 2, extSize{3456, 2232, true}},
		// Too big: shrunk keeping its shape, within 1920 x 1200 points.
		{"5K", 0, 0, 2560, 1440, 2, extSize{3840, 2160, true}},
		{"tall", 0, 0, 1000, 3000, 2, extSize{960, 2400, true}},
		{"tiny", 0, 0, 100, 100, 2, extSize{960, 640, true}},
		// Nonsense ratios are not Retina, and the old w x h stands.
		{"huge dpr", 1920, 1080, 1180, 820, 50, extSize{1920, 1080, false}},
		{"negative", -5, -5, -1180, 820, 2, extSize{640, 480, false}},
	}
	for _, c := range cases {
		if c.want.hidpi && !retinaExtended {
			continue // Retina extended displays are off for now (retinaExtended)
		}
		got := extSizeFor(c.w, c.h, c.cw, c.ch, c.dpr)
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
		if got.w%2 != 0 || got.h%2 != 0 {
			t.Errorf("%s: odd size %+v", c.name, got)
		}
		if got.hidpi && (got.w > 2*ext2xMaxW || got.h > 2*ext2xMaxH || got.w < 2*ext2xMinW || got.h < 2*ext2xMinH || got.w%4 != 0 || got.h%4 != 0) {
			t.Errorf("%s: Retina size out of bounds %+v", c.name, got)
		}
		if !got.hidpi && (got.w > ext1xMaxW || got.h > ext1xMaxH || got.w < ext1xMinW || got.h < ext1xMinH) {
			t.Errorf("%s: 1x size out of bounds %+v", c.name, got)
		}
	}
}

func TestExtSpecFor(t *testing.T) {
	s := extSpecFor(1920, 1080, 60, false)
	if s.HiDPI || s.ProductID != 0 || s.SizeMM != (vd.Size{}) || s.Name != extName {
		t.Errorf("1x spec changed: %+v", s)
	}
	r := extSpecFor(2360, 1640, 60, true)
	if !r.HiDPI || r.Width != 2360 || r.Height != 1640 || r.Name != extName {
		t.Errorf("Retina spec: %+v", r)
	}
	if r.ProductID == 0 || r.ProductID == vd.DefaultProductID {
		t.Errorf("Retina display shares the 1x identity: %+v", r)
	}
	if ppi := float64(r.Width) * 25.4 / r.SizeMM.Width; ppi < 200 || ppi > 240 {
		t.Errorf("Retina density %.0f ppi", ppi)
	}
	for _, sp := range []vd.Spec{s, r} {
		if sp.Width < vd.MinDimension || sp.Width > vd.MaxDimension || sp.Height < vd.MinDimension || sp.Height > vd.MaxDimension {
			t.Errorf("outside virtualdisplay's limits: %+v", sp)
		}
	}
}
