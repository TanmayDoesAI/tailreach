# Setting up tailreach

A step-by-step setup that a person, or a Claude agent working for them, can follow on **their own** Mac and Tailscale account. Each step says what to run, how to check it worked, and which parts only the human can do.

> The binary and its folders are still named `mydevices`. Renaming them would make macOS
> forget the permissions already granted on existing installs, so the name stays.

## What you end up with

- `https://devices.<your-tailnet>.ts.net/`: a dashboard of your computers (or just type `devices/` in a browser on your tailnet).
- `https://devices.<your-tailnet>.ts.net/mac/`: this Mac's screen, controllable from an iPhone, iPad or another computer.
- A login item, so it starts on its own after every login.

## Rules for agents

- **Never type, choose or store a password.** The device password is set by the human with
  `./mydevices password` in their own terminal (suggest `! ./mydevices password` in Claude Code), or in the settings window.
- **Never toggle macOS privacy settings** for the human. Tell them exactly where to click and wait for them to confirm.
- **Never complete the Tailscale sign-in for them.** Open the link and let them approve it.
- Don't publish the human's tailnet name (`tailXXXXXX.ts.net`), Tailscale IPs, device names or e-mail anywhere: in commits, issues or docs.
- If a check fails twice, stop and report what you saw instead of improvising.

## 0. Requirements

| Need | Check |
|---|---|
| macOS 14+ on Apple silicon (Intel works too) | `sw_vers -productVersion`, `uname -m` |
| Xcode Command Line Tools (for `swiftc`, `codesign`) | `xcode-select -p` → if missing: `xcode-select --install` (human clicks Install) |
| Go 1.24+ | `go version` → if missing: `brew install go` |
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

Check: prints `✓ created 'mydevices local'` or `✓ ... already exists`.
If `codesign` later asks for a **keychain password**, that's the human's **Mac login password**. They type it into the macOS prompt themselves.

## 3. Build

```sh
make local
```

Check: `./mydevices version` prints a version, and `ls mydevices-capture` exists.

> Go builds with `CGO_ENABLED=0` (set in the Makefile). If `go` is an x86_64 toolchain on Apple
> silicon, keep it that way; it cross-compiles fine.

## 4. Turn on Screen Sharing (needed for unlocking the lock screen)

Human: **System Settings → General → Sharing → Screen Sharing: on**.
Check: `nc -z 127.0.0.1 5900 && echo ok`.

## 5. First run: join the tailnet

```sh
./mydevices --hub --name mac
```

- First time only, it prints a **Tailscale login link** and opens it. The human approves it with **their** Tailscale account.
- Check: the output shows `✓ Signed in as <their login>` and `✓ This Mac: https://devices.<tailnet>.ts.net/mac/`.
- The first HTTPS request can take up to about 40 s while Tailscale issues the certificate.

Stop it with Ctrl-C once it's signed in. Step 7 runs it permanently.

Names: `--name` is the path segment (`/mac/`). `--hub-name` (default `devices`) is the hostname.
If `devices` is already taken in their tailnet, use e.g. `--hub-name screens`.

## 6. Set the device password (human)

```sh
./mydevices password
```

This is a password **they choose** for this remote access, entered once every 30 days per phone or tablet. It's not the Mac login password. Use at least 8 characters.
They can also do it in the settings window: `./mydevices settings`.

## 7. Start at login

```sh
./mydevices install --hub --name mac
```

Check: `launchctl print gui/$(id -u)/com.mydevices.agent | grep state` shows `running`.
The log is at `~/Library/Application Support/mydevices/mydevices.log`.

## 8. Grant permissions (human)

Open the Mac's page once from the phone (step 9) so macOS registers the app, then go to:

- **System Settings → Privacy & Security → Screen Recording**: enable `mydevices`.
- **System Settings → Privacy & Security → Accessibility**: enable `mydevices`. In newer macOS this list may be called *Device Control and Data Access*. If it isn't listed, press **+** and add the `mydevices` binary from the repo folder.

Then restart the service: `make reload`.

If you're unsure about an entry, remove it with **−**, add it back with **+**, and run `make reload`.

## 9. Connect from the phone

1. Phone: Tailscale app connected (turning on **On Demand** stops iOS from letting it sleep).
2. Safari → `devices/` (or the full `https://devices.<tailnet>.ts.net/`).
3. Enter the device password. Optionally create a passkey for Face ID next time.
4. Tap **mac**. Share → **Add to Home Screen** for an app icon.

## 10. Verify (agent can do this)

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
./mydevices install --name mac2    # reachable only through the hub, at /mac2/
```

Multi-computer setups are less hardened than a single hub; see "Known gaps" in `CLAUDE.md`.

## Uninstall

```sh
./mydevices uninstall
rm -rf ~/Library/Application\ Support/mydevices   # forgets the Tailscale node, passwords, passkeys
```

Then remove the `devices` machine in the Tailscale admin console, and the permission entries in System Settings.
