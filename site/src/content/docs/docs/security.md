---
title: Security and guests
description: Who can get in, what a viewer can do, and how to invite a guest.
---

## Who can get in

- **Only your tailnet.** tailreach joins your Tailscale network itself. Nothing listens on your LAN or the internet, and there's no tailreach server or account.
- **Only you.** Every connection is identified by Tailscale. Only the account that signed the Mac in gets through, plus guests you invite.
- **Then the device password** (or a passkey). Each device enters it once and is trusted for 30 days. Five wrong tries lock it for 15 minutes and notify the Mac.
- **More Macs** behind one hub use the hub's password. Each one accepts only its own pinned hub, and the hub never passes on your browser's cookies.

## What a viewer can do

See the screen, click and type, use the clipboard, add an extended display, and drop new files into Downloads. That's all: no commands, no reading files, no settings. Settings live only on the Mac (`tailreach settings`, or the menu bar).

## You always know

While anyone is connected, a **Screen shared** panel on the Mac shows who, with a **Stop** button (which also blocks reconnects for 2 minutes). Sign-ins and sessions go to `~/Library/Application Support/tailreach/audit.log`.

## Guests

Let someone see or use your Mac for a while. They need their own free Tailscale account and never get your password.

```sh
tailreach invite their-login@example.com                      # 24 hours, can control it
tailreach invite their-login@example.com --hours 2 --view-only
```

Then share the `devices` machine with them in the [Tailscale admin console](https://login.tailscale.com/admin/machines) (**⋯ → Share**). They open the link and enter the one-time code the command printed.

- They reach only this Mac, can't unlock it, and can't take it from you.
- **View only** is enforced by the Mac, not their browser.
- The Mac notifies you when they connect. `tailreach invite --list` and `--remove` manage invites; removing one ends their session at once.

## Report a vulnerability

Privately, through [GitHub's vulnerability reporting](https://github.com/TanmayDoesAI/tailreach/security/advisories/new), please, not a public issue.
