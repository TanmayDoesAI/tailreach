# Plan: a stream that stays smooth (branch `smooth-stream`, built on `master`)

Every step is judged by the real-phone test below, not by `tools/streambench` alone.

## Where we are (2026-10-25)

We measured two builds on a real iPhone while the Mac played a YouTube video. Logs are kept outside the repo in `~/Library/Application Support/mydevices/`.

| | `master` | `robust-stream` |
|---|---|---|
| Video: fps (p50) | 57 (cellular) | 13 (Wi-Fi) |
| Video: time frozen | 3% | 24% |
| Normal use: fps / time frozen | 35–57 / 6–8% | 9 / 45% |
| Phone buffer (p50 / max) | 24–50 ms / 67 ms | 104–404 ms / 1.6 s |
| Frames dropped on the Mac | 0 | ~600 |
| Cellular → Wi-Fi | seamless | — |
| Wi-Fi → cellular | not measured yet | ~20 s on the WebSocket, 2–4 s behind |

- **`robust-stream`'s rate controller spirals to its floor on a clean network.** It throws frames away and builds seconds of delay. So this branch starts again from `master`, and the branch is kept only for reference.
- **What was carried over:** the phone measurement mode and `tools/phonetest/`.
- **Network conditions to design for:** the phone's 5G drops and recovers often. Every change has to handle short, repeated dips and fast recoveries, not just one steady bandwidth.

### `master`'s known weaknesses

- **Bitrate pinned at 1 Mbit/s.** The encoder runs at GCC's estimate × 0.85, floored at 1 Mbit/s, and pion's GCC estimate stays low. The result is smooth but soft at 2622×1704 on Wi-Fi.
- **Below 1 Mbit/s it oversends.** On the WebRTC path nothing drops frames when the link can't keep up, so a deep 5G dip turns into delay.
- **Reconnects show stale video.** A dropped connection goes back to WebSocket video, which runs seconds behind over the relay, and WebRTC retries back off for 5 s, 10 s, 20 s and so on.
- **A ~5 s blank reconnect on cellular.** One was seen during the `master` test.

## The phone test

1. Run `make reload`. When you're ready on the phone, run `python3 tools/phonetest/record.py 1800`. Only arm it when you are about to start: it expires.
2. On the iPhone: `devices/` → sign in → **mac**.
3. **Video:** play a YouTube video on the Mac for about 70 s, with one screen switch.
4. **Normal use:** about 60 s of scrolling, switching tabs and switching Spaces.
5. **Network switches:** switch to mobile data and use it for about 40 s, then switch back.
6. Run `python3 tools/phonetest/analyze.py` and compare each phase with the table above.

## Steps

**1. Small fixes.** Done in `56899da`.
- The frame counter counts each frame once after a reconnect.
- The viewer says `bye` when you leave, so the Mac's panel clears at once.
- The stats log GCC's estimate, giving real data for step 2.

**2 and 3 (first version): done in `42e9130`, checked offline, not yet on the phone.**

**How it works**
- **Signals:** the rate is steered by a queue probe on the unordered channel plus the phone's receiver-report loss. GCC is ignored: its loss figure reads 5–30% on a clean jittery path.
- **Dips:** cut fast on a real queue or loss, then jump back to the last good rate when the dip clears. Probing is gentle near the last limit.
- **Retransmits:** NACK resends are capped. Unlimited resends caused a storm after every dip, about 3000 drops a second.
- **Reconnects:** the viewer holds the last frame and re-offers WebRTC, never stale WebSocket video.

**Offline evidence**
- **Unit simulations:** `ratectl_test.go` covers dips, a step down, a path change, a blackout, and still content followed by motion. A 30-seed sweep with 40 ms jitter (`SEEDS=1 go test -run Seeds`) gave 0 slow recoveries and a between-dip queue p90 with median 64 ms and max 155 ms.
- **`streambench`:** first its proxy was fixed. It used to charge dropped packets against link time and never tail-dropped, which skewed all earlier capped-link numbers.
  - Clean link, plugged in: 53.7 fps, p95 11 ms, 3 → 20 Mbit/s in 11 s.
  - Three 5G-style dips and a blackout: every recovery reached about 55 fps within 2–3 s. `master` stayed stuck at 1.06 Mbit/s after its first dip.
- **Watch out:** on battery, the Mac's capture drops to about 30 fps. Benches are only comparable when the power state matches.

**Known cost:** a sudden dip from a high rate fills the path's buffer once before the cut lands. Latency p99 is about 1 s in the harsh bench, against about 0.3 s for `master`, which stays at 1 Mbit/s. A candidate fix is dropping frames before encoding when the probe reads a deep queue (step 5).

**2. A controller built for 5G: drop fast, recover fast, never stall.**
- **Log first.** Record what GCC's estimate, loss and RTT do across real 5G dips and Wi-Fi.
- **Cut quickly** on real congestion: loss bursts, or RTT rising above its recent floor.
- **Remember the last good rate.** When a dip clears (RTT back near the floor and no loss for about 1 s), return to that rate in one or two steps instead of crawling up from the floor.
- **Climb above 1 Mbit/s** when the link is clean, so Wi-Fi gets sharp text.
- **Never stall on a deep dip.** Lower the floor, but only together with lower resolution or fps, so a dip below 1 Mbit/s neither freezes nor queues.
- **Target:**
  - Wi-Fi: fps p50 ≥ 55, under 2% frozen, buffer p50 under 60 ms, sharp text.
  - 5G: fps p50 ≥ 45 and under 5% frozen. Every dip recovers within 2 s.

**3. Network switches without stale video.**
- On `disconnected`, re-offer WebRTC straight away (the server already accepts a fresh `rtc-offer`).
- Hold the last frame with a "reconnecting" note instead of switching to WebSocket video.
- Use WebSocket video only if WebRTC can't come back within about 6 s.
- **Target:** live within 3 s of a Wi-Fi ↔ cellular switch, and never a picture that's seconds behind.

**4. Resolution that fits the link.** Scale 2622×1704 down when the bitrate is low. A phone doesn't need full resolution at 1 Mbit/s.

**5. Only if still useful:** frame-level backpressure to the helper (drop before encoding), FlexFEC for lossy 5G, and a QP cap.

## Ground rules

- Every change is judged by the phone test, with the numbers recorded here.
- Keep the invariants in `CLAUDE.md`.
- Commit on `smooth-stream` as the repo owner, with no AI attribution. Don't push without asking.
