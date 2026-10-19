# mydevices

Your own computers' screens, from any browser on your tailnet: iPhone, iPad, Android or another laptop. It works like Chrome Remote Desktop, but traffic stays inside your own Tailscale network.

```
https://mydevices.<your-tailnet>.ts.net/          → dashboard: every device, online or not
https://mydevices.<your-tailnet>.ts.net/mac/      → that Mac's screen
https://mydevices.<your-tailnet>.ts.net/mac2/     → another one
```

## How it works

```
 iPhone / iPad / laptop browser
        │  HTTPS (Tailscale cert), tailnet only
        ▼
 mydevices --hub   (joins your tailnet as "mydevices")
        │  /mac/ → local      /mac2/ → forwarded over the tailnet
        ▼                          ▼
 Screen Sharing (VNC :5900)   mydevices --name mac2 → its Screen Sharing
```

- A single binary embeds Tailscale (`tsnet`). Each machine signs in with your own Tailscale account the first time it runs. You don't need the Tailscale app on the machines being shared.
- The browser side is [noVNC](https://github.com/novnc/noVNC) (MPL-2.0) plus a small touch-friendly UI in `web/viewer/`.

## Security

1. **Tailnet only.** Nothing listens on your LAN or the internet. Only devices on your tailnet can reach it.
2. **Owner only.** Every request is checked with Tailscale `WhoIs`. Only the Tailscale user who signed the device in gets through (`--allow a@b.com` adds others), and tagged devices are rejected.
3. **Encrypted twice.** Browser to hub uses HTTPS with a real Tailscale certificate, and every hop is inside WireGuard.
4. **Mac login still required.** The Screen Sharing session asks for the Mac's username and password.
5. **Socket origin check.** Only pages from your tailnet can open the screen WebSocket, which blocks other websites from piggybacking.

Tip: in the Tailscale admin console's Access Controls, you can also limit which of your devices may reach `mydevices`.

## Use it

```sh
# 1. On each Mac: System Settings → General → Sharing → Screen Sharing: on

# 2. On the main Mac (prints a Tailscale login link the first time):
./mydevices --hub --name mac

# 3. On other computers:
./mydevices --name mac2

# 4. Start automatically at login (same flags):
./mydevices install --hub --name mac
./mydevices uninstall
```

Then open the dashboard link on your phone. In Safari, use **Share → Add to Home Screen** to get an app icon.

**Touch controls:** tap = click, two-finger tap = right-click, two-finger drag = scroll, long-press then drag = drag, pinch = zoom. The ⌨︎ button opens the keyboard, and ⌘ ⌥ ⌃ ⇧ are sticky (they apply to the next key).

## Hack on it

| What | Where |
|---|---|
| Remote-screen UI (toolbar, keys, login) | `web/viewer/index.html`, `app.js`, `app.css` |
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
make            # → dist/mydevices-{darwin-arm64,darwin-amd64,linux-amd64,linux-arm64,windows-amd64.exe}
```

On Linux or Windows, point `--vnc` at any VNC server (e.g. `--vnc 127.0.0.1:5901` for TigerVNC or TightVNC).
