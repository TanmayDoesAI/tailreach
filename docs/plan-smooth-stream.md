# Plan: a stream that stays smooth (branch `robust-stream`)

Written 2026-10-25 after the first real-phone measurement. Every step is judged by the
phone test below, not by `tools/streambench` alone.

## What we know

The first real-iPhone recording was made on Wi-Fi, then cellular, with the Mac also playing a YouTube video. Its log is saved outside the repo as
`~/Library/Application Support/mydevices/phone-baseline-robust-stream-2026-10-25.jsonl`.

| Phase | fps (p50) | Time frozen | Bitrate (p50) | Phone buffer (p50) | Network RTT |
|---|---|---|---|---|---|
| YouTube playing, 71 s | 13 | 24% | 1.2 Mbit/s | 104 ms | 8 ms |
| Scrolling and switching Spaces, 62 s | 9 | 45% | 0.6 Mbit/s | 404 ms | 8 ms |
| Cellular after the switch, 39 s | 1 | 44% | 0.56 Mbit/s | 300 ms | 51 ms |

- **The pipeline can do it.** The first 18 s ran at 55 fps and 5–8 Mbit/s, with a ~50 ms buffer. Network loss was zero throughout, and the phone decodes a frame in ~8 ms.
- **The Mac's rate control collapses on a clean network.** About 20 s in, the bitrate is cut with no loss and an 8 ms RTT. It then spirals:
  - the target is clamped to the measured output;
  - starved frames get dropped, so the measured output falls further;
  - the target is cut again, until it reaches the 250k floor.
  - The Mac dropped about 600 frames before sending them.
- **Delays of a few seconds come from two queues stacking up.**
  - The Mac's pacer backlog reaches 0.4–0.9 s.
  - The phone's jitter buffer reaches 0.3–1.6 s and grows after every freeze.
- **Switching Spaces or tabs** produces a full-screen change, which can't clear at a starved bitrate. The result is drops and a freeze until a keyframe gets through.
- **Switching Wi-Fi to cellular** drops WebRTC with no ICE restart.
  - It takes about 20 s to come back.
  - Meanwhile the WebSocket fallback runs 2–4 s behind.
- **`tools/streambench` is not trustworthy when it runs on the same Mac.**
  - Its receiver's timing jitter under CPU load reads as congestion.
  - Clean A/A runs spread from 25 to 54 fps.
  - Use it only for network-impairment checks, and record the load average with each run.

## The phone test (the measure for every step)

1. `make reload`, then `python3 tools/phonetest/record.py 900`.
2. On the iPhone: `devices/` → sign in → **mac**.
3. Play a YouTube video on the Mac for about 70 s, hands off.
4. Scroll, switch tabs, and switch Spaces with the arrow keys, for about 60 s.
5. Switch the phone from Wi-Fi to mobile data, and keep using it for about 40 s.
6. Run `python3 tools/phonetest/analyze.py`, then compare the results per phase with the table above.

Note: the viewer's "shown" counter double-counts after a reconnect. Trust `dec` (frames decoded) until step 1 fixes it.

## Steps

**0. Run the test against `master`, about 5 minutes.** Deploy `master` with the measurement mode cherry-picked onto it (commit `8469ac2`), and run the phone test.
- If `master` holds 5+ Mbit/s on a clean network, the spiral came in with this branch. Fix it by removing logic, not adding more.
- If `master` also collapses, the problem is older.

**1. Stop the spiral (biggest win).**
- No bitrate cut without real evidence: loss, NACK bursts, or a rising RTT. Don't cut on estimate dips alone.
- Stop clamping the target to measured output while frames are being dropped, since that output was starved by us.
- Climb back to the last good rate quickly once the probe is clear.
- Fix the viewer's double-counted frame counter.
- **Target:** on the Wi-Fi phases, fps p50 ≥ 50, under 2% of the time frozen, and a phone buffer p50 under 80 ms.

**2. Keep delay from building up.**
- Ask the phone for a minimal playout delay, and let the buffer shrink back after a freeze.
- Drop frames before encoding (backpressure to the helper), not after. That keeps the reference chain intact, so no repair keyframe is needed.
- **Target:** never more than 300 ms behind. A Space switch costs no more than one brief dip.

**3. Survive network switches.**
- Restart ICE immediately when the network changes, and keep the picture on screen while it reconnects.
- Show "reconnecting" in place of a WebSocket picture that's seconds behind.
- **Target:** back to live video within 3 s of a Wi-Fi ↔ cellular switch.

**4. Use the bandwidth sensibly when it really is low.**
- Lower the resolution or fps when the bitrate is low. 2622×1704 is too much at 1 Mbit/s.
- Align the encoder's 500k clamp with `minBitrate`, and correct the `DataRateLimits` window (findings H1 and H2 in the review). A/B test this with the phone.
- **Target:** cellular at 1–2 Mbit/s stays readable at ≥ 30 fps.

**5. Then the remaining review items,** if still useful: FlexFEC, packet size, a QP cap and a shorter max keyframe interval.

## Ground rules

- Every change is judged by the phone test, with the numbers recorded here per step.
- Keep the invariants in `CLAUDE.md`.
- Commit on `robust-stream` as the repo owner, with no AI attribution. No pushes without asking.
