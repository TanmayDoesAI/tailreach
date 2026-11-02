# tailreach: notes for coding agents (and people) working in this repo

This file is read by Codex, Cursor, Gemini CLI and most other coding agents. Claude Code reads it
through `CLAUDE.md`.

Remote control of a Mac from an iPhone/iPad browser, over the owner's own Tailscale tailnet.
Go server with embedded Tailscale (`tsnet`), a Swift capture/input helper (`macos/`), and a plain HTML/JS viewer (`web/`).

- **Setting it up for someone:**
  - Run `make setup`, then work through what `./tailreach doctor` reports until it exits 0.
  - Follow the "Rules for agents" in `SETUP.md`: never set passwords or privacy settings, and never approve the sign-in for them.
  - Claude Code also has a `tailreach-setup` agent in `.claude/agents/`.
- **User-facing docs:** `README.md`.

## Layout

| Path | What |
|---|---|
| `cmd/tailreach/` | the command: `main.go` (subcommands, then `server.Serve`), `setup.go` (signs an installed copy, login item; a Homebrew copy is first copied to `~/Library/Application Support/tailreach/bin`, one path for every version, since macOS files a command-line program's permissions by path; old login items keep their flags except `--web-dir`/`-v`; the Homebrew formula lives in [TanmayDoesAI/homebrew-tailreach](https://github.com/TanmayDoesAI/homebrew-tailreach): `.github/workflows/release.yml` updates its `url` and `sha256` when a release is published, after building and testing it), `install.go` (LaunchAgent), `doctor.go` (checks everything, prints each fix), `commands.go` (`settings`, `password`, `invite`, over the control socket) |
| `internal/server/serve.go` | flags, tsnet startup, `Serve` |
| `internal/server/server.go` | HTTP routes, Tailscale `WhoIs` owner check, hub ↔ device proxy (own viewer, dynamic endpoints only, no cookies, pinned nodes), discovery |
| `internal/server/update.go` | once-a-day check for a newer release on GitHub (the only non-Tailscale request; `--no-update-check` / `updateCheck` op) |
| `internal/server/auth.go` | passkeys (WebAuthn), sessions, pairing codes, guest invite-code sign-in, audit log |
| `internal/server/invites.go` | guests: expiring invites for other Tailscale logins, one-time codes (hashed, single use, rate limited), `invites.json` |
| `internal/server/security.go` | device password (Argon2id), 30-day device trust, live sessions, local control socket (up before Tailscale signs in; `ctlApp` once ready), Pause sharing |
| `internal/server/awake.go` | keep the Mac reachable: a no-sleep assertion (`caffeinate`) on power, optionally on battery above 20%, following the power source |
| `internal/server/menubar.go` | starts the menu-bar item, restarts it with backoff, stops it by closing its stdin; one at a time |
| `internal/server/stream.go` | viewer WebSocket: control, input dispatch (`forward`), WS video/input fallback, bitrate steering |
| `internal/server/rtc.go` | WebRTC (pion): video with true capture timestamps, NACK + RTCP reports only; queue probe (`sping`/`spong`); `input` (reliable) and `move` (unordered, no resends) data channels |
| `internal/ratectl/` | the rate controller (cut fast, jump back to the last good rate, probe gently), the capture scale that follows it (`scale.go`) and the cap on retransmits; see `docs/streaming.md` |
| `internal/server/logrotate*.go` | keeps `tailreach.log` (5 MB) and `audit.log` (1 MB) from growing forever: one older copy each |
| `internal/server/debug*.go` | `dlog` and the `debug` build tag (`make DEBUG=1`) |
| `internal/server/vrec.go` | measurement mode: the viewer's own receive stats, logged on the Mac (switched on from the control socket) |
| `internal/server/vdisplay*.go` | extended display: a virtual second monitor (private CGVirtualDisplay via `go-macos/virtualdisplay`), child process `tailreach vdisplay` owns it, idle auto-close; never the main display |
| `internal/server/files.go` | files sent from the viewer, saved as new files in `~/Downloads` (write-only) |
| `internal/server/vnchold.go` | keeps the phone's unlock connection to Screen Sharing open (idle) while anyone views, since macOS re-locks when it ends |
| `internal/server/helper.go` | one warm `tailreach-capture` process shared across reconnects |
| `internal/server/migrate.go` | moves an install from before the rename (`mydevices`) over |
| `macos/*.swift` | `tailreach-capture`: ScreenCaptureKit + VideoToolbox H.264/HEVC, Opus sound, CGEvent input, "Screen shared" panel, settings window (`--settings`), menu-bar item (`--menubar`, `menubar.swift`) |
| `web/viewer/` | the viewer (`app.js`; `rtcworker.js` is direct video); `classic/` + `novnc/` is the VNC fallback for the lock screen |
| `web/login.html`, `web/dashboard/` | sign-in and device list (`web/embed.go` builds them into the binary) |
| `web/sw.js`, `web/offline.html` | service worker: shows the offline page when a page load can't reach the Mac at all; caches nothing else |
| `scripts/` | `signing-identity.sh` (the local code-signing identity; embedded for `tailreach setup`) |
| `tools/streamtest` | end-to-end check over the tailnet (gets a test session from the control socket) |
| `tools/streambench` | WebRTC benchmark through a simulated bad network (drops, blackouts, rate changes) |
| `tools/phonetest` | record and summarise a real phone session (with `vrec.go`) |
| `tools/pattern` | animated test-pattern window, for benchmarking and latency (its clock strip can be read from each frame) |

## Build, run, check

```sh
make local     # build + sign both binaries (needs the "tailreach local" identity: scripts/signing-identity.sh)
make reload    # build, sign, restart the login item
make setup     # first-time setup on a Mac (safe to re-run); ends with ./tailreach doctor
./tailreach doctor   # what's working, what isn't, and the fix (exit 1 = something to fix)
go run ./tools/streamtest -rtc -t 6 https://<hub>.<tailnet>.ts.net/<name>/
CGO_ENABLED=0 GOARCH=arm64 go vet ./... && CGO_ENABLED=0 GOARCH=arm64 go test ./...
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
  - The menu-bar item (`tailreach-capture --menubar`) talks only to that socket too; it has no network side. Quitting or hiding it never stops the server.
- **Pause sharing is enforced on the server** (`sharingPaused`, saved as `paused` in `security.json`): `streamsBlocked` refuses every stream, VNC and upload until it's resumed, and open ones close with 4001 ("paused on the Mac").
- **The service worker only ever serves the offline page** (and its icon), and only when a page load fails. Never cache signed-in pages or API replies, and keep page loads going to the network first (navigation preload). Bump `VERSION` in `sw.js` when the offline page changes.
- **The one exception is `extend` / `unextend`** (handled in `stream.go`, never sent to the helper). They create or remove a virtual display, in a child process, as an extra screen only: never change which display is main or move the Mac's own displays (the one exception: the extended display is put back on the side it was last left, `vdisplay_place_darwin.go`). Keep them bounded (sizes, 30/60 Hz only, one at a time, no faster than every 2 s), and never change a virtual display's mode once open. The display always runs at 60 Hz; the 30/60 choice is the rate it's streamed at, so changing it restarts only the capture. Retina (viewer `dpr >= 1.5`, `extSizeFor`) is asked for with HiDPI and a Retina-like physical size, never switched into; a different size or Retina-ness is a new display.
- **`diag` with `ask` (permission prompts) is for `tailreach doctor` only,** over the control socket. `forward` strips `ask` from anything a viewer sends.
- **One streaming device at a time.** A different device gets a `busy` event and close code 4002 and must ask again with `?take=1`; a displaced device gets 4003 (reason = who) and must not auto-reconnect. Claiming and kicking happen under one lock (`claimStream`).
- **The Mac's Stop button must stick.** The stream closes with code 4001 and new streams are refused for 2 minutes. Stop Current Session in the menu bar does the same.
- **The "Screen shared" panel is shown while anyone views,** and is excluded from capture.
- **The helper always ends its capture session before exiting.** It runs in its own process group, and stops are made by closing its stdin, never by killing it outright. Otherwise macOS can leave a stuck "Unknown — Screen Recording" indicator.
- **Sound only when the viewer switches it on** (⋯ → Sound, off by default). Only then does capture include audio (never the helper's own), and it goes over WebRTC only.

- **Behind a hub, the hub signs everyone in** (one hub-wide password and passkeys). A device behind it trusts only its pinned hub (`fromHub`) and has no password of its own; `setPassword` / `enablePassword` are refused there.
- **The hub never hands a device the browser's credentials or its origin.** For `/<name>/` it serves its own viewer files and forwards only `deviceEndpoints` (`stream`, `websockify`, `caps`, `upload`), to the node pinned for that name (`pinNode`, re-checked with `WhoIs` per request), with `Cookie`/`Authorization` stripped and `Set-Cookie` dropped (`stripProxyRequest` / `stripProxyResponse`). Keep it that way when adding endpoints.
- **The update check sends nothing identifying** (only `User-Agent: tailreach/<version>`), and is the only request outside Tailscale. Keep the opt-out working.

## Known gaps

1. Tests cover the stream rate control (`ratectl_test.go`), the bench proxy (`tools/streambench`), the invite store and guest sessions (`invites_test.go`, `guest_test.go`), the hub proxy's routing and header stripping (`hubproxy_test.go`) and the update check (`update_test.go`); nothing tests `WhoIs`-based auth or the hub end to end yet.

## Repo hygiene (the repo must be safe to make public at any time)

- Never commit:
  - binaries (`tailreach`, `tailreach-capture`, `dist/`)
  - state (`security.json`, `trusted.json`, `passkeys.json`, `invites.json`, `*.state`, `audit.log`, logs)
  - keys and certificates
- Never commit real tailnet names (`tailXXXXXX.ts.net`), Tailscale IPs, device names or personal e-mails. Use placeholders like `<tailnet>`.
- Commits are authored by the repo owner (repo-local git config). Do not add AI/assistant attribution or co-author trailers to commits.
