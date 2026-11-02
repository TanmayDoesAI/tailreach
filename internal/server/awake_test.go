package server

import "testing"

func TestParsePower(t *testing.T) {
	for _, c := range []struct {
		out  string
		want powerInfo
	}{
		{"Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t51%; discharging; 4:01 remaining present: true\n", powerInfo{"battery", 51}},
		{"Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged; 0:00 remaining present: true\n", powerInfo{"ac", 100}},
		{"Now drawing from 'AC Power'\n", powerInfo{"ac", 0}}, // a desktop Mac
		{"", powerInfo{}},
	} {
		if got := parsePower(c.out); got != c.want {
			t.Errorf("parsePower(%q) = %+v, want %+v", c.out, got, c.want)
		}
	}
}

func TestAwakeWanted(t *testing.T) {
	ac, full, low := powerInfo{"ac", 0}, powerInfo{"battery", 80}, powerInfo{"battery", batteryFloor - 1}
	for _, c := range []struct {
		cfg  secConfig
		p    powerInfo
		want string
	}{
		{secConfig{}, ac, ""},
		{secConfig{KeepAwake: true}, ac, "ac"},
		{secConfig{KeepAwake: true}, full, "ac"}, // -s alone: macOS ignores it on battery
		{secConfig{KeepAwake: true, AwakeOnBattery: true}, ac, "all"},
		{secConfig{KeepAwake: true, AwakeOnBattery: true}, full, "all"},
		{secConfig{KeepAwake: true, AwakeOnBattery: true}, low, "ac"}, // below the floor: may sleep
		{secConfig{AwakeOnBattery: true}, full, ""},
	} {
		if got := awakeWanted(c.cfg, c.p); got != c.want {
			t.Errorf("awakeWanted(%+v, %+v) = %q, want %q", c.cfg, c.p, got, c.want)
		}
	}
}

func TestKeepAwakeDefaultsOn(t *testing.T) {
	dir := t.TempDir()
	if s := newSecurity(dir); !s.cfg.KeepAwake {
		t.Fatal("a new install should keep the Mac awake")
	}
	s := newSecurity(dir)
	s.cfg.KeepAwake = false
	s.saveLocked()
	if newSecurity(dir).cfg.KeepAwake {
		t.Fatal("a saved choice to let it sleep should stick")
	}
}
