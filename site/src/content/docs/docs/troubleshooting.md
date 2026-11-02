---
title: Troubleshooting
description: Start with tailreach doctor; fixes for the common problems.
---

## Start with `tailreach doctor`

```sh
tailreach doctor      # ./tailreach doctor in a clone
```

It checks every step and prints the exact fix for whatever isn't right: the Tailscale sign-in, a phone that isn't on your tailnet yet, macOS permissions, and the error from the log if tailreach stopped. Run it again after each fix. It exits with status 1 while something still needs fixing.

The log is at `~/Library/Application Support/tailreach/tailreach.log`.

## Common problems

| Symptom | Fix |
|---|---|
| The link doesn't open on your phone | The phone isn't on your tailnet. Open the Tailscale app and sign in with the same account as the Mac. |
| Safari: "can't establish a secure connection", or it loads forever | The phone's Tailscale is asleep or disconnected: toggle it off and on. On iPhone, **On Demand** keeps it awake. |
| The very first visit is slow | The first HTTPS request can take up to about 40 seconds while Tailscale issues the certificate. |
| Black screen, or the viewer says it needs permission | Screen Recording isn't granted. Switch it on for `tailreach`, then run `tailreach setup` (or `make reload` in a clone). |
| Video works, taps do nothing; the viewer warns "Mac is blocking input" | Accessibility isn't granted for `tailreach`. |
| `tailreach` isn't in a permission list | `tailreach doctor --ask` adds it (macOS may show a prompt). Or press **+** and add the binary. If an entry seems stuck, remove it with **−** and add it back. |
| The passkey step does nothing | Use **Use password instead**. The password always works on its own. |
| "Mac is locked" and unlocking fails | Turn on **System Settings → General → Sharing → Screen Sharing**. |
| The Mac can't be reached after a restart | With FileVault on, someone has to type the password at the disk-unlock screen once. `sudo fdesetup authrestart` skips it for planned restarts. |
| The Mac goes offline when it sleeps | Turn on *Keep this Mac reachable while plugged in* in settings. |
| The menu bar keeps showing "Unknown — Screen Recording" after viewing ended | A capture session left behind by an older build. `killall replayd` (it restarts on its own), then `killall ControlCenter`. |
| `codesign` asks for a keychain password | That's your Mac login password, for the local signing identity. |
| It worked, then stopped after `brew upgrade` | Run `tailreach setup` again after every upgrade. |

## Let a coding agent finish it with you

Clone the repo, open it in Claude Code, Codex or Cursor, and say:

> Set up tailreach on this Mac by following SETUP.md. Stop and tell me whenever I need to do something myself.

`AGENTS.md` and `SETUP.md` tell the agent what it may and may not do: it never types or stores your passwords, never switches privacy settings for you, and never approves the Tailscale sign-in on your behalf.

## Reporting a bug

[Open an issue](https://github.com/TanmayDoesAI/tailreach/issues/new/choose) with your macOS version, the device and browser you view from, how you installed, and what `tailreach doctor` printed. The doctor never prints your password, but **take out your tailnet name and e-mail** before posting.
