package server

import (
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// The menu-bar item (`tailreach-capture --menubar SOCK`, macos/menubar.swift): tailreach's
// presence on the Mac. It only talks to the control socket, like the settings window.
// We start it, restart it if it exits (with backoff), and stop it by closing its stdin,
// which it also sees if we die. Never more than one: one supervisor, one process.

// Exit codes from the menu bar that mean "don't start me again": no window server (an SSH
// session, a headless Mac) or another one already running.
const (
	menuBarNoGUI     = 3
	menuBarDuplicate = 4
)

var menuBar = struct {
	sync.Mutex
	once  sync.Once
	want  bool
	stdin io.Closer // of the running one, or nil
	kick  chan struct{}
}{kick: make(chan struct{}, 1)}

// startMenuBar supervises the menu-bar item from now on (shown if want).
func startMenuBar(sock string, want bool) {
	if runtime.GOOS != "darwin" {
		return
	}
	if _, err := os.Stat(HelperPath()); err != nil {
		return
	}
	menuBar.once.Do(func() {
		menuBar.Lock()
		menuBar.want = want
		menuBar.Unlock()
		go superviseMenuBar(sock)
	})
}

// setMenuBar shows or hides it (the "Hide menu-bar icon" setting).
func setMenuBar(on bool) {
	menuBar.Lock()
	menuBar.want = on
	if !on && menuBar.stdin != nil {
		menuBar.stdin.Close()
	}
	menuBar.Unlock()
	select {
	case menuBar.kick <- struct{}{}:
	default:
	}
}

// stopMenuBar ends it as tailreach quits.
func stopMenuBar() {
	menuBar.Lock()
	menuBar.want = false
	if menuBar.stdin != nil {
		menuBar.stdin.Close()
	}
	menuBar.Unlock()
}

func superviseMenuBar(sock string) {
	backoff := time.Second
	for {
		menuBar.Lock()
		want := menuBar.want
		menuBar.Unlock()
		if !want {
			<-menuBar.kick
			continue
		}
		cmd := exec.Command(HelperPath(), "--menubar", sock)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("menu bar: %v", err)
			return
		}
		dlog("menu bar started (pid %d)", cmd.Process.Pid)
		menuBar.Lock()
		menuBar.stdin = stdin
		menuBar.Unlock()
		started := time.Now()
		err = cmd.Wait()
		menuBar.Lock()
		menuBar.stdin = nil
		want = menuBar.want
		menuBar.Unlock()
		var ee *exec.ExitError
		if errors.As(err, &ee) && (ee.ExitCode() == menuBarNoGUI || ee.ExitCode() == menuBarDuplicate) {
			dlog("menu bar: not shown (exit %d)", ee.ExitCode())
			return
		}
		if !want {
			continue // hidden: wait to be shown again
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		dlog("menu bar exited (%v), restarting in %s", err, backoff)
		select {
		case <-time.After(backoff):
		case <-menuBar.kick:
		}
		backoff = min(backoff*2, time.Minute)
	}
}
