# tailreach: notes for coding agents (and people) working in this repo

This file is read by Codex, Cursor, Gemini CLI and most other coding agents. Claude Code reads it
through `CLAUDE.md`.

Remote control of a Mac from an iPhone/iPad browser, over the owner's own Tailscale tailnet.
Go server with embedded Tailscale (`tsnet`), a Swift capture/input helper, and a plain HTML/JS viewer.

- **Setting it up for someone:**
  - Run `make setup`, then work through what `./tailreach doctor` reports until it exits 0.
  - Follow the "Rules for agents" in `SETUP.md`: never set passwords or privacy settings, and never approve the sign-in for them.
  - Claude Code also has a `tailreach-setup` agent in `.claude/agents/`.
- **User-facing docs:** `README.md`.

## Layout

| Path | What |
|---|---|
| `main.go` | flags, subcommands (`install`, `settings`, `password`, `invite`, `doctor`, `version`), tsnet startup |
| `setup.go` | `tailreach setup`: signs an installed (Homebrew) copy, login item, doctor; the formula lives in the tap repo [TanmayDoesAI/homebrew-tailreach](https://github.com/TanmayDoesAI/homebrew-tailreach): update its `url` and `sha256` for each release tag |
| `doctor.go` | `tailreach doctor`: checks the whole setup, prints the fix for each problem |
| `server.go` | HTTP routes, Tailscale `WhoIs` owner check, hub ↔ device proxy, discovery |
| `auth.go` | passkeys (WebAuthn), sessions, pairing codes, guest invite-code sign-in, audit log |
| `invites.go` | guests: expiring invites for other Tailscale logins, one-time codes (hashed, single use, rate limited), `invites.json` |
| `security.go` | device password (Argon2id), 30-day device trust, live sessions, local control socket |
| `stream.go` | viewer WebSocket: control, input dispatch (`forward`), WS video/input fallback, bitrate steering |
| `rtc.go` | WebRTC (pion): video with true capture timestamps, NACK + RTCP reports only; queue probe (`sping`/`spong`); `input` (reliable) and `move` (unordered, no resends) data channels |
| `ratectl.go`, `nacklimit.go` | the rate controller (cut fast, jump back to the last good rate, probe gently) and the cap on retransmits; see `docs/streaming.md` |
| `debug*.go` | `dlog` and the `debug` build tag (`make DEBUG=1`) |
| `vrec.go` | measurement mode: the viewer's own receive stats, logged on the Mac (switched on from the control socket) |
| `vdisplay*.go` | extended display: a virtual second monitor (private CGVirtualDisplay via `go-macos/virtualdisplay`), child process `tailreach vdisplay` owns it, idle auto-close; never the main display |
| `files.go` | files sent from the viewer, saved as new files in `~/Downloads` (write-only) |
| `vnchold.go` | keeps the phone's unlock connection to Screen Sharing open (idle) while anyone views, since macOS re-locks when it ends |
| `helper.go` | one warm `tailreach-capture` process shared across reconnects |
| `capture/*.swift` | ScreenCaptureKit + VideoToolbox H.264, CGEvent input, "Screen shared" panel, settings window |
| `web/viewer/` | the viewer (`app.js`); `classic/` + `novnc/` is the VNC fallback for the lock screen |
| `web/login.html`, `web/dashboard/` | sign-in and device list |
| `tools/streamtest` | end-to-end check over the tailnet (gets a test session from the control socket) |
| `tools/streambench` | WebRTC benchmark through a simulated bad network (drops, blackouts, rate changes) |
| `tools/phonetest` | record and summarise a real phone session (with `vrec.go`) |
| `tools/pattern` | animated test-pattern window, for benchmarking and glass-to-glass latency |

## Build, run, check

```sh
make local     # build + sign both binaries (needs the "tailreach local" identity: scripts/signing-identity.sh)
make reload    # build, sign, restart the login item
make setup     # first-time setup on a Mac (safe to re-run); ends with ./tailreach doctor
./tailreach doctor   # what's working, what isn't, and the fix (exit 1 = something to fix)
go run ./tools/streamtest -rtc -t 6 https://<hub>.<tailnet>.ts.net/<name>/
CGO_ENABLED=0 GOARCH=arm64 go vet .
```

- **Never run `tailreach-capture` directly, and test against the login item (`make reload`), not a terminal-run hub.**
  - macOS credits screen recording to whatever app launched the process, so a terminal run shows the terminal as "sharing".
  - The helper refuses to start unless tailreach launched it (`TAILREACH_PARENT`).
- **Debug logging:** `make DEBUG=1 reload`.
  - Use `dlog(...)` in Go and `dlog(...)` in Swift for anything worth seeing while debugging. Both compile to nothing in a normal build, so don't use `log.Printf` for chatty lines.
  - In Go, put any extra work for a log line inside `if debugLog { … }`.
  - The viewer's `dlog` only runs when a debug build sends `{"t":"debug"}`. It reaches the log as a `dlog` message, which the server accepts only in debug builds.
- Web files are embedded at build time. Run with `--web-dir ./web` to edit the UI without rebuilding.
- Swift builds with Command Line Tools only (`swiftc -swift-version 5`). There are no SwiftUI macros like `@State`; use `ObservableObject`.
- Don't rename the binary, signing identifiers (`com.tailreach.*`) or state dir again: macOS ties granted permissions to them. Before 0.2 they were `mydevices`; `migrate.go` moves an old install over.

## Invariants: don't break these

- **Viewers can only see the screen and send input/clipboard.** No command execution, file access or background work. The command list is `allowedCmds` in `stream.go`, and data-channel input goes through the same `forward` check.
  - The one file exception is `files.go`: a viewer can add a new file to `~/Downloads` (never overwrite, read, list or open one), up to 4 GB.
- **Clicks from absolute pointing (touch mode, mouse) carry their own `x`,`y`.** Moves travel on the unreliable `move` channel, so a click must not depend on the move before it having arrived.
- **Every HTTP request passes `auth()` (tailnet owner via `WhoIs`).** On hub/standalone it also passes `requireSession` (device password trust plus session).
- **Guests are invite-only and expiring.** `auth()` admits another login only while it has a running invite (`invites.go`), created on the Mac (`tailreach invite` / settings, over the control socket; never a network route). Redeeming the one-time code is the only invite-related thing reachable over the network.
  - A guest never uses the owner's device password, pairing codes or passkeys, and gets a session of their own (`session.guest`, at most 4 h, never past the invite). Owner and guest sessions never count for each other.
  - A guest reaches only the hub's own Mac: no proxying to other devices, no VNC (`/websockify`), no macOS account name, and can't take the stream from the owner.
  - View only is enforced on the server: `forward` lets through only `viewOnlyCmds` (stream upkeep), uploads are refused, and the Mac's clipboard isn't sent. The viewer hiding controls is cosmetic.
  - Removing an invite, or it running out, ends the guest's sessions and streams at once (`endGuest`); `watchAuth` re-checks every minute. The panel names a guest, and a notification shows when one connects.
- **Settings are only reachable over the Unix control socket,** with a peer-UID check. Never add network routes for them.
- **The one exception is `extend` / `unextend`** (handled in `stream.go`, never sent to the helper). They create or remove a virtual display, in a child process, as an extra screen only: never change which display is main or move the Mac's own displays (the one exception: the extended display is put back on the side it was last left, `vdisplay_place_darwin.go`). Keep them bounded (sizes, 30/60 Hz only, one at a time, no faster than every 2 s), and never change a virtual display's mode once open. The display always runs at 60 Hz; the 30/60 choice is the rate it's streamed at, so changing it restarts only the capture.
- **`diag` with `ask` (permission prompts) is for `tailreach doctor` only,** over the control socket. `forward` strips `ask` from anything a viewer sends.
- **One streaming device at a time.** A different device gets a `busy` event and close code 4002 and must ask again with `?take=1`; a displaced device gets 4003 (reason = who) and must not auto-reconnect. Claiming and kicking happen under one lock (`claimStream`).
- **The Mac's Stop button must stick.** The stream closes with code 4001 and new streams are refused for 2 minutes.
- **The "Screen shared" panel is shown while anyone views,** and is excluded from capture.
- **The helper always ends its capture session before exiting.** It runs in its own process group, and stops are made by closing its stdin, never by killing it outright. Otherwise macOS can leave a stuck "Unknown — Screen Recording" indicator.
- **Sound only when the viewer switches it on** (⋯ → Sound, off by default). Only then does capture include audio (never the helper's own), and it goes over WebRTC only.

## Known gaps (from review; fix before advertising multi-computer use)

1. The hub proxy forwards the browser's cookies to devices and serves device HTML under the hub's origin. It should pin device node IDs, strip cookies and serve its own viewer bundle.
2. A device's own password isn't enforced behind a hub. Either make the password hub-wide or forward and verify the viewer identity.
3. Tests cover the stream rate control (`ratectl_test.go`), the bench proxy (`tools/streambench`), the invite store and guest sessions (`invites_test.go`, `guest_test.go`); nothing tests `WhoIs`-based auth or the hub end to end yet.

## Repo hygiene (the repo must be safe to make public at any time)

- Never commit:
  - binaries (`tailreach`, `tailreach-capture`, `dist/`)
  - state (`security.json`, `trusted.json`, `passkeys.json`, `invites.json`, `*.state`, `audit.log`, logs)
  - keys and certificates
- Never commit real tailnet names (`tailXXXXXX.ts.net`), Tailscale IPs, device names or personal e-mails. Use placeholders like `<tailnet>`.
- Commits are authored by the repo owner (repo-local git config). Do not add AI/assistant attribution or co-author trailers to commits.
