// Command tailreach reaches your own computers' screens from any browser on your tailnet.
//
// Every machine runs this binary. It joins your tailnet as its own device (via
// tsnet, so no separate Tailscale setup is needed) and relays the browser's
// WebSocket to the local VNC server (macOS Screen Sharing by default).
//
// One machine runs with --hub: it joins as "devices" and serves a dashboard of every
// device at https://devices.<tailnet>.ts.net/ (or just devices/), each at /<name>/.
package main

import (
	"fmt"
	"os"

	"github.com/TanmayDoesAI/tailreach/internal/server"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			if err := setup(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "tailreach:", err)
				stuckHelp()
				os.Exit(1)
			}
			return
		case "install":
			exitOn(install(os.Args[2:]))
			return
		case "uninstall":
			exitOn(uninstall())
			return
		case "password":
			exitOn(passwordCmd(os.Args[2:]))
			return
		case "invite":
			exitOn(inviteCmd(os.Args[2:]))
			return
		case "settings":
			exitOn(openSettings(os.Args[2:]))
			return
		case "doctor":
			exitOn(doctor(os.Args[2:]))
			return
		case "version":
			if server.DebugBuild() {
				fmt.Println(server.Version, "(debug build)")
			} else {
				fmt.Println(server.Version)
			}
			return
		case "vdisplay": // internal: owns the extended display (see vdisplay.go)
			os.Exit(server.VDisplayChild(os.Args[2:]))
		}
	}

	exitOn(server.Serve(os.Args[1:]))
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailreach:", err)
		os.Exit(1)
	}
}
