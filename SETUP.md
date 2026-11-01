# Setting up tailreach

A step-by-step setup that a person, or a coding agent working for them, can follow on **their own** Mac and Tailscale account. Each step says what to run, how to check it worked, and which parts only the human can do.

> Earlier versions were called `mydevices`. `make setup` moves such an install over, keeping its
> Tailscale login, password and passkeys; macOS asks for its two permissions once more.

## What you end up with

- `https://devices.<your-tailnet>.ts.net/`: a dashboard of your computers (or just type `devices/` in a browser on your tailnet).
- `https://devices.<your-tailnet>.ts.net/mac/`: this Mac's screen, controllable from an iPhone, iPad or another computer.
- A login item, so it starts on its own after every login.

## Quick path

```sh
git clone https://github.com/TanmayDoesAI/tailreach.git && cd tailreach
make setup          # signing identity, build, login item, then a check of everything
```

With Homebrew instead of a clone: `brew install tanmaydoesai/tailreach/tailreach`, then
`tailreach setup` (it does what `make setup` does, for
the installed copy; run it again after every upgrade). Use `tailreach` wherever this guide
says `./tailreach`.

`make setup` does every step that doesn't need you, and is safe to run again. It ends with
`./tailreach doctor`, which lists what's left with the exact fix for each:

- approving the Tailscale sign-in (it opens the link);
- choosing a device password;
- switching on two permissions.

Run `./tailreach doctor` again after each fix, until it says **All good** or **Working**. It
exits with status 1 while something still needs fixing.

The numbered steps below are the same thing done by hand, with the reasoning.

## Rules for agents

- **Never type, choose or store a password.** The device password is set by the human with
  `./tailreach password` in their own terminal (suggest `! ./tailreach password` in Claude Code), or in the settings window.
- **Never toggle macOS privacy settings** for the human. Tell them exactly where to click and wait for them to confirm.
- **Never complete the Tailscale sign-in for them.** Open the link and let them approve it.
- Don't publish the human's tailnet name (`tailXXXXXX.ts.net`), Tailscale IPs, device names or e-mail anywhere: in commits, issues or docs.
- If a check fails twice, stop and report what you saw instead of improvising.
- `./tailreach doctor` is the source of truth for what's done. Run it first, and after every step.

## 0. Requirements

| Need | Check |
|---|---|
| macOS 14+ on Apple silicon (Intel works too) | `sw_vers -productVersion`, `uname -m` |
| Xcode Command Line Tools (for `swiftc`, `codesign`) | `xcode-select -p` → if missing: `xcode-select --install` (human clicks Install) |
| Go 1.27+ | `go version` → if missing: `brew install go` |
| A Tailscale account, with the Tailscale app on the **phone/tablet** you'll use | human confirms |

You don't need the Tailscale app on the Mac being shared: the binary embeds its own Tailscale node. It's fine if the app is there anyway.

## 1. Get the code

```sh
git clone https://github.com/TanmayDoesAI/tailreach.git
cd tailreach
```

## 2. Create a stable signing identity (once per Mac)

macOS ties Screen Recording and Accessibility permissions to the app's code signature. A stable local identity means you grant them once, not after every rebuild.

```sh
sh scripts/signing-identity.sh
```

Check: prints `✓ created 'tailreach local'` or `✓ ... already exists`.
If `codesign` later asks for a **keychain password**, that's the human's **Mac login password**. They type it into the macOS prompt themselves.

## 3. Build

```sh
make local
```

Check: `./tailreach version` prints a version, and `ls tailreach-capture` exists.

> Go builds with `CGO_ENABLED=0` (set in the Makefile). On Apple silicon, use the Apple silicon
> Go (`go env GOARCH` says `arm64`). An Intel Go builds an Intel binary, which works but runs
> through Rosetta and costs more CPU while streaming; `./tailreach doctor` warns about it.

## 4. Turn on Screen Sharing (needed for unlocking the lock screen)

Human: **System Settings → General → Sharing → Screen Sharing: on**.
Check: `nc -z 127.0.0.1 5900 && echo ok`.

## 5. First run: join the tailnet

```sh
./tailreach --hub --name mac
```

- First time only, it prints a **Tailscale login link** and opens it. The human approves it with **their** Tailscale account.
- Check: the output shows `✓ Signed in as <their login>` and `✓ This Mac: https://devices.<tailnet>.ts.net/mac/`.
- The first HTTPS request can take up to about 40 s while Tailscale issues the certificate.

Stop it with Ctrl-C once it's signed in. Step 7 runs it permanently.

Names: `--name` is the path segment (`/mac/`). `--hub-name` (default `devices`) is the hostname.
If `devices` is already taken in their tailnet, use e.g. `--hub-name screens`.

## 6. Set the device password (human)

```sh
./tailreach password
```

This is a password **they choose** for this remote access, entered once every 30 days per phone or tablet. It's not the Mac login password. Use at least 8 characters.
They can also do it in the settings window: `./tailreach settings`.

## 7. Start at login

```sh
./tailreach install --hub --name mac
```

Check: `launchctl print gui/$(id -u)/com.tailreach.agent | grep state` shows `running`.
The log is at `~/Library/Application Support/tailreach/tailreach.log`.

## 8. Grant permissions (human)

Open the Mac's page once from the phone (step 9) so macOS registers the app, then go to:

- **System Settings → Privacy & Security → Screen Recording**: enable `tailreach`.
- **System Settings → Privacy & Security → Accessibility**: enable `tailreach`. In newer macOS this list may be called *Device Control and Data Access*. If it isn't listed, press **+** and add the `tailreach` binary from the repo folder.

If `tailreach` isn't in a list, `./tailreach doctor --ask` puts it there (macOS may show a prompt). Then switch it on.

Then restart the service: `make reload`.

If you're unsure about an entry, remove it with **−**, add it back with **+**, and run `make reload`.

## 9. Connect from the phone

1. Phone: Tailscale app connected (turning on **On Demand** stops iOS from letting it sleep).
2. Safari → `devices/` (or the full `https://devices.<tailnet>.ts.net/`).
3. Enter the device password. Optionally create a passkey (Face ID, Touch ID, Windows Hello) for next time, or tap **Not now**.
4. Tap **mac**. Share → **Add to Home Screen** for an app icon.

## 10. Verify (agent can do this)

```sh
./tailreach doctor
```

Every line should be ✓. Then check a real stream end to end:

```sh
go run ./tools/streamtest -rtc -t 6 https://devices.<tailnet>.ts.net/mac/
```

This mints a short test session over the local control socket. Expect `webrtc: N RTP packets, first after ~300ms` and stats lines with `"rtc":true` and `"path":"direct"`.

| Symptom | Fix |
|---|---|
| `needsPermission` / black screen | Screen Recording not granted (step 8), then `make reload` |
| Video works, taps do nothing; viewer warns "Mac is blocking input" | Accessibility (step 8) |
| Safari: "can't establish a secure connection" / loads forever | Phone's Tailscale is asleep or disconnected: toggle it off and on |
| Passkey step does nothing | Use **Use password instead** |
| Menu bar keeps showing "Unknown — Screen Recording" after viewing ended | A capture session left behind by an older build that force-killed the helper. `killall replayd` (it restarts on its own), then `killall ControlCenter` |
| `path` is `relay …` | Works, just slower; check both devices' networks for UDP blocking |

## More computers (optional)

On another Mac, do steps 1–4 and 8, then:

```sh
./tailreach install --name mac2    # reachable only through the hub, at /mac2/
```

Multi-computer setups are less hardened than a single hub; see "Known gaps" in `AGENTS.md`.

## Uninstall

```sh
./tailreach uninstall
rm -rf ~/Library/Application\ Support/tailreach   # forgets the Tailscale node, passwords, passkeys
```

Then remove the `devices` machine in the Tailscale admin console, and the permission entries in System Settings.

## Extended display (optional)

Needs no extra permission beyond Screen Recording: the virtual display is created by the login item itself. See "Extended display" in the README.
