---
title: Getting started
description: Install tailreach on your Mac, sign it in to your tailnet and open it from your phone.
---

tailreach runs on the Mac you want to reach. The phone, tablet or laptop you view it from needs only a browser and the Tailscale app.

## Requirements

**On the Mac being shared**

- macOS 14 or later. Apple silicon is the main target; Intel Macs work too.
- The Xcode Command Line Tools, for `swiftc` and `codesign`: `xcode-select --install`.
- [Go](https://go.dev/dl/) 1.27 or later if you build from a clone (on Apple silicon, the `darwin-arm64` one). Homebrew fetches Go for you.

The Mac doesn't need the Tailscale app: tailreach has Tailscale built in and joins your tailnet itself. It's fine if the app is installed anyway.

**Your Tailscale account**

- A [Tailscale](https://tailscale.com) account (free for personal use).
- **MagicDNS** and **HTTPS certificates** switched on in the admin console, under [DNS settings](https://login.tailscale.com/admin/dns). They give the Mac a name like `devices.<your-tailnet>.ts.net` with a real certificate, which a phone's browser needs for WebRTC, passkeys and the clipboard.

**On each device you'll view from**

- The Tailscale app, signed in to the same account.
- A modern browser: Safari on iPhone and iPad, Chrome on Android, or Chrome, Edge, Firefox or Safari on a computer.

## Install with Homebrew

```sh
brew install tanmaydoesai/tailreach/tailreach
tailreach setup
```

You still need the Command Line Tools. After every `brew upgrade tailreach`, run `tailreach setup` again: Homebrew can't use your keychain while it builds, so `setup` is what signs the new copy and keeps macOS's permissions in place.

## Install from source

```sh
git clone https://github.com/TanmayDoesAI/tailreach.git && cd tailreach
make setup
```

In a clone, the command is `./tailreach` rather than `tailreach`.

There are no prebuilt downloads: tailreach is always built on your own Mac. `make setup` and `tailreach setup` do the same thing, and both are safe to run again:

1. create a local signing identity, so macOS remembers tailreach's permissions across rebuilds;
2. build tailreach and its screen-capture helper, and sign them;
3. add a login item, so it starts on its own after every login;
4. run `tailreach doctor`.

## First run and `tailreach doctor`

`tailreach doctor` checks every step and prints what's left, with the exact fix for each. On a fresh install that's usually three things only you can do:

- **Approve the Tailscale sign-in.** The doctor opens the login link; approve it with your own account.
- **Choose a device password.** Run `tailreach password` in your terminal, or use the settings window (`tailreach settings`). It's a password for remote access, not your Mac login password; use at least 8 characters.
- **Switch on two permissions** in System Settings → Privacy & Security: **Screen Recording** and **Accessibility**, both for `tailreach`. If `tailreach` isn't in a list, `tailreach doctor --ask` puts it there.

Run `tailreach doctor` again after each fix until it says **All good** or **Working**. It exits with status 1 while something still needs fixing.

To unlock the Mac from its lock screen, also turn on **System Settings → General → Sharing → Screen Sharing**. See [Using it](/docs/using-it/#lock-screen-sleep-and-restarts).

:::tip[Let a coding agent do it]
Open the cloned folder in Claude Code, Codex or Cursor and say: *"Set up tailreach on this Mac by following SETUP.md. Stop and tell me whenever I need to do something myself."* The repo's `AGENTS.md` and `SETUP.md` tell the agent never to set your passwords or privacy settings, and never to approve the sign-in for you.
:::

## Set up your phone

1. Install the Tailscale app and sign in with the same account as the Mac. On iPhone, turning on **On Demand** stops iOS from letting it sleep.
2. Open `devices/` in Safari (or the full `https://devices.<your-tailnet>.ts.net/`). The first HTTPS request can take up to about 40 seconds while Tailscale issues the certificate.
3. Enter the device password. A device stays trusted for 30 days. You can add a passkey (Face ID, Touch ID, Windows Hello) for next time, or tap **Not now**.
4. Tap your Mac. In Safari, **Share → Add to Home Screen** gives it an app icon and full screen.

The addresses look like this:

```
https://devices.<your-tailnet>.ts.net/        dashboard: every device, online or not
https://devices.<your-tailnet>.ts.net/mac/    that Mac's screen
https://devices.<your-tailnet>.ts.net/mac2/   another one
```

If `devices` is already taken in your tailnet, choose another hostname with `--hub-name`, for example `--hub-name screens`.

## More computers

On each extra Mac, clone the repo and run:

```sh
make setup FLAGS='--name mac2'              # shown at /mac2/ on the first Mac's dashboard
make setup FLAGS='--name mac2 --via ""'     # or on its own, with its own sign-in
```

With Homebrew: `tailreach setup --name mac2` (or `tailreach setup --name mac2 --via ""`).

Several computers behind one hub are less isolated than a single Mac; see [Security](/docs/security/#more-than-one-computer).

## Windows and Linux

A Windows or Linux computer can be shared at a basic level: point `--vnc` at a VNC server you run (TightVNC, TigerVNC…), for example `--vnc 127.0.0.1:5901`. There's no fast stream or extended display there yet; a native Windows host is on the roadmap. Any of them can be the *viewing* device.

## Upgrading from `mydevices`

Earlier versions were called `mydevices`. `tailreach setup` (or `make setup`) moves such an install over, keeping your Tailscale login, password and passkeys. macOS asks for Screen Recording and Accessibility once more.

## Uninstall

```sh
tailreach uninstall
rm -rf ~/Library/Application\ Support/tailreach   # forgets the Tailscale node, passwords, passkeys
```

Then remove the `devices` machine in the Tailscale admin console, and the permission entries in System Settings.
