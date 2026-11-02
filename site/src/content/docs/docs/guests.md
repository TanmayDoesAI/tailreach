---
title: Guests
description: Invite a friend, colleague or family member to see or use your Mac for a while.
---

Let someone else see, or use, your Mac for a while. They need a free Tailscale account of their own, and they never get your password.

## Invite someone

1. **Invite them, on the Mac:**

   ```sh
   tailreach invite their-login@example.com                  # 24 hours, they can control it
   tailreach invite their-login@example.com --hours 2 --view-only
   ```

   Or use **Guests** in the settings window (`tailreach settings`). Use the login they sign in to Tailscale with: an e-mail address, or `name@github`.

2. **Share the Mac with them in Tailscale.** At [login.tailscale.com/admin/machines](https://login.tailscale.com/admin/machines), open the `devices` machine's **⋯** menu → **Share…**, and send them the link. Sharing a machine works across Tailscale accounts and is free. The invite command prints this step too.

3. **They open the address** the command printed (`https://devices.<tailnet>.ts.net/mac/`) and enter the **invite code** it printed. The code works once and ends with the invite. Run `tailreach invite` again for a new one.

## What a guest gets

- **This Mac only.** They don't see your other devices, can't unlock the Mac (that needs your password), and can't take it away from you while you're using it.
- **View only, if you choose.** With `--view-only` they see the screen and nothing else: no pointer, keyboard, clipboard, files or extended display. The Mac enforces this, not just their browser, and their viewer shows a **View only** pill.
- **A short session.** Signing in lasts at most 4 hours, and never past the invite. After that they need a new code.
- **No guessing.** Five wrong codes lock invite sign-in for 10 minutes, and the Mac is notified.

## You always know

- The **Screen shared** panel on the Mac names them and says **Guest** (and **view only**).
- The Mac shows a notification when a guest connects.
- Invites, sign-ins and sessions go in the audit log.

## Ending an invite

Invites end by themselves: after 24 hours unless you pass `--hours`, and at most 30 days.

- `tailreach invite --list` shows who's invited.
- `tailreach invite --remove their-login@example.com`, or **Remove** in the settings window, ends one now. Anything they have open closes immediately.
- You can also unshare the machine in the Tailscale admin console.
