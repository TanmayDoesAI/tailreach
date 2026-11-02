---
title: Using it
description: Gestures, keyboard, second screen, watching, lock screen, files and sound.
---

## Touch and keys

The toolbar switches between **trackpad mode** (move a cursor, tap to click) and **touch mode** (tap where you want to click).

- **Two fingers:** tap to right-click, drag to scroll. **Pinch** to zoom.
- **Three fingers:** swipe for Mission Control and Spaces, tap for the keyboard.
- Keys arrive as real key presses, so shortcuts and held keys work, following the Mac's keyboard layout (AZERTY, QWERTZ…). Input methods (Japanese, Chinese, Korean) type as usual. **⌘ ⌥ ⌃ ⇧** in the toolbar apply to the next key.
- The **⋯** panel has everything else: quality, frame rate, codec, sound, clipboard, Mission Control, Spotlight, Force Quit, Lock.

Drag the toolbar by its grip to move it; tap the status pill to hide it.

## Second screen

Open tailreach on the laptop or tablet, tap the extended-display button and **Start**. The Mac gets a real extra display the size of that screen: drag windows onto it like a plugged-in monitor.

- It's always an extra screen; your Mac's own display stays main.
- 30 Hz by default (light on the Mac), 60 Hz if you want.
- **Exit** closes it. If the connection drops, it waits 5 minutes for you to come back.
- It uses a private macOS API, so a macOS update could break it.

## Locked, asleep, restarted

- **Locked:** the viewer says so. Type your Mac password and it goes into the real lock screen (needs Screen Sharing on).
- **Asleep:** turn on **Keep Mac Reachable** in the menu-bar icon (or `tailreach settings`).
- **Restarted with FileVault:** someone has to type the password at the startup screen once. `sudo fdesetup authrestart` skips it for planned restarts.

## Files, clipboard, sound

- **Files:** drop them on the screen. They land in the Mac's Downloads; nothing is ever replaced or read.
- **Clipboard:** **Copy** and **Paste** in **⋯**, both ways, text only (not images or files). Password-manager secrets are never sent.
- **Sound:** off by default; **⋯ → Sound** turns it on.

## One controls, others can watch

One device controls the Mac at a time. Another one is asked whether to **take over** or **just watch**: watching shows the same screen, view only, alongside whoever has control (up to 4 at once, guests too). A link ending in `?watch` (like `…/mac/?watch`) goes straight to watching. The Mac's panel counts who's watching, and **Stop** on the Mac, or **Pause sharing** in the menu bar, ends everyone's view.
