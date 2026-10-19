# tailreach: notes for agents working in this repo

Remote control of a Mac from an iPhone/iPad browser, over the owner's own Tailscale tailnet.
Go server with embedded Tailscale (`tsnet`), a Swift capture/input helper, and a plain HTML/JS viewer.

- **Setting it up for someone:** follow `SETUP.md`, or use the `tailreach-setup` agent in `.claude/agents/`.
- **User-facing docs:** `README.md`.

## Layout

| Path | What |
|---|---|
| `main.go` | flags, subcommands (`install`, `settings`, `password`, `version`), tsnet startup |
| `server.go` | HTTP routes, Tailscale `WhoIs` owner check, hub ↔ device proxy, discovery |
| `auth.go` | passkeys (WebAuthn), sessions, pairing codes, audit log |
| `security.go` | device password (Argon2id), 30-day device trust, live sessions, local control socket |
| `stream.go` | viewer WebSocket: input/control, WS video fallback, bitrate steering |
| `rtc.go` | WebRTC video (pion): RTP packetizing with true capture timestamps, GCC estimate |
| `helper.go` | one warm `mydevices-capture` process shared across reconnects |
| `capture/*.swift` | ScreenCaptureKit + VideoToolbox H.264, CGEvent input, "Screen shared" panel, settings window |
| `web/viewer/` | the viewer (`app.js`); `classic/` + `novnc/` is the VNC fallback for the lock screen |
| `web/login.html`, `web/dashboard/` | sign-in and device list |
| `tools/streamtest` | end-to-end check over the tailnet (gets a test session from the control socket) |

## Build, run, check

```sh
make local     # build + sign both binaries (needs the "mydevices local" identity: scripts/signing-identity.sh)
make reload    # build, sign, restart the login item
go run ./tools/streamtest -rtc -t 6 https://<hub>.<tailnet>.ts.net/<name>/
CGO_ENABLED=0 GOARCH=arm64 go vet .
```

- Web files are embedded at build time. Run with `--web-dir ./web` to edit the UI without rebuilding.
- Swift builds with Command Line Tools only (`swiftc -swift-version 5`). There are no SwiftUI macros like `@State`; use `ObservableObject`.
- The binary, signing identifiers (`com.mydevices.*`) and state dir keep the `mydevices` name **on purpose**: changing them resets the macOS permissions users have granted.

## Invariants: don't break these

- **Viewers can only see the screen and send input/clipboard.** No command execution, file access or background work. The command list is `allowedCmds` in `stream.go`.
- **Every HTTP request passes `auth()` (tailnet owner via `WhoIs`).** On hub/standalone it also passes `requireSession` (device password trust plus session).
- **Settings are only reachable over the Unix control socket,** with a peer-UID check. Never add network routes for them.
- **The Mac's Stop button must stick.** The stream closes with code 4001 and new streams are refused for 2 minutes.
- **The "Screen shared" panel is shown while anyone views,** and is excluded from capture.
- **No audio.**

## Known gaps (from review; fix before advertising multi-computer use)

1. The hub proxy forwards the browser's cookies to devices and serves device HTML under the hub's origin. It should pin device node IDs, strip cookies and serve its own viewer bundle.
2. A device's own password isn't enforced behind a hub. Either make the password hub-wide or forward and verify the viewer identity.
3. Input travels over the WebSocket (TCP). A DataChannel would avoid stalls under packet loss.
4. There are no automated tests yet.

## Repo hygiene (the repo must be safe to make public at any time)

- Never commit:
  - binaries (`mydevices`, `mydevices-capture`, `dist/`)
  - state (`security.json`, `trusted.json`, `passkeys.json`, `*.state`, `audit.log`, logs)
  - keys and certificates
- Never commit real tailnet names (`tailXXXXXX.ts.net`), Tailscale IPs, device names or personal e-mails. Use placeholders like `<tailnet>`.
- Commit as the GitHub noreply address configured in this repo.
