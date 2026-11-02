---
title: Troubleshooting
description: Start with tailreach doctor; fixes for the common problems.
---

Run `tailreach doctor` first. It checks every step and prints the exact fix. The log is at `~/Library/Application Support/tailreach/tailreach.log`.

| Problem | Fix |
|---|---|
| The link doesn't open | That device isn't on your tailnet: sign in to the Tailscale app with the same account. |
| Safari can't connect, or loads forever | Tailscale on the phone is asleep: toggle it off and on. **On Demand** keeps it awake. |
| First visit is slow | Normal: up to ~40 s while the certificate is issued. |
| Black screen | Allow **Screen Recording** for `tailreach`, then run `tailreach setup`. |
| Taps do nothing ("Mac is blocking input") | Allow **Accessibility** for `tailreach`. |
| `tailreach` isn't in a permission list | `tailreach doctor --ask` adds it. |
| Can't unlock from the lock screen | Turn on **Screen Sharing** in System Settings → General → Sharing. |
| Offline when the Mac sleeps | Turn on **Keep Mac Reachable** in the menu-bar icon. |
| Stopped working after `brew upgrade` | Run `tailreach setup` again (`tailreach update` does both). |

Still stuck? Open the repo in a coding agent and say *"Fix my tailreach setup by following SETUP.md"*, or [open an issue](https://github.com/TanmayDoesAI/tailreach/issues/new/choose) with what `tailreach doctor` printed (take out your tailnet name and e-mail first).
