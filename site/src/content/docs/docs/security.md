---
title: Security
description: What a viewer can and can't do, and the layers every request passes through.
---

tailreach gives remote control of your Mac, so it's built to let in exactly one person (you) and to make every session visible on the Mac.

## No server, no open ports

tailreach has Tailscale built in (`tsnet`) and joins your tailnet as its own machine. Nothing listens on your LAN or the internet, there's no port forwarding, and there's no tailreach server or account anywhere: traffic goes between your own devices, directly when they can reach each other and through Tailscale's encrypted relays when they can't.

## What a viewer can and can't do

A signed-in viewer can:

- see the screen;
- point, click, type and scroll;
- use the clipboard both ways (secrets copied from password managers are never sent);
- add an extended display, which can only add an extra screen;
- send a file, which can only add a new file to Downloads.

A viewer can't run commands, browse, read or open files, overwrite anything, or start background processes. Settings aren't reachable over the network at all.

## The layers

Every request has to pass all of these:

1. **Tailnet only.** Nothing listens outside your tailnet.
2. **Owner only.** Every connection is identified with Tailscale `WhoIs`. Only the Tailscale user who signed the Mac in gets through (`--allow` adds others), and tagged devices are rejected. The one exception is a [guest you invited](/docs/guests/) from the Mac: only their login, only until the invite ends, only this Mac, and only after a one-time code (5 wrong codes lock it for 10 minutes).
3. **Device password**, set on the Mac. Each phone, tablet or computer enters it once, then is trusted for 30 days.
   - Trust is tied to that device's Tailscale identity, so a copied cookie doesn't work anywhere else.
   - Five wrong tries lock it for 15 minutes and notify the Mac.
   - Set or reset it in the settings window (`tailreach settings`) or from a terminal (`tailreach password`; add `--off` to turn it off).
4. **Sign-in.** Use the device password, or a passkey (Face ID, Touch ID, Windows Hello) as a shortcut.
   - The password alone signs you in, so a passkey problem never locks you out.
   - With no device password set, adding a passkey needs a 6-digit code shown on the Mac.
   - Sessions are HttpOnly, Secure, SameSite=Strict cookies. They expire after 12 hours, or after 1 hour idle.
5. **Hub pinning.** Devices behind a hub only accept connections from that one hub, whose node ID is pinned the first time it connects.
6. **Origin checks.** Only pages from your tailnet can open screen sockets, and sign-in requests must come from the hub's own origin.

## The Mac always shows who's connected

- A **Screen shared** panel sits at the top right of the Mac while anyone is connected. It shows who and for how long, and has a **Stop** button. The panel is left out of the stream.
- **Stop means stop:** it ends the session and refuses reconnects for 2 minutes.
- macOS also shows its own screen-recording indicator, and notifications cover new passkeys, trusted devices and guests connecting.
- **One viewer at a time.** Another device has to ask to take over.
- Changing the password, revoking a device or resetting passkeys ends live sessions immediately.

## Audit log

Sign-ins, sessions, new passkeys and trusted devices, invites and guest sessions are written to:

```
~/Library/Application Support/tailreach/audit.log
```

## Settings only on the Mac

The settings window and the `tailreach` commands talk to the running service over a Unix socket that only your macOS user can open. Nothing about settings is reachable over the network.

## Permissions

On first run, allow **Screen Recording** and **Accessibility** for `tailreach` in System Settings → Privacy & Security. tailreach is signed with a stable local identity on your Mac, so these permissions survive rebuilds.

:::tip
In the Tailscale admin console's access controls, you can also limit which of your devices may reach tailreach.
:::

## More than one computer

Several computers behind one hub are less isolated than a single Mac:

- the hub forwards the browser's cookies to the devices behind it;
- a device's own password isn't enforced when you reach it through the hub.

If you share more than one computer, treat everyone on your tailnet who can reach the hub as able to reach them all.

## Reporting a vulnerability

Please report security issues privately through [GitHub's private vulnerability reporting](https://github.com/TanmayDoesAI/tailreach/security/advisories/new), not as a public issue.
