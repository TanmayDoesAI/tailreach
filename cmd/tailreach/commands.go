package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/TanmayDoesAI/tailreach/internal/server"
	"golang.org/x/term"
)

// openSettings opens the native settings window, talking to the running tailreach over its local socket.
func openSettings(args []string) error {
	fs := flag.NewFlagSet("settings", flag.ExitOnError)
	state := fs.String("state", server.DefaultStateDir(), "state directory of the running tailreach")
	fs.Parse(args)
	sock := server.ControlSocketPath(*state)
	if _, err := os.Stat(sock); err != nil {
		return fmt.Errorf("tailreach isn't running (no %s)", sock)
	}
	cmd := exec.Command(server.HelperPath(), "--settings", sock)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// passwordCmd sets, resets or turns off the device password via the running tailreach.
func passwordCmd(args []string) error {
	fs := flag.NewFlagSet("password", flag.ExitOnError)
	state := fs.String("state", server.DefaultStateDir(), "state directory of the running tailreach")
	off := fs.Bool("off", false, "turn the password off")
	fs.Parse(args)
	// Behind a hub there's no password of its own to set: say so before asking for one.
	if st, err := controlCall(*state, map[string]any{"op": "status"}); err == nil && st["mode"] == "behindHub" {
		return fmt.Errorf("this Mac is reached through your hub (%v), so the password is set there: run `tailreach password` on the hub. Signing in on the hub opens every Mac behind it", st["hub"])
	}
	req := map[string]any{"op": "enablePassword", "enabled": false}
	if !*off {
		fmt.Print("New device password (8+ characters): ")
		p1, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		fmt.Print("Again: ")
		p2, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if string(p1) != string(p2) {
			return fmt.Errorf("passwords don't match")
		}
		req = map[string]any{"op": "setPassword", "password": string(p1)}
	}
	c, err := net.Dial("unix", server.ControlSocketPath(*state))
	if err != nil {
		return fmt.Errorf("tailreach isn't running: %v", err)
	}
	defer c.Close()
	json.NewEncoder(c).Encode(req)
	var reply map[string]any
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		return err
	}
	if e, ok := reply["error"].(string); ok {
		return errors.New(e)
	}
	if *off {
		fmt.Println("✓ Password turned off")
	} else {
		fmt.Println("✓ Password set. Every device will be asked for it again (then trusted for 30 days).")
	}
	return nil
}

// inviteCmd lets someone else in for a while, from this Mac only (the running tailreach,
// over its control socket): `tailreach invite name@example.com [--hours N] [--view-only]`,
// `--list`, `--remove name@example.com`.
func inviteCmd(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	state := fs.String("state", server.DefaultStateDir(), "state directory of the running tailreach")
	hours := fs.Int("hours", 24, "how long the invite lasts, in hours (up to 720 = 30 days)")
	viewOnly := fs.Bool("view-only", false, "they can watch the screen, not control it")
	list := fs.Bool("list", false, "list running invites")
	remove := fs.String("remove", "", "end this login's invite now (and anything they have open)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: tailreach invite THEIR-TAILSCALE-LOGIN [--hours N] [--view-only]\n       tailreach invite --list\n       tailreach invite --remove THEIR-TAILSCALE-LOGIN")
		fs.PrintDefaults()
	}
	// Flags may come before or after the login.
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	call := func(req map[string]any) (map[string]any, error) {
		st, err := controlCall(*state, req)
		if err != nil {
			return nil, fmt.Errorf("tailreach isn't running: %v", err)
		}
		if e, ok := st["error"].(string); ok {
			return nil, errors.New(e)
		}
		return st, nil
	}
	switch {
	case *remove != "":
		if _, err := call(map[string]any{"op": "uninvite", "login": *remove}); err != nil {
			return err
		}
		fmt.Printf("✓ %s can't get in any more; anything they had open was ended.\n", *remove)
		return nil
	case *list:
		st, err := call(map[string]any{"op": "status"})
		if err != nil {
			return err
		}
		inv, _ := st["invites"].([]any)
		if len(inv) == 0 {
			fmt.Println("No guests invited.")
			return nil
		}
		for _, x := range inv {
			m, _ := x.(map[string]any)
			exp, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(m["expires"]))
			mode := "can control"
			if m["viewOnly"] == true {
				mode = "view only"
			}
			code := "code not used yet"
			if _, used := m["used"]; used {
				code = "signed in"
			}
			fmt.Printf("  %s  %s, until %s (%s)\n", m["login"], mode, exp.Local().Format("Mon Jan 2 15:04"), code)
		}
		return nil
	case len(pos) != 1:
		fs.Usage()
		os.Exit(2)
	}
	if *hours < 1 || *hours > 720 {
		return fmt.Errorf("--hours is 1 to 720 (30 days)")
	}
	st, err := call(map[string]any{"op": "invite", "login": pos[0], "hours": *hours, "viewOnly": *viewOnly})
	if err != nil {
		return err
	}
	in, _ := st["invited"].(map[string]any)
	exp, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(in["expires"]))
	mode := "can see and control this Mac"
	if *viewOnly {
		mode = "can watch this Mac's screen, not control it"
	}
	fmt.Printf(`✓ Invited %s until %s: %s.

What they need:
  1. You share this Mac with them in Tailscale (free, works across Tailscale accounts):
     %s → the machine "%s" → ⋯ → Share… → send them the link it makes.
  2. They accept it, signed in to Tailscale as %s, then open
     %s
  3. The sign-in page asks them for this invite code:  %s
     (one use, and it ends with the invite. Run this again for a new code.)

This Mac shows who's connected while they view. End it any time:
  tailreach invite --remove %s
`, in["login"], exp.Local().Format("Mon Jan 2 15:04"), mode, in["admin"], in["machine"], in["login"], in["address"], in["code"], in["login"])
	return nil
}
