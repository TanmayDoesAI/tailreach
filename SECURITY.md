# Security policy

tailreach gives remote control of a Mac, so security reports are taken seriously.

## Reporting a vulnerability

Please **don't open a public issue.** Report it privately through GitHub's
[private vulnerability reporting](https://github.com/TanmayDoesAI/tailreach/security/advisories/new)
(the repo's **Security** tab → **Report a vulnerability**).

Include what you found, how to reproduce it, and what an attacker could do with it. Leave out
your tailnet name, Tailscale IPs and e-mail. You'll get a reply as soon as possible, and credit in
the fix if you'd like it.

## Supported versions

Fixes go into the latest release. Upgrade with `brew upgrade tailreach && tailreach setup`, or
`git pull && make setup` in a clone.

## Scope

The security model (what a viewer can and can't do, and the layers every request passes) is
described in the [README](README.md#security) and on the
[website](https://tailreach.tanmaylabs.me/docs/security/), and the rules the code keeps are the
invariants in [AGENTS.md](AGENTS.md#invariants-dont-break-these).
