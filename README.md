# mydevices

Your own computers' screens, from any browser on your tailnet: iPhone, iPad, Android or another laptop. It works like Chrome Remote Desktop, but traffic stays inside your own Tailscale network.

```
devices/                                        → type this in any browser on your tailnet
https://devices.<your-tailnet>.ts.net/          → dashboard: every device, online or not
https://devices.<your-tailnet>.ts.net/mac/      → that Mac's screen
https://devices.<your-tailnet>.ts.net/mac2/     → another one
```

## How it works

```
 iPhone / iPad / laptop browser
        │  HTTPS (Tailscale cert), tailnet only
        ▼
 mydevices --hub   (joins your tailnet as "devices")
        │  /mac/ → local      /mac2/ → forwarded over the tailnet
        ▼                          ▼
 Screen Sharing (VNC :5900)   mydevices --name mac2 → its Screen Sharing
```

- A single binary embeds Tailscale (`tsnet`). Each machine signs in with your own Tailscale account the first time it runs. You don't need the Tailscale app on the machines being shared.
- The browser side is [noVNC](https://github.com/novnc/noVNC) (MPL-2.0) plus a small touch-friendly UI in `web/viewer/`.

## Speed

On a Mac, the screen is streamed as **hardware H.264** (ScreenCaptureKit + VideoToolbox in low-latency mode), at the resolution your phone or tablet actually displays. The browser decodes it in hardware (WebCodecs) and draws each frame the moment it arrives.

- Nothing is sent while the screen is still. Once things stop moving, the last frame is re-sent a few times so text sharpens up.
- If the network can't keep up, the hub skips ahead to a fresh keyframe and lowers the bitrate. It never builds up lag. The bitrate climbs back up once the network is calm.
- The status pill shows round-trip time, frames per second and bitrate.

`classic/` (noVNC) is the fallback for unlocking the Mac's login screen, which macOS doesn't let screen capture see.

## Security

Every request has to pass all of these layers:

1. **Tailnet only.** Nothing listens on your LAN or the internet.
2. **Owner only.** Every connection is identified with Tailscale `WhoIs`. Only the Tailscale user who signed the device in gets through (`--allow` adds others), and tagged devices are rejected.
3. **Device password (yours, set on the Mac).** Each phone, tablet or computer enters it once, then is trusted for 30 days.
   - Trust is tied to that device's Tailscale identity, so a copied cookie doesn't work anywhere else.
   - Five wrong tries lock it for 15 minutes and send a notification to the Mac.
   - Set or reset it in the settings window (`./mydevices settings`) or from a terminal (`./mydevices password`; add `--off` to turn it off).
4. **Passkey (Face ID / Touch ID).** Opening a session takes one Face ID prompt.
   - A new device adds its passkey after entering the device password. With no password set, it needs a 6-digit code shown on the Mac instead.
   - Sessions are HttpOnly, Secure, SameSite=Strict cookies. They expire after 12 h, or after 1 h idle.
5. **Hub pinning.** Devices behind a hub only accept connections from that one hub. The hub's node ID is pinned the first time it connects, so another machine on your tailnet can't pose as it.
6. **Origin checks.** Only pages from your tailnet can open screen sockets, and sign-in requests must come from the hub's own origin.
7. **You see every session.** A **"Screen shared" panel** sits at the top right of the Mac while someone is connected. It shows who and for how long, and has a **Stop** button. The panel is left out of the stream.
   - macOS also shows its screen-recording indicator, and notifications cover new passkeys and trusted devices. Everything is written to `~/Library/Application Support/mydevices/audit.log`.
8. **Settings only on the Mac.** The settings window talks to mydevices over a Unix socket that only your macOS user can open. Nothing about settings is reachable over the network.
9. **Screen and input only.** Viewers can see the screen, point, type, scroll and use the clipboard (secrets copied from password managers are never sent). There is no way to run commands, browse files or start background processes. The screen helper only exists during a session.
10. **One viewer at a time.** A new session ends the previous one.

To reset passkeys, delete `~/Library/Application Support/mydevices/mydevices/passkeys.json` and restart.

**Permissions (first run on each Mac):** System Settings → Privacy & Security. Allow **Screen Recording** and **Accessibility** for `mydevices` (or for your terminal app, if you run it from a terminal).

Tip: in the Tailscale admin console's Access Controls, you can also limit which of your devices may reach `mydevices`.

## Lock screen, sleep and restarts

- **Locked Mac:** your phone shows "Mac is locked". Type the Mac password and it's entered on the real lock screen through the Mac's own Screen Sharing. The fast stream then resumes. Screen Sharing must be on.
- **Sleep:** turn on *Keep this Mac reachable while plugged in* in settings. The display still sleeps, but the Mac stays online.
- **Restart:** with FileVault on, the Mac waits at the disk-unlock screen before anything starts, so no remote tool can reach it. Someone has to type the password there once. Then mydevices starts on its own, if it's installed with `./mydevices install …`. For restarts you plan, `sudo fdesetup authrestart` skips that screen once.

## Use it

```sh
# 1. On each Mac: System Settings → General → Sharing → Screen Sharing: on

# 2. On the main Mac (prints a Tailscale login link the first time):
./mydevices --hub --name mac

# 3. On other computers (only reachable through the hub):
./mydevices --name mac2
#    or on its own, with its own Face ID sign-in:
./mydevices --name mac2 --via ""

# 4. Start automatically at login (same flags):
./mydevices install --hub --name mac
./mydevices uninstall
```

Then open the dashboard link on your phone. In Safari, use **Share → Add to Home Screen** to get an app icon.

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

The ⌘ ⌥ ⌃ ⇧ buttons are sticky: they apply to the next key or click. A hardware keyboard or trackpad on an iPad works directly.

## Hack on it

| What | Where |
|---|---|
| Remote-screen UI (gestures, toolbar, keys) | `web/viewer/index.html`, `app.js`, `app.css` |
| Classic VNC fallback | `web/viewer/classic/` |
| Face ID sign-in page | `web/login.html` |
| Screen capture, encoder, input injection (Swift) | `capture/main.swift` |
| Stream relay and congestion control | `stream.go` |
| Passkeys, sessions, audit log | `auth.go` |
| Dashboard | `web/dashboard/index.html` (one file) |
| Server, auth, relay, discovery | `server.go` |
| Startup, flags, login | `main.go` |
| The VNC client itself | `web/viewer/novnc/` (upstream noVNC 1.7.0, unmodified) |

Edit the UI live without rebuilding:

```sh
go run . --hub --name mac --web-dir ./web   # then just refresh the browser
```

Build release binaries:

```sh
make local      # this Mac: mydevices + mydevices-capture
make            # → dist/mydevices-{darwin-arm64,darwin-amd64,linux-amd64,linux-arm64,windows-amd64.exe}
```

On Linux or Windows, point `--vnc` at any VNC server (e.g. `--vnc 127.0.0.1:5901` for TigerVNC or TightVNC).
