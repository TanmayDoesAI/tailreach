---
title: How the stream works
description: Capture, codecs, WebRTC over the tailnet, rate control built for mobile links, and how it was measured.
---

The design target is a phone on 5G: a link that drops and comes back several times a minute, not a steady Wi-Fi line.

## The path

```
ScreenCaptureKit → VideoToolbox H.264 or HEVC   (Swift helper on the Mac)
      → tailreach (Go)
      → WebRTC over the tailnet (pion) → the browser's hardware decoder
```

- **One Go binary** embeds Tailscale (`tsnet`). With `--hub`, it joins your tailnet as `devices` and serves `/mac/`, and forwards `/mac2/` and others over the tailnet to the computers behind it.
- **Capture** is a small Swift helper using ScreenCaptureKit and VideoToolbox in low-latency mode, at the resolution the viewing screen actually shows. It also injects input and shows the "Screen shared" panel.
- **Nothing is sent while the screen is still.** When motion stops, the last frame is re-encoded four times at 2.5× the bitrate (the link is idle then), so text sharpens up.
- **Codec:** HEVC when the browser can play it both over WebRTC and with WebCodecs (it checks before connecting), otherwise H.264. It's fixed for the session; **⋯ → Video → H.264** forces H.264.
- **WebRTC** carries the video as UDP over the tailnet, with a minimal jitter buffer on the viewer. With *Lowest latency* on, each frame is handed over before the jitter buffer and decoded with WebCodecs.
- **Input:** taps, keys and scrolls ride WebRTC data channels. Pointer moves use a channel that never resends, so a lost packet never holds up the next move.
- **Sound**, only when the viewer switches it on: the Mac's audio, encoded to Opus in 20 ms packets, on its own WebRTC track.
- **Fallback:** if WebRTC can't connect, the same video goes over a WebSocket and is decoded with WebCodecs. There's no sound on the fallback.
- **Lock screen:** macOS hides the login screen from screen capture, so unlocking goes through the Mac's own Screen Sharing (VNC), shown with [noVNC](https://github.com/novnc/noVNC).

## Rate control

The encoder's bitrate is set four times a second by tailreach's own controller. pion's GCC estimator is not used: on a jittery mobile path its loss figure read 5–30% on a clean link, and its estimate stayed pinned low.

**Signals**

- **Queue.** The Mac pings the viewer every 250 ms over the unordered data channel. Round-trip time minus its recent floor is the standing queue on the path, the main signal. A missing reply for 1.5 s counts as a severe queue.
- **Loss**, from the viewer's RTCP receiver reports.

**Behaviour**

- **Cut fast** on a queue (80 ms) or loss (5%): × 0.75, or × 0.25 when severe (300 ms, 15%).
- **Remember the last good rate.** When a dip clears, jump straight back to it rather than crawling up from the floor. A jump that immediately fails lowers the remembered rate.
- **Probe gently near the last limit:** × 1.2 per second, × 1.05 near where the last cut happened.
- **Floor and start:** 500 kbit/s floor, 3 Mbit/s start, up to what the viewer asks for (20 Mbit/s by default).

**Retransmits.** Without a limit, every dip ended in a retransmit storm, about 3000 drops a second. tailreach caps resends: at most two per packet, at least 80 ms apart, and within 30% of the current bitrate.

## Reconnects

A network switch (Wi-Fi ↔ 5G) breaks the WebRTC connection.

- **Re-offer.** On `disconnected` (after 1.5 s) or `failed`, the viewer offers a fresh connection, keeping the last frame on screen with a "reconnecting" note.
- **WebSocket only as a last resort,** after two failed re-offers.
- **No stale video.** While WebRTC connects, the Mac holds WebSocket video back, because over a relay it runs seconds behind.
- **Leaving.** A viewer that leaves says so, and the Mac's "Screen shared" panel clears at once.

## How it was measured

- **Unit simulations** cover dips, a step down, a path change, a blackout, and still content followed by motion, plus 30 random seeds with 40 ms of jitter.
- **A benchmark** runs a real WebRTC session through a simulated bad network (rate caps, drops, blackouts, rate changes):
  - Clean link, Mac plugged in: 53.7 fps, p95 latency 11 ms, 3 → 20 Mbit/s in 11 s.
  - Three 5G-style dips and a blackout: every recovery was back to about 55 fps within 2–3 s. The previous GCC-based controller stayed stuck at about 1 Mbit/s after the first dip.
  - On battery, macOS caps capture at about 30 fps, so only runs on the same power state are compared.
- **A real phone:** a measurement mode logs the viewer's own WebRTC receive stats on the Mac, through about 70 s of video with a screen switch, about 60 s of normal use, and Wi-Fi → mobile data → Wi-Fi.

**Targets**

| | fps (p50) | Time frozen | Other |
|---|---|---|---|
| Wi-Fi | ≥ 55 | under 2% | phone buffer p50 under 60 ms |
| 5G | ≥ 45 | under 5% | every dip recovered within 2 s |
| Network switch | | | live again within 3 s |

## Known costs and next steps

- **Latency spike on a sudden dip.** A dip from a high rate fills the path's buffer once before the cut lands: latency p99 is about 1 s in the harshest bench, against about 0.3 s for a controller that never leaves 1 Mbit/s. A candidate fix is dropping frames before encoding when the probe reads a deep queue.
- **Resolution doesn't follow the bitrate yet.** At 1 Mbit/s a phone doesn't need full resolution; scaling down would keep text readable in deep dips.
- **Not tried yet:** FlexFEC for lossy 5G, and a QP cap.

The engineering version of this page, with file names, is [`docs/streaming.md`](https://github.com/TanmayDoesAI/tailreach/blob/main/docs/streaming.md) in the repo.
