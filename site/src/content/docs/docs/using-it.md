---
title: Using it
description: Gestures, keyboard, the toolbar, displays, the extended display, sound and files.
---

## Gestures

The ◎/☝︎ button in the toolbar switches between **trackpad mode** and **touch mode**.

| Gesture | Trackpad mode (◎) | Touch mode (☝︎) |
|---|---|---|
| One finger | Moves the cursor | Drags at that spot |
| Tap | Click at the cursor | Click at that spot |
| Hold | Starts a drag (or tap, then drag) | Right-click |

In both modes:

- **Two-finger tap** right-clicks, **two-finger drag** scrolls, and **pinch** zooms (the view follows the cursor).
- **Three-finger swipe** up or down opens Mission Control or App Exposé; left or right switches Spaces.
- **Three-finger tap** opens the keyboard.

Pointer speed and natural scrolling are in the **⋯** panel.

## Keyboard

Keys reach the Mac as real key presses, so app and website shortcuts work (YouTube's Shift+> for speed, Space, J/K/L), and a key held down stays held on the Mac, for games and hold-to-repeat.

- The **⌘ ⌥ ⌃ ⇧** buttons are sticky: they apply to the next key or click.
- A hardware keyboard or trackpad on an iPad works directly.
- In full screen in Chrome or Edge, the browser's own shortcuts (⌘W, ⌘T, Esc) go to the Mac too; hold Esc to leave full screen. Safari keeps those shortcuts for itself.
- The **⋯** panel has one-tap Mission Control, Spotlight, Switch App, Desktop, Screenshot, Force Quit and Lock Mac, plus media, volume and brightness keys.

## Toolbar

- The toolbar floats over the screen. Drag its ⠿ grip to put it anywhere (it stays there next time). Tap the grip to fold it down to a small button, or open it again.
- To hide the toolbar completely, tap the status pill at the top (ping · fps). Tap the pill again to bring it back.
- The full-screen button (desktop browsers, Android, iPad) sits next to **⋯**.
- **⋯** holds everything else: view (Fit or Fill), input mode, quality, video codec, frame rate, sound, files, clipboard and signing out.

## Clipboard

Clipboard works both ways: **Paste** and **Copy** in the **⋯** panel, or type text into the box there and choose **Type on Mac** or **Copy to Mac**. Secrets copied from password managers are never sent.

## Displays

If the Mac has more than one display, pick which one to show in the **⋯** panel. It switches live, and taps, the pointer and the cursor follow it.

## Extended display

Use a laptop's, tablet's or phone's browser as a second monitor for the Mac. Open the viewer on that device, tap the extended-display button in the toolbar and start it. The Mac gets a virtual display the size of that screen: drag windows onto it, and the mouse crosses over like a real monitor.

- **Always an extra screen.** The Mac's own display stays the main one. Nothing moves the menu bar, the Dock or your arrangement.
- **Refresh rate.** 30 Hz by default (plenty for documents, and light on the Mac); 60 Hz is a choice in the same panel. Switching while it's on keeps the display and its windows where they are.
- **Where it sits.** Drag it to any side in System Settings → Displays → Arrange, and it opens on that side every time after.
- **Closing it.** **Exit** in the viewer closes it right away. If the connection just drops, it stays for 5 minutes so a reconnect finds your windows where you left them.
- **One device at a time.** A second device is asked whether to take over; the first is told it was taken over and doesn't fight back. Taking over can also close the extended display.

It needs no permission beyond Screen Recording. Caveats:

- It uses a private macOS API (`CGVirtualDisplay`), so a macOS update can break it.
- The Mac keeps a small colour-profile file per display size, in the system ColorSync folder.
- Keep the Mac's lid open.

## Sound

Off by default. Switch on **⋯ → Sound** to hear the Mac on this device. It travels over WebRTC, and the Mac keeps playing it too. If the browser held it back, tap the screen once.

Sound is one-way (no microphone from the phone to the Mac), and there's none on the WebSocket fallback.

## Sending files

Drop files anywhere on the screen, or use **⋯ → Send a file to the Mac**. They're saved in the Mac's Downloads folder. Nothing is ever replaced, and the viewer can't read files on the Mac.

## Picture quality and latency

- **Sharper text.** Where the browser can play HEVC (recent Safari, Chrome and Edge), the Mac sends HEVC instead of H.264, which keeps text crisper at the same bitrate. When the screen stops moving, a few extra frames with more bits sharpen it further. **⋯ → Video → H.264** switches HEVC off.
- **Quality** (Saver, Auto, Crisp) and **Frame rate** (30 or 60) are in the **⋯** panel.
- **Lowest latency** (on by default) is *direct video*: WebRTC hands each complete frame to the viewer before the browser's jitter buffer, and it's decoded with WebCodecs and drawn at once. The panel shows whether it's active in this browser. If it ever stalls or fails, it switches itself off for the session; turn it off yourself in **⋯** if you prefer the browser's own video player.

## Lock screen, sleep and restarts

- **Locked Mac.** Your phone shows "Mac is locked". Type the Mac password and it's entered on the real lock screen through the Mac's own Screen Sharing, which must be on. The fast stream then resumes.
  - macOS locks the Mac again as soon as the Screen Sharing session that unlocked it ends. So that connection stays open, idle, while you're viewing, and closes a minute after you leave (or at once when Stop is pressed on the Mac). The Mac then locks itself again.
- **Sleep.** Turn on *Keep this Mac reachable while plugged in* in settings. The display still sleeps, but the Mac stays online.
- **Restart.** With FileVault on, the Mac waits at the disk-unlock screen before anything starts, so no remote tool can reach it. Someone has to type the password there once; after that, tailreach starts on its own. For restarts you plan, `sudo fdesetup authrestart` skips that screen once.

## Signing out

**Sign out of this device** (in **⋯**, or on the device list) ends the session and forgets the device, so the password is asked again next time.
