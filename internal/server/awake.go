package server

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Keeping the Mac reachable. A sleeping Mac drops off the tailnet, and the phone just
// can't connect: the most common "it's broken". So, unless the owner turned it off, we
// hold a "no system sleep" assertion (caffeinate, which takes an IOPMAssertion for us:
// no cgo). The display still sleeps. On battery only if asked, and never below
// batteryFloor. Closing the lid on battery sleeps the Mac anyway; nothing can stop that.

const batteryFloor = 20 // percent: below this, on battery, the Mac may sleep again

// powerInfo is where the Mac's power comes from: Source "ac" or "battery" ("" if unknown).
type powerInfo struct {
	Source  string `json:"source"`
	Percent int    `json:"percent,omitempty"` // battery charge, if it has one
}

var battPct = regexp.MustCompile(`(\d+)%`)

// readPower asks pmset. A desktop Mac has no battery and is always "ac".
func readPower() powerInfo {
	if runtime.GOOS != "darwin" {
		return powerInfo{}
	}
	out, err := exec.Command("pmset", "-g", "batt").Output()
	if err != nil {
		return powerInfo{}
	}
	return parsePower(string(out))
}

func parsePower(out string) powerInfo {
	var p powerInfo
	switch {
	case strings.Contains(out, "'AC Power'"):
		p.Source = "ac"
	case strings.Contains(out, "'Battery Power'"):
		p.Source = "battery"
	}
	if m := battPct.FindStringSubmatch(out); m != nil {
		p.Percent, _ = strconv.Atoi(m[1])
	}
	return p
}

// awakeWanted is what to hold: "" nothing, "ac" no system sleep on power (caffeinate -s
// is ignored on battery by macOS), "all" on battery too (-i keeps idle sleep off there).
func awakeWanted(cfg secConfig, p powerInfo) string {
	switch {
	case !cfg.KeepAwake:
		return ""
	case cfg.AwakeOnBattery && (p.Source != "battery" || p.Percent >= batteryFloor):
		return "all"
	}
	return "ac"
}

// applyKeepAwake starts, swaps or stops the assertion to match the settings and the
// current power source. Called on start, on every change, and each minute (keepAwakeLoop).
func (s *security) applyKeepAwake() {
	if runtime.GOOS != "darwin" {
		return
	}
	p := readPower()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.power = p
	want := awakeWanted(s.cfg, p)
	if want == s.awakeMode {
		return
	}
	if s.awake != nil {
		s.awake.Process.Kill()
		s.awake.Wait()
		s.awake = nil
	}
	s.awakeMode = ""
	if want == "" {
		return
	}
	args := []string{"-s"}
	if want == "all" {
		args = append(args, "-i")
	}
	// -w: it ends with us, even if we're killed outright.
	c := exec.Command("caffeinate", append(args, "-w", strconv.Itoa(os.Getpid()))...)
	if c.Start() == nil {
		s.awake, s.awakeMode = c, want
	}
}

// keepAwakeLoop follows the power source and battery level (unplugged, the floor).
func (s *security) keepAwakeLoop() {
	for range time.Tick(time.Minute) {
		s.applyKeepAwake()
	}
}

// awakeStatus is the keep-awake part of the control socket's status reply.
func (s *security) awakeStatus(st map[string]any) {
	st["keepAwake"] = s.cfg.KeepAwake
	st["awakeOnBattery"] = s.cfg.AwakeOnBattery
	st["batteryFloor"] = batteryFloor
	st["power"] = s.power
	// held: the Mac won't sleep by itself right now.
	st["awakeHeld"] = s.awakeMode == "all" || (s.awakeMode == "ac" && s.power.Source != "battery")
}
