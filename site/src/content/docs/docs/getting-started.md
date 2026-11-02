---
title: Getting started
description: Install tailreach on your Mac and open it from any browser.
---

tailreach runs on the Mac you want to reach. The device you view from (phone, tablet, an old laptop) only needs a browser and the Tailscale app.

## You need

- **The Mac:** macOS 14 or later, and the Xcode Command Line Tools (`xcode-select --install`). It doesn't need the Tailscale app; tailreach has Tailscale built in.
- **A [Tailscale](https://tailscale.com) account** (free for personal use) with **MagicDNS** and **HTTPS certificates** on, under [DNS settings](https://login.tailscale.com/admin/dns).
- **The viewing device:** the Tailscale app, signed in to the same account, and a current browser.

## Install

```sh
brew install tanmaydoesai/tailreach/tailreach
tailreach setup
```

Or from source, with [Go](https://go.dev/dl/) installed: `git clone https://github.com/TanmayDoesAI/tailreach.git && cd tailreach && make setup` (the command is then `./tailreach`).

`setup` signs tailreach, adds a login item, walks you through the two macOS permissions (it opens the right Settings pane and waits for you), then runs `tailreach doctor`. Run it again after every `brew upgrade`.

## Finish with `tailreach doctor`

The doctor checks everything and prints the fix for whatever's left. Usually that's three things only you can do:

1. **Approve the Tailscale sign-in** it opens.
2. **Choose a device password** with `tailreach password` (not your Mac password).
3. **Allow Screen Recording and Accessibility** for `tailreach` in System Settings → Privacy & Security.

Run it again until it says **All good**. To unlock the Mac from its lock screen too, turn on **System Settings → General → Sharing → Screen Sharing**.

:::tip[Let a coding agent do it]
Open the cloned repo in Claude Code, Codex or Cursor and say: *"Set up tailreach on this Mac by following SETUP.md."* It stops whenever you need to do something yourself, and never sets passwords or privacy settings for you.
:::

## Open it

On your phone, tablet or laptop, open `https://devices.<your-tailnet>.ts.net/` (the first visit can take ~40 s while the certificate is issued). Enter the device password once; that device is then trusted for 30 days, and you can add a passkey. On iPhone, **Share → Add to Home Screen** makes it an app.

## More Macs

On each extra Mac: `tailreach setup --name mac2`. It shows up on the first Mac's dashboard at `/mac2/`, behind the same password.

## Uninstall

```sh
tailreach uninstall
rm -rf ~/Library/Application\ Support/tailreach
```

Then remove the `devices` machine in the Tailscale admin console.
