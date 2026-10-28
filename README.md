# tailreach

**Your Mac's screen in any browser, anywhere, over your own Tailscale network.**
Control it from your phone on 5G, or turn a spare laptop, tablet or phone into a second monitor for it.
Nothing to install on the device you're viewing from, no port forwarding, no relay server, no accounts besides your own tailnet.

- **Remote control** from an iPhone, iPad, Android phone or any laptop's browser: touch gestures, trackpad mode, a full keyboard, clipboard both ways.
- **Second screen:** any browser can become an extra display for the Mac. The Mac creates a real virtual monitor the size of that screen; drag windows onto it like a plugged-in one.
- **Fast on bad networks:** hardware H.264 over WebRTC, with its own rate control built for mobile links that drop and come back. It cuts quickly, recovers in a couple of seconds and never builds up lag.
- **Private by design:** traffic never leaves your tailnet. Every request is checked against your Tailscale identity, and there's a device password and passkeys on top.

```
https://devices.<your-tailnet>.ts.net/          → dashboard: every device, online or not
https://devices.<your-tailnet>.ts.net/mac/      → that Mac's screen
https://devices.<your-tailnet>.ts.net/mac2/     → another one
```

## What works where

| | Status |
|---|---|
| **Mac sharing its screen** | Full: hardware H.264 over WebRTC, touch and keyboard input, clipboard, extended display, lock-screen unlock |
| **Windows or Linux sharing its screen** | Basic: point `--vnc` at a VNC server you run (TightVNC, TigerVNC…). No fast stream or extended display yet |
| **Viewing / being the second screen** | Any modern browser: Safari on iPhone and iPad, Chrome on Android, Chrome, Edge, Firefox and Safari on Windows, Mac and Linux |

A native Windows host (fast stream and extended display) is on the roadmap.

## Get started

Follow [`SETUP.md`](SETUP.md). It takes about ten minutes on a Mac: build, grant two permissions, sign in to Tailscale, set a device password.

With [Claude Code](https://claude.com/claude-code), you can also ask it to *"set up tailreach on this Mac"*. The `tailreach-setup` agent in `.claude/agents/` follows the same guide and stops for the steps only you can do.

> The program is called `mydevices` on disk (binary, login item, settings folder). Renaming it would reset the macOS permissions of existing installs.

```sh
# On the main Mac (prints a Tailscale login link the first time):
./mydevices --hub --name mac

# On other computers (reachable through the hub):
./mydevices --name mac2
#    or on its own, with its own sign-in:
./mydevices --name mac2 --via ""

# Start automatically at login (same flags):
./mydevices install --hub --name mac
./mydevices uninstall
```

Then open the dashboard on your phone. In Safari, **Share → Add to Home Screen** gives it an app icon and full screen.

## Using it

**Touch controls.** The ◎/☝︎ button switches between trackpad mode and touch mode.

| Gesture | Trackpad mode (◎) | Touch mode (☝︎) |
|---|---|---|
| One finger | Moves the cursor | Drags at that spot |
| Tap | Click at the cursor | Click at that spot |
| Hold | Starts a drag (or tap, then drag) | Right-click |

In both modes:
- Two-finger tap = right-click, two-finger drag = scroll, pinch = zoom (the view follows the cursor).
- Three-finger swipe up or down = Mission Control / App Exposé; left or right = switch Spaces.
- Three-finger tap = keyboard.

**Toolbar.**
- The ⌘ ⌥ ⌃ ⇧ buttons are sticky: they apply to the next key or click. A hardware keyboard or trackpad on an iPad works directly.
- Drag the ⠿ grip to dock the toolbar on any edge. On the left or right it sits in the black margin beside a landscape picture.
- The toolbar never covers the Mac's screen. To use every pixel, tap the status pill at the top (ping · fps) to hide it, and tap the pill again to bring it back.
- **More than one display:** pick which one to show in the **⋯** panel. It switches live, and taps, the pointer and the cursor follow it.

## Extended display (Mac)

Use a laptop's, tablet's or phone's browser as a second monitor for the Mac. Open the viewer on that device, tap the extended-display button in the toolbar and start it. The Mac gets a virtual display the size of that screen: drag windows onto it, and the mouse crosses over like a real monitor.

- **Always an extra screen:** the Mac's own display stays the main one. Nothing moves the menu bar, the Dock or your arrangement.
- **Refresh rate:** 30 Hz by default (plenty for documents, and light on the Mac); 60 Hz is a choice in the same panel.
- **Closing it:** **Exit** in the viewer closes it right away. If the connection just drops, it stays for 5 minutes so a reconnect finds your windows where you left them.
- **One device at a time.** A second device is asked whether to take over. The first is told it was taken over and doesn't fight back. Taking over can also close the extended display.
- **Caveats:**
  - It uses a private macOS API (`CGVirtualDisplay`), so a macOS update can break it.
  - The Mac keeps a small colour-profile file per display size, in the system ColorSync folder.
  - Keep the Mac's lid open.

## How it works

```
 phone / tablet / laptop browser
        │  HTTPS with a Tailscale certificate, tailnet only
        ▼
 mydevices --hub        joins your tailnet as "devices"
        │  /mac/ → this Mac        /mac2/ → forwarded over the tailnet
        ▼                               ▼
 mydevices-capture                 mydevices --name mac2
 (ScreenCaptureKit + VideoToolbox)
```

- **One Go binary** embeds Tailscale (`tsnet`). Each machine signs in with your Tailscale account the first time it runs. The machines being shared don't need the Tailscale app.
- **Capture** is a small Swift helper using ScreenCaptureKit and VideoToolbox, in low-latency mode, at the resolution the viewing screen actually displays. It also injects input and shows the "Screen shared" panel.
- **Video** goes over WebRTC (pion). It travels as UDP over the tailnet with a minimal jitter buffer, and Safari decodes it in hardware.
  - Rate control measures the queue directly (a ping every 250 ms) rather than trusting loss reports, which mobile networks distort.
  - Retransmits are rate-limited, so a dip can't turn into a resend storm.
- **Input:** taps, keys and scrolls ride WebRTC data channels. Pointer moves use a channel that never resends, so a lost packet never holds up the next move.
- **Reconnects:** after a network switch (Wi-Fi ↔ 5G), the viewer re-negotiates WebRTC while keeping the last frame on screen. If that fails twice, it falls back to a WebSocket stream decoded with WebCodecs.
- **Still screens cost nothing:** nothing is sent while the screen is still. Once things stop moving, the last frame is re-sent a few times so text sharpens up.
- **Lock screen:** macOS hides the login screen from screen capture, so unlocking goes through the Mac's own Screen Sharing (VNC). It's shown with [noVNC](https://github.com/novnc/noVNC) in `web/viewer/classic/`.

## Security

Every request has to pass all of these layers:

1. **Tailnet only.** Nothing listens on your LAN or the internet.
2. **Owner only.** Every connection is identified with Tailscale `WhoIs`. Only the Tailscale user who signed the device in gets through (`--allow` adds others), and tagged devices are rejected.
3. **Device password (set on the Mac).** Each phone, tablet or computer enters it once, then is trusted for 30 days.
   - Trust is tied to that device's Tailscale identity, so a copied cookie doesn't work anywhere else.
   - Five wrong tries lock it for 15 minutes and notify the Mac.
   - Set or reset it in the settings window (`./mydevices settings`) or from a terminal (`./mydevices password`; add `--off` to turn it off).
4. **Sign-in.** Use the device password, or a passkey (Face ID / Touch ID) as a shortcut.
   - The device password alone signs you in, so a passkey problem never locks you out.
   - With no device password set, adding a passkey needs a 6-digit code shown on the Mac.
   - Sessions are HttpOnly, Secure, SameSite=Strict cookies. They expire after 12 h, or after 1 h idle.
5. **Hub pinning.** Devices behind a hub only accept connections from that one hub. The hub's node ID is pinned the first time it connects.
6. **Origin checks.** Only pages from your tailnet can open screen sockets, and sign-in requests must come from the hub's own origin.
7. **You see every session.** A **"Screen shared" panel** sits at the top right of the Mac while someone is connected. It shows who and for how long, and has a **Stop** button. The panel is left out of the stream.
   - macOS also shows its screen-recording indicator, and notifications cover new passkeys and trusted devices.
   - Everything is written to `~/Library/Application Support/mydevices/audit.log`.
8. **Settings only on the Mac.** The settings window talks to mydevices over a Unix socket that only your macOS user can open. Nothing about settings is reachable over the network.
9. **Screen and input only.** Viewers can see the screen, point, type, scroll and use the clipboard (secrets copied from password managers are never sent). There is no way to run commands, browse files or start background processes. The one extra is the extended display, which can only add an extra screen.
10. **One viewer at a time.** Another device has to ask to take over.
11. **Stop means stop.** Pressing Stop on the Mac ends the session and refuses reconnects for 2 minutes.
    - Changing the password, revoking a device or resetting passkeys ends live sessions immediately.

**Permissions (first run on each Mac):** System Settings → Privacy & Security. Allow **Screen Recording** and **Accessibility** for `mydevices`.

Tip: in the Tailscale admin console's access controls, you can also limit which of your devices may reach `mydevices`.

## Lock screen, sleep and restarts

- **Locked Mac:** your phone shows "Mac is locked". Type the Mac password and it's entered on the real lock screen through the Mac's own Screen Sharing. The fast stream then resumes. Screen Sharing must be on.
- **Sleep:** turn on *Keep this Mac reachable while plugged in* in settings. The display still sleeps, but the Mac stays online.
- **Restart:** with FileVault on, the Mac waits at the disk-unlock screen before anything starts, so no remote tool can reach it. Someone has to type the password there once. After that, mydevices starts on its own if it's installed with `./mydevices install …`. For restarts you plan, `sudo fdesetup authrestart` skips that screen once.

## Known limitations

- **Mac only, for the fast stream and the extended display.** Windows and Linux hosts work through VNC for now.
- **No audio.** This is deliberate for now.
- **Multi-computer setups behind a hub are less isolated than a single Mac:**
  - The hub forwards the browser's cookies to the devices behind it.
  - A device's own password isn't enforced when you reach it through the hub.

  If you share more than one computer, treat everyone on your tailnet who can reach the hub as able to reach them all.
- The extended display depends on a private macOS API (see above).

## Hack on it

| What | Where |
|---|---|
| Remote-screen UI (gestures, toolbar, keys) | `web/viewer/index.html`, `app.js`, `app.css` |
| Stream, input checks, rate steering | `stream.go`, `ratectl.go`, `nacklimit.go` |
| WebRTC | `rtc.go` |
| Extended display | `vdisplay.go` |
| Screen capture, encoder, input injection (Swift) | `capture/*.swift` |
| Passkeys, sessions, audit log | `auth.go`, `security.go` |
| Server, auth, relay, discovery | `server.go` |
| Startup, flags, login | `main.go` |
| Dashboard / sign-in pages | `web/dashboard/`, `web/login.html` |
| Lock-screen VNC fallback | `web/viewer/classic/`, `web/viewer/novnc/` (upstream noVNC 1.7.0, unmodified) |
| Stream tests on a simulated bad network | `go test ./...`, `tools/streambench` |

Edit the UI live without rebuilding:

```sh
go run . --hub --name mac --web-dir ./web   # then just refresh the browser
```

Build:

```sh
make local      # this Mac: mydevices + mydevices-capture
make            # → dist/mydevices-{darwin-arm64,darwin-amd64,linux-amd64,linux-arm64,windows-amd64.exe}
```

On Linux or Windows, point `--vnc` at any VNC server (e.g. `--vnc 127.0.0.1:5901`).

`CLAUDE.md` lists the rules the code must keep: what a viewer may and may not do, how the capture helper stops, and so on. Read it before changing anything security-related.

## License

[Apache License 2.0](LICENSE).

Third-party code: [noVNC](https://github.com/novnc/noVNC) in `web/viewer/novnc/` is MPL-2.0, with its own licence files kept in that folder. The Go dependencies (Tailscale, pion, go-webauthn and others) are fetched through `go.mod` under their own permissive licences (BSD, MIT, Apache-2.0, ISC).
