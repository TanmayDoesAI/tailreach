# How the stream works

This note is for anyone changing the video path. It explains what the code in `internal/server/stream.go` and `rtc.go`, `internal/ratectl/` and the viewer's WebRTC code does, and why. The design target is a phone on 5G, a link that drops and comes back several times a minute, not a steady Wi-Fi line.

## The path

```
ScreenCaptureKit → VideoToolbox H.264 or HEVC (helper, Swift)
      → stdout frames → tailreach (Go)
      → WebRTC over the tailnet (pion) → Safari's hardware decoder → <video>
```

- **Capture** runs at the resolution the viewing screen actually shows. Nothing is sent while the screen is still. When motion stops, the last frame is re-encoded four times at 2.5× the bitrate (the link is idle then), so text sharpens up.
- **Codec:** HEVC when the viewer says it can play it both over WebRTC and with WebCodecs (it checks before connecting), otherwise H.264. It's fixed for the session; ⋯ → Video → H.264 forces H.264.
- **WebRTC** carries the video as UDP over the tailnet, with a minimal jitter buffer on the phone (`jitterBufferTarget = 0`).
- **Sound** (only when the viewer switches it on): ScreenCaptureKit's audio, encoded to Opus in 20 ms packets by AudioToolbox, on its own WebRTC track. None on the WebSocket fallback.
- **Fallback:** if WebRTC can't connect, the same H.264 goes over the WebSocket and is decoded with WebCodecs.

## Rate control

The encoder's bitrate is set four times a second by `rateCtl` (`ratectl.go`). There's no congestion-control interceptor. pion's GCC estimator is not used: on a jittery mobile path its loss figure read 5–30% on a clean link, and its estimate stayed pinned low.

**Signals**
- **Queue:** the server pings the viewer every 250 ms over the unordered `move` data channel (`sping`/`spong`). Round-trip time minus its recent floor is the standing queue on the path, the main signal. A missing pong for 1.5 s counts as a severe queue.
- **Loss:** from the phone's RTCP receiver reports, counted once per report.

**Behaviour**
- **Cut fast** on a queue (80 ms) or loss (5%): × 0.75, or × 0.25 when severe (300 ms, 15%).
- **Remember the last good rate.** When a dip clears, jump straight back to it rather than crawling up from the floor. A jump that immediately fails lowers the remembered rate.
- **Probe gently near the last limit:** × 1.2 per second, × 1.05 near where the last cut happened.
- **Floor and start:** 500 kbit/s floor, 3 Mbit/s start, up to what the viewer asks for (20 Mbit/s by default).

**Retransmits.** Without a limit, every dip ended in a retransmit storm, about 3000 drops a second. `nackLimiter` caps resends: at most two per packet, at least 80 ms apart, and within 30% of the current bitrate.

## Resolution follows the bitrate

At 1–3 Mbit/s a full-size Mac screen turns text to mush: the encoder spreads too few bits over too many pixels. Fewer, sharper pixels read better, so, like Parsec and Jump, the capture size drops when the link can only carry a low rate and comes back when it recovers. The policy is `ratectl.Scaler` (`internal/ratectl/scale.go`), run in the same 4×/s loop as the rate controller.

| Step (capture size, of what the viewer asked for) | H.264: down below / back up above | HEVC |
|---|---|---|
| 100% ↔ 75% | 4 / 6 Mbit/s | 2.8 / 4.2 |
| 75% ↔ 50% | 1.5 / 2.5 Mbit/s | 1.05 / 1.75 |

- **Only when the link is the limit.** It steps down only while the rate controller has seen congestion and not yet climbed well past it (`CutAt`). A still screen also sits at a low rate, since nothing proves the link can carry more, and that is no reason to drop pixels.
- **Slow to change, quicker down than up.** Down once the rate has been below a threshold for 2 s (most 5G dips are over sooner, and don't change the size at all). A deep dip goes straight to 50%. Back up one level at a time, after 6 s above the threshold. At least 4 s at any level, so it never flaps. Each change costs a keyframe.
- **Never above what the viewer asked for** (`w`/`h` in `start`), and never under 640 px on the long side.
- **How:** the server sends the helper `{"t":"scale","f":0.75}` (a server-only command: not in `allowedCmds`). The helper changes the size with `SCStream.updateConfiguration`, so capture keeps running. When frames arrive at the new size, the encoder starts a new VideoToolbox session at that size. Its first frame is a keyframe with the new SPS/PPS, which the WebRTC path puts in front of it, and the WebSocket path sends as a new decoder config.
- **The viewer** sizes its canvas from each frame (`<video>` metadata, direct video's decoded frames, or the WebSocket config). Layout, the local cursor and touch input all work in the picture's on-screen size and 0..1 coordinates, so a size change only changes sharpness. The status pill's tooltip says e.g. "75% res".
- **Per stream:** a new stream starts at full size; a restart within one (another display, sound on) keeps the current scale (`start` carries it). Only the WebRTC loop changes it; on the WebSocket fallback it stays where it was.

## Reconnects

A network switch (Wi-Fi ↔ 5G) breaks the WebRTC connection.

- **Re-offer:** on `disconnected` (after 1.5 s) or `failed`, the viewer offers a fresh connection, keeping the last frame on screen with a "reconnecting" note.
- **WebSocket only as a last resort:** after two failed re-offers, video goes to the WebSocket.
- **No stale video:** while WebRTC connects, the server holds WebSocket video back (`rtcFirst`), because over a relay it runs seconds behind.
- **Leaving:** a viewer that leaves says `bye`, so the Mac's "Screen shared" panel clears at once.

## How it was measured

- **Unit simulations** in `ratectl_test.go` cover dips, a step down, a path change, a blackout, and still content followed by motion. `SEEDS=1 go test -run Seeds` runs 30 random seeds with 40 ms of jitter, and there should be no slow recoveries. `scale_test.go` covers the resolution steps: dips, recoveries, HEVC, and rates swinging around a threshold (never two changes within 4 s).
- **`tools/streambench`** runs a real WebRTC session through a simulated bad network: rate caps, drops, blackouts, rate changes.
  - **Clean link, Mac plugged in:** 53.7 fps, p95 latency 11 ms, 3 → 20 Mbit/s in 11 s.
  - **Three 5G-style dips and a blackout:** every recovery was back to about 55 fps within 2–3 s. The previous GCC-based controller stayed stuck at about 1 Mbit/s after the first dip.
  - **Power state matters:** on battery, macOS caps capture at about 30 fps, so only compare runs on the same power state.
- **A real phone:** `tools/phonetest/record.py` switches on measurement mode (`vrec.go`), the viewer's own WebRTC receive stats logged on the Mac, and `analyze.py` summarises them per second. The test script:
  - about 70 s of video with one screen switch;
  - about 60 s of normal use;
  - Wi-Fi → mobile data → Wi-Fi.

**Targets**

| | fps (p50) | Time frozen | Other |
|---|---|---|---|
| Wi-Fi | ≥ 55 | under 2% | phone buffer p50 under 60 ms |
| 5G | ≥ 45 | under 5% | every dip recovered within 2 s |
| Network switch | | | live again within 3 s |

## Known costs and next steps

- **Latency spike on a sudden dip.** A dip from a high rate fills the path's buffer once before the cut lands. Latency p99 is about 1 s in the harshest bench, against about 0.3 s for a controller that never leaves 1 Mbit/s.
  - Candidate fix: drop frames before encoding when the probe reads a deep queue.
- **Resolution thresholds are absolute,** not per pixel: a viewer that asked for a small picture is treated like one that asked for a large one. They are a first guess, not yet tuned on a phone.
- **Not tried yet:** FlexFEC for lossy 5G, and a QP cap.
