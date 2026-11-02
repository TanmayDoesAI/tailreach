---
title: FAQ
description: Short answers to common questions about tailreach.
---

### Is it free?

Yes. tailreach is open source under the Apache License 2.0. Tailscale is free for personal use.

### Do I need to install an app on my phone?

Only the Tailscale app, so the phone is on your tailnet. tailreach itself runs in the browser: Safari on iPhone and iPad, Chrome on Android, or any modern desktop browser. **Add to Home Screen** gives it an icon and full screen.

### Does it need a server, an account or port forwarding?

No. tailreach has Tailscale built in and joins your tailnet itself. There's no tailreach server or account, nothing listens on the internet, and you don't open any ports. The only account involved is your own Tailscale one.

### Does my screen go through someone else's server?

Devices connect directly when they can. When they can't, Tailscale carries the traffic through its encrypted relays. Either way it stays inside your tailnet.

### Why Tailscale?

It puts your devices on one private network wherever they are, gives every connection a verified identity (so tailreach can let in only you before asking for a password), and issues real HTTPS certificates, which a phone's browser needs for WebRTC, passkeys and the clipboard. tailreach is an independent project, not affiliated with Tailscale Inc.

### Which Macs does it run on?

macOS 14 or later, on Apple silicon or Intel. You build it on the Mac itself; Homebrew or `make setup` does that for you.

### Can I share a Windows or Linux computer?

At a basic level: point `--vnc` at a VNC server you run. The fast stream and the extended display are Mac-only for now; a native Windows host is on the roadmap. Any computer can be the viewing device or the second screen.

### Can I unlock the Mac from the lock screen?

Yes, if Screen Sharing is on in the Mac's settings. Your phone shows "Mac is locked"; type the Mac password and it's entered on the real lock screen. After a restart with FileVault on, someone has to type the password at the disk-unlock screen once.

### Can I use an iPad or an old laptop as a second monitor?

Yes. Open the viewer on it and start the extended display. The Mac gets a virtual monitor the size of that screen. It uses a private macOS API, so a macOS update could break it.

### Can two people watch at once?

One device streams at a time. Another device is asked whether to take over. A guest can't take the Mac away from you.

### Can I let a friend in?

Yes, with an expiring invite created on the Mac, optionally view-only. They need their own free Tailscale account. See [Guests](/docs/guests/).

### Does it send sound?

Only when you switch on **⋯ → Sound** in the viewer. It's one-way (no microphone) and only over WebRTC.

### Can a viewer read my files or run commands?

No. A viewer can see the screen, send input and use the clipboard. It can add a new file to Downloads and add an extended display, nothing else. See [Security](/docs/security/).

### Who made it?

[Tanmay](https://github.com/TanmayDoesAI). Issues and pull requests are welcome on [GitHub](https://github.com/TanmayDoesAI/tailreach).
