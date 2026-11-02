---
title: FAQ
description: Short answers to common questions.
---

### Is it free?

Yes, and open source (Apache-2.0). Tailscale is free for personal use.

### What do I install on the device I view from?

Just the Tailscale app. tailreach itself is a web page, so a ten-year-old laptop, a Chromebook or a cheap tablet is enough: the Mac does the encoding, the browser only plays video.

### How is it different from Parsec, Chrome Remote Desktop or RustDesk?

Parsec and RustDesk need their own app on every device. Chrome Remote Desktop runs in a desktop browser, but through Google's account and servers, and needs an app on phones. tailreach needs only a browser and your own Tailscale, has no server of its own, and can also turn the other device into a second monitor.

### Does my screen go through anyone's server?

No tailreach server exists. Devices connect directly when they can, otherwise through Tailscale's encrypted relays.

### Which Macs?

macOS 14 or later, Apple silicon or Intel. Windows and Linux can be *viewers*; sharing them is only basic for now (`--vnc` to a VNC server you run).

### Can two devices watch at once?

One streams at a time; another is asked whether to take over.

### How fast is it?

60 fps with hardware encoding, about 11 ms added on the Mac. The stream adapts to bad mobile links. Details: [`docs/streaming.md`](https://github.com/TanmayDoesAI/tailreach/blob/main/docs/streaming.md).

### Who made it?

[Tanmay](https://github.com/TanmayDoesAI). Issues and pull requests welcome.
