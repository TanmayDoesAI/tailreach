// Mac Remote — fast viewer.
// Hardware H.264 from the Mac → WebSocket → WebCodecs VideoDecoder → <canvas>.
// Touch, mouse and keyboard go back as small JSON messages.
'use strict';

// ---------- settings you might want to tweak ----------
const PRESETS = {
  saver: { maxSide: 1280, maxbr: 3_000_000 },
  auto:  { maxSide: 2560, maxbr: 20_000_000 },
  crisp: { maxSide: 3840, maxbr: 40_000_000 },
};
const TAP_MS = 250;          // max duration of a tap
const LONG_PRESS_MS = 420;   // hold this long to start a drag (trackpad) / right-click (touch)
const MOVE_SLOP = 8;         // px of movement before a touch counts as a move
const SCROLL_GAIN = 1.6;

// ---------- small helpers ----------
const $ = (s) => document.querySelector(s);
const store = {
  get(k, d) { try { const v = localStorage.getItem('md.' + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem('md.' + k, JSON.stringify(v)); } catch {} },
};
const settings = {
  quality: store.get('quality', 'auto'),
  fps: store.get('fps', 60),
  speed: store.get('speed', 1.4),
  natural: store.get('natural', true),
  mode: store.get('mode', 'trackpad'),   // 'trackpad' | 'touch'
};

const canvas = $('#screen');
const ctx2d = canvas.getContext('2d', { alpha: false, desynchronized: true });
const surface = $('#surface');

let ws = null;
let caps = {};
let locked = false;
let decoder = null;
let waitKey = true;
let retry = 0;
let wakeLock = null;
const stats = { frames: 0, fps: 0, rtt: 0, kbps: 0 };

// ---------- connection ----------
function setState(state, text) {
  document.body.dataset.state = state;   // 'off' | 'connecting' | 'on'
  $('#splashText').textContent = text;
  $('#statusText').textContent = text;
}

function splashAction(label, href) {
  const a = $('#splashAction');
  a.textContent = label;
  a.href = href;
  a.classList.remove('hidden');
}

async function boot() {
  if (!('VideoDecoder' in window)) {
    setState('off', 'This browser can’t decode video streams.');
    splashAction('Use classic (VNC) mode', 'classic/');
    return;
  }
  try {
    caps = await (await fetch('caps', { cache: 'no-store' })).json();
    $('#macUser').textContent = caps.user;
    $('#macUserField').value = caps.user;
    document.title = `${caps.name} — Mac Remote`;
    if (!caps.fast) { location.replace('classic/'); return; }
  } catch {
    setState('off', 'Can’t reach this device.');
    return setTimeout(boot, 3000);
  }
  connect();
}

function connect() {
  if (ws) return;
  setState('connecting', retry ? 'Reconnecting…' : 'Connecting…');
  const url = new URL('stream', location.href);
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    retry = 0;
    const p = PRESETS[settings.quality] || PRESETS.auto;
    const side = Math.min(p.maxSide, Math.round(Math.max(screen.width, screen.height) * devicePixelRatio));
    send({ t: 'start', w: side, h: side, fps: +settings.fps, maxbr: p.maxbr });
    keepAwake();
  };
  ws.onmessage = (e) => (typeof e.data === 'string' ? onEvent(JSON.parse(e.data)) : onBinary(e.data));
  ws.onclose = () => {
    ws = null;
    resetDecoder();
    if (document.visibilityState === 'visible') {
      setState('connecting', 'Reconnecting…');
      setTimeout(connect, Math.min(5000, 300 * 2 ** retry++));
    }
  };
}

function send(obj) {
  if (ws && ws.readyState === 1) ws.send(JSON.stringify(obj));
}

// Drop the connection while the app is in the background; resume instantly on return.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden') ws?.close();
  else connect();
});

async function keepAwake() {
  try { wakeLock = await navigator.wakeLock?.request('screen'); } catch {}
}

// ---------- video ----------
function resetDecoder() {
  try { decoder?.close(); } catch {}
  decoder = null;
  waitKey = true;
}

function onBinary(buf) {
  const type = new Uint8Array(buf, 0, 1)[0];
  if (type === 1) {                     // decoder config: [u16 len][json][avcC]
    const dv = new DataView(buf);
    const jlen = dv.getUint16(1);
    const cfg = JSON.parse(new TextDecoder().decode(new Uint8Array(buf, 3, jlen)));
    const description = new Uint8Array(buf.slice(3 + jlen));
    resetDecoder();
    decoder = new VideoDecoder({
      output: drawFrame,
      error: (err) => { console.warn('decoder', err); resetDecoder(); send({ t: 'kf' }); },
    });
    decoder.configure({
      codec: cfg.codec, codedWidth: cfg.w, codedHeight: cfg.h, description,
      optimizeForLatency: true, hardwareAcceleration: 'prefer-hardware',
    });
    if (canvas.width !== cfg.w || canvas.height !== cfg.h) {
      canvas.width = cfg.w;
      canvas.height = cfg.h;
      layout();
    }
    return;
  }
  if (type === 2 && decoder && decoder.state === 'configured') {   // [u8 key][u64 µs][AVCC]
    const dv = new DataView(buf);
    const key = dv.getUint8(1) === 1;
    if (waitKey && !key) return;
    // A device that can't keep up skips ahead instead of falling behind.
    if (decoder.decodeQueueSize > 4 && !key) { waitKey = true; send({ t: 'kf' }); return; }
    waitKey = false;
    decoder.decode(new EncodedVideoChunk({
      type: key ? 'key' : 'delta',
      timestamp: Number(dv.getBigUint64(2)),
      data: new Uint8Array(buf, 10),
    }));
  }
}

function drawFrame(frame) {
  ctx2d.drawImage(frame, 0, 0, canvas.width, canvas.height);
  frame.close();
  stats.frames++;
  if (document.body.dataset.state !== 'on') setState('on', 'Connected');
}

function onEvent(ev) {
  switch (ev.t) {
    case 'cursor': cursor.x = ev.x; cursor.y = ev.y; followCursor(); break;
    case 'stats': stats.kbps = ev.kbps; break;
    case 'pong': stats.rtt = Math.round(performance.now() - ev.at); break;
    case 'lock': setLocked(ev.locked); break;
    case 'error':
      if (locked) break;   // capture pauses while the Mac is locked; that's expected
      setState('off', ev.needsPermission
        ? 'Allow Screen Recording for mydevices-capture on the Mac (System Settings → Privacy & Security).'
        : `Mac: ${ev.msg}`);
      break;
    case 'warn': console.warn(ev.msg); break;
    case 'clip': onMacClipboard(ev); break;
  }
}

setInterval(() => {
  send({ t: 'ping', at: performance.now() });
  stats.fps = stats.frames; stats.frames = 0;
  if (document.body.dataset.state === 'on') {
    $('#statusText').textContent = `${stats.rtt} ms · ${stats.fps} fps · ${(stats.kbps / 1000).toFixed(1)} Mbps`;
  }
}, 1000);

// ---------- lock screen ----------
// Capture and simulated typing can't reach macOS's lock screen, but the Mac's own
// Screen Sharing can. So: sign in to Screen Sharing (over the same tailnet-only
// connection), type the password into the lock screen, hang up, resume the fast stream.
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function setLocked(v) {
  const wasLocked = locked;
  locked = v;
  document.body.classList.toggle('locked', v);
  $('#unlock').classList.toggle('hidden', !v);
  if (v) {
    $('#unlockErr').textContent = '';
    kbd.blur();
  } else if (wasLocked) {
    ws?.close();   // fresh capture of the unlocked desktop (reconnects right away)
  }
}

async function unlockMac(password) {
  const [{ default: RFB }, { default: KeyTable }, { default: keysyms }] = await Promise.all([
    import('./novnc/core/rfb.js'), import('./novnc/core/input/keysym.js'), import('./novnc/core/input/keysymdef.js'),
  ]);
  const holder = document.createElement('div');
  holder.style.cssText = 'position:fixed;left:-10px;top:-10px;width:1px;height:1px;overflow:hidden';
  document.body.append(holder);
  const url = new URL('websockify', location.href);
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';

  await new Promise((resolve, reject) => {
    const rfb = new RFB(holder, url.href, { credentials: { username: caps.user, password } });
    let settled = false;
    const finish = (err) => {
      if (settled) return;
      settled = true;
      try { rfb.disconnect(); } catch {}
      holder.remove();
      err ? reject(err) : resolve();
    };
    rfb.addEventListener('securityfailure', () => finish(new Error('Wrong password.')));
    rfb.addEventListener('credentialsrequired', () => finish(new Error('Wrong password.')));
    rfb.addEventListener('disconnect', () => finish(new Error('Screen Sharing is off on the Mac, so it can’t be unlocked remotely.')));
    rfb.addEventListener('connect', async () => {
      rfb.sendKey(KeyTable.XK_Shift_L, 'ShiftLeft');   // wake the display, reveal the password field
      await sleep(900);
      for (const ch of password) rfb.sendKey(keysyms.lookup(ch.codePointAt(0)));
      rfb.sendKey(KeyTable.XK_Return, 'Enter');
      await sleep(1200);
      finish();
    });
    setTimeout(() => finish(new Error('The Mac didn’t answer.')), 15000);
  });
}

$('#unlock').addEventListener('submit', async (e) => {
  e.preventDefault();
  const pw = $('#macPw').value;
  if (!pw) return;
  $('#unlockBtn').disabled = true;
  $('#unlockBtn').textContent = 'Unlocking…';
  $('#unlockErr').textContent = '';
  try {
    await unlockMac(pw);
    for (let i = 0; i < 20 && locked; i++) await sleep(250);   // wait for the Mac to report it
    if (locked) $('#unlockErr').textContent = 'Still locked. Check the password and try again.';
  } catch (err) {
    $('#unlockErr').textContent = err.message;
  } finally {
    $('#macPw').value = '';
    $('#unlockBtn').disabled = false;
    $('#unlockBtn').textContent = 'Unlock';
  }
});

// ---------- view: fit, zoom and pan ----------
const view = { z: 1, x: 0, y: 0 };   // zoom factor and top-left offset in CSS px
const cursor = { x: 0.5, y: 0.5 };   // remote cursor, normalised 0..1

function baseScale() {
  return Math.min(innerWidth / (canvas.width || 1), innerHeight / (canvas.height || 1));
}

function layout() {
  const s = baseScale() * view.z;
  const w = canvas.width * s, h = canvas.height * s;
  // Center when smaller than the screen, otherwise keep the content covering it.
  view.x = w <= innerWidth ? (innerWidth - w) / 2 : Math.min(0, Math.max(innerWidth - w, view.x));
  view.y = h <= innerHeight ? (innerHeight - h) / 2 : Math.min(0, Math.max(innerHeight - h, view.y));
  canvas.style.transform = `translate(${view.x}px, ${view.y}px) scale(${s})`;
}
addEventListener('resize', layout);

function zoomAt(newZ, cx, cy) {
  const old = baseScale() * view.z;
  view.z = Math.min(6, Math.max(1, newZ));
  const s = baseScale() * view.z;
  view.x = cx - (cx - view.x) * (s / old);
  view.y = cy - (cy - view.y) * (s / old);
  layout();
}

// When zoomed in with the trackpad, keep the cursor in view.
function followCursor() {
  if (view.z === 1 || settings.mode !== 'trackpad') return;
  const s = baseScale() * view.z;
  const px = view.x + cursor.x * canvas.width * s;
  const py = view.y + cursor.y * canvas.height * s;
  const mx = innerWidth * 0.2, my = innerHeight * 0.2;
  if (px < mx) view.x += mx - px; else if (px > innerWidth - mx) view.x -= px - (innerWidth - mx);
  if (py < my) view.y += my - py; else if (py > innerHeight - my) view.y -= py - (innerHeight - my);
  layout();
}

// Screen point → remote position (0..1)
function toRemote(clientX, clientY) {
  const s = baseScale() * view.z;
  return {
    x: Math.min(1, Math.max(0, (clientX - view.x) / (canvas.width * s))),
    y: Math.min(1, Math.max(0, (clientY - view.y) / (canvas.height * s))),
  };
}

// ---------- modifiers ----------
const sticky = new Set();
function mods() {
  const m = [...sticky];
  sticky.clear();
  document.querySelectorAll('[data-mod]').forEach((b) => b.classList.remove('active'));
  return m;
}

// ---------- clicks ----------
let lastClick = { at: 0, n: 0 };
function click(button = 0) {
  const now = performance.now();
  const n = button === 0 && now - lastClick.at < 400 ? Math.min(3, lastClick.n + 1) : 1;
  lastClick = { at: now, n };
  send({ t: 'click', b: button, n, m: mods() });
}

// ---------- touch gestures ----------
// Trackpad mode:  1 finger moves the cursor · tap = click · hold or tap-then-drag = drag
// Touch mode:     tap where you want to click · drag = drag · hold = right-click
// Both:           2-finger tap = right-click · 2-finger drag = scroll · pinch = zoom
//                 3-finger swipe up/down = Mission Control / App Exposé · left/right = switch Spaces
//                 3-finger tap = keyboard
const g = { active: false };

function touchInfo(list) {
  let x = 0, y = 0;
  for (const t of list) { x += t.clientX; y += t.clientY; }
  x /= list.length; y /= list.length;
  let dist = 0;
  if (list.length >= 2) dist = Math.hypot(list[0].clientX - list[1].clientX, list[0].clientY - list[1].clientY);
  return { x, y, dist, n: list.length };
}

surface.addEventListener('touchstart', (e) => {
  e.preventDefault();
  const t = touchInfo(e.touches);
  const now = performance.now();
  if (!g.active) {
    Object.assign(g, {
      active: true, start: now, maxN: t.n, moved: false, mode: null,
      sx: t.x, sy: t.y, lx: t.x, ly: t.y, lt: now,
      tapDrag: settings.mode === 'trackpad' && now - (g.lastTapEnd || 0) < 280,
    });
    clearTimeout(g.longTimer);
    g.longTimer = setTimeout(onLongPress, LONG_PRESS_MS);
  } else {
    clearTimeout(g.longTimer);
  }
  g.maxN = Math.max(g.maxN, t.n);
  if (t.n >= 2) {   // re-anchor multi-finger gestures
    Object.assign(g, { sx: t.x, sy: t.y, lx: t.x, ly: t.y, sdist: t.dist, sz: view.z, mode: g.mode === 'drag' ? 'drag' : null });
  }
}, { passive: false });

function onLongPress() {
  if (!g.active || g.moved || g.maxN !== 1) return;
  if (settings.mode === 'trackpad') {
    g.mode = 'drag';
    send({ t: 'btn', b: 0, down: true, n: 1, m: mods() });
  } else {
    const p = toRemote(g.sx, g.sy);
    send({ t: 'abs', x: p.x, y: p.y });
    click(1);
    g.mode = 'done';
  }
  navigator.vibrate?.(10);
}

surface.addEventListener('touchmove', (e) => {
  e.preventDefault();
  if (!g.active) return;
  const t = touchInfo(e.touches);
  const now = performance.now();
  const dx = t.x - g.lx, dy = t.y - g.ly, dt = Math.max(1, now - g.lt);
  if (!g.moved && Math.hypot(t.x - g.sx, t.y - g.sy) > MOVE_SLOP) { g.moved = true; clearTimeout(g.longTimer); }

  if (t.n === 1 && g.maxN === 1) {
    if (settings.mode === 'trackpad') {
      if (g.tapDrag && g.moved && !g.mode) { g.mode = 'drag'; send({ t: 'btn', b: 0, down: true, n: 1 }); }
      if (g.moved || g.mode === 'drag') {
        const v = Math.hypot(dx, dy) / dt;                 // px per ms
        const gain = settings.speed * (1 + Math.min(2.5, v * 0.9));
        send({ t: 'rel', dx: dx * gain, dy: dy * gain });
      }
    } else if (g.moved && g.mode !== 'done') {
      const p = toRemote(t.x, t.y);
      if (!g.mode) {
        const p0 = toRemote(g.sx, g.sy);
        send({ t: 'abs', x: p0.x, y: p0.y });
        send({ t: 'btn', b: 0, down: true, n: 1, m: mods() });
        g.mode = 'drag';
      }
      send({ t: 'abs', x: p.x, y: p.y });
    }
  } else if (t.n === 2) {
    if (!g.mode) {
      const ratio = t.dist / (g.sdist || 1);
      if (Math.abs(ratio - 1) > 0.12) g.mode = 'pinch';
      else if (Math.hypot(t.x - g.sx, t.y - g.sy) > MOVE_SLOP) g.mode = 'scroll';
    }
    if (g.mode === 'scroll') {
      const k = SCROLL_GAIN * (settings.natural ? 1 : -1);
      send({ t: 'scroll', dx: dx * k, dy: dy * k });
    } else if (g.mode === 'pinch') {
      zoomAt(g.sz * (t.dist / (g.sdist || 1)), t.x, t.y);
      view.x += dx; view.y += dy;   // pinch also pans
      layout();
    }
  } else if (t.n >= 3) {
    g.mode = 'three';
  }
  g.lx = t.x; g.ly = t.y; g.lt = now;
}, { passive: false });

function endTouch(e) {
  e.preventDefault();
  if (!g.active || e.touches.length > 0) return;
  clearTimeout(g.longTimer);
  const dur = performance.now() - g.start;

  if (g.mode === 'drag') {
    send({ t: 'btn', b: 0, down: false, n: 1 });
  } else if (g.maxN === 1 && !g.moved && !g.mode && dur < LONG_PRESS_MS) {
    if (settings.mode === 'touch') {
      const p = toRemote(g.sx, g.sy);
      send({ t: 'abs', x: p.x, y: p.y });
    }
    click(0);
    g.lastTapEnd = performance.now();
  } else if (g.maxN === 2 && !g.mode && dur < TAP_MS * 1.6) {
    click(1);
  } else if (g.maxN >= 3) {
    const dx = g.lx - g.sx, dy = g.ly - g.sy;
    if (Math.hypot(dx, dy) < 30) toggleKeyboard();
    else if (Math.abs(dy) > Math.abs(dx)) send({ t: 'key', k: dy < 0 ? 'up' : 'down', m: ['ctrl'] });
    else send({ t: 'key', k: dx > 0 ? 'left' : 'right', m: ['ctrl'] });
  }
  g.active = false;
}
surface.addEventListener('touchend', endTouch, { passive: false });
surface.addEventListener('touchcancel', endTouch, { passive: false });
document.addEventListener('gesturestart', (e) => e.preventDefault());

// ---------- real mouse / trackpad (iPad with trackpad, laptops) ----------
surface.addEventListener('pointermove', (e) => {
  if (e.pointerType !== 'mouse') return;
  const p = toRemote(e.clientX, e.clientY);
  send({ t: 'abs', x: p.x, y: p.y });
});
surface.addEventListener('pointerdown', (e) => {
  if (e.pointerType !== 'mouse') return;
  surface.setPointerCapture(e.pointerId);
  send({ t: 'btn', b: e.button === 2 ? 1 : e.button === 1 ? 2 : 0, down: true, n: Math.max(1, e.detail), m: physicalMods(e) });
});
surface.addEventListener('pointerup', (e) => {
  if (e.pointerType !== 'mouse') return;
  send({ t: 'btn', b: e.button === 2 ? 1 : e.button === 1 ? 2 : 0, down: false, n: Math.max(1, e.detail), m: physicalMods(e) });
});
surface.addEventListener('contextmenu', (e) => e.preventDefault());
surface.addEventListener('wheel', (e) => {
  e.preventDefault();
  if (e.ctrlKey) { zoomAt(view.z * Math.exp(-e.deltaY / 200), e.clientX, e.clientY); return; }   // trackpad pinch
  const k = e.deltaMode === 1 ? 16 : 1;
  send({ t: 'scroll', dx: -e.deltaX * k, dy: -e.deltaY * k });
}, { passive: false });

function physicalMods(e) {
  const m = mods();
  if (e.metaKey) m.push('cmd');
  if (e.altKey) m.push('opt');
  if (e.ctrlKey) m.push('ctrl');
  if (e.shiftKey) m.push('shift');
  return m;
}

// ---------- keyboard ----------
const kbd = $('#kbd');
const KEY_NAMES = {
  Enter: 'return', Tab: 'tab', Escape: 'esc', Backspace: 'backspace', Delete: 'delete', ' ': 'space',
  ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down',
  Home: 'home', End: 'end', PageUp: 'pageup', PageDown: 'pagedown',
  F1: 'f1', F2: 'f2', F3: 'f3', F4: 'f4', F5: 'f5', F6: 'f6', F7: 'f7', F8: 'f8', F9: 'f9', F10: 'f10', F11: 'f11', F12: 'f12',
};

function sendKey(k, extra = []) {
  send({ t: 'key', k, m: [...new Set([...mods(), ...extra])] });
}

function sendText(s) {
  if (sticky.size && s.length === 1) sendKey(s);   // e.g. ⌘ then "c"
  else send({ t: 'text', s });
}

// Physical keyboards (iPad Magic Keyboard, laptops) and special keys
document.addEventListener('keydown', (e) => {
  if (e.target.closest?.('#more')) return;   // typing in the panel
  const named = KEY_NAMES[e.key];
  const chord = e.metaKey || e.ctrlKey || e.altKey;
  const fromSoftKbd = e.target === kbd;
  // The on-screen keyboard's text, Enter and Backspace arrive via beforeinput below.
  if (fromSoftKbd && !chord && (!named || ['return', 'backspace', 'space'].includes(named))) return;
  if (!named && !chord && e.key.length !== 1) return;
  e.preventDefault();
  const m = [];
  if (e.metaKey) m.push('cmd');
  if (e.altKey) m.push('opt');
  if (e.ctrlKey) m.push('ctrl');
  if (e.shiftKey && (named || chord)) m.push('shift');
  if (named) sendKey(named, m);
  else if (chord) sendKey(e.key.toLowerCase(), m);
  else sendText(e.key);
});

// On-screen keyboard: keep the textarea empty, forward what would have been typed
kbd.addEventListener('beforeinput', (e) => {
  e.preventDefault();
  switch (e.inputType) {
    case 'insertText':
    case 'insertReplacementText':
    case 'insertFromPaste': {
      const s = e.data ?? e.dataTransfer?.getData('text/plain') ?? '';
      if (s) sendText(s);
      break;
    }
    case 'insertLineBreak':
    case 'insertParagraph': sendKey('return'); break;
    case 'deleteContentBackward': sendKey('backspace'); break;
    case 'deleteWordBackward': sendKey('backspace', ['opt']); break;
  }
});
kbd.addEventListener('focus', () => document.body.classList.add('kbd-open'));
kbd.addEventListener('blur', () => document.body.classList.remove('kbd-open'));

function toggleKeyboard() {
  document.activeElement === kbd ? kbd.blur() : kbd.focus();
}

// Keep the toolbar above the on-screen keyboard
if (window.visualViewport) {
  const fit = () => {
    const vv = visualViewport;
    document.documentElement.style.setProperty('--kb-offset', `${Math.max(0, innerHeight - vv.height - vv.offsetTop)}px`);
  };
  visualViewport.addEventListener('resize', fit);
  visualViewport.addEventListener('scroll', fit);
}

// ---------- clipboard ----------
// iOS only lets a page write the clipboard during a tap, so Mac → phone goes through
// a button (in the panel, or the toast that appears when you copy on the Mac).
let macClip = '';
let clipWaiter = null;

function onMacClipboard(ev) {
  macClip = ev.s;
  if (clipWaiter) { clipWaiter(ev.s); clipWaiter = null; return; }
  if (ev.auto) {
    $('#toastText').textContent = `Mac copied: ${ev.s.replace(/\s+/g, ' ').slice(0, 60)}`;
    $('#toast').classList.remove('hidden');
    clearTimeout(onMacClipboard.t);
    onMacClipboard.t = setTimeout(() => $('#toast').classList.add('hidden'), 6000);
  }
}

function copyToPhone(textPromise) {
  // ClipboardItem accepts a promise, so the write still counts as part of the tap.
  if (window.ClipboardItem && navigator.clipboard?.write) {
    const blob = textPromise.then((t) => new Blob([t], { type: 'text/plain' }));
    return navigator.clipboard.write([new ClipboardItem({ 'text/plain': blob })]);
  }
  return textPromise.then((t) => navigator.clipboard.writeText(t));
}

function flash(msg) {
  $('#toastText').textContent = msg;
  $('#toast').classList.remove('hidden');
  clearTimeout(onMacClipboard.t);
  onMacClipboard.t = setTimeout(() => $('#toast').classList.add('hidden'), 2500);
}

// ---------- orientation ----------
// The stream is sized for the device's longer side, so rotating needs no reconnect: just re-fit.
function fitMode(mode) {
  view.z = 1;
  if (mode === 'fill' && canvas.width) {   // cover the whole screen, cropping the edges
    const b = baseScale();
    view.z = Math.max(1, innerWidth / (canvas.width * b), innerHeight / (canvas.height * b));
    view.x = (innerWidth - canvas.width * b * view.z) / 2;
    view.y = (innerHeight - canvas.height * b * view.z) / 2;
  }
  layout();
}
addEventListener('orientationchange', () => setTimeout(() => fitMode('fit'), 250));
screen.orientation?.addEventListener?.('change', () => setTimeout(() => fitMode('fit'), 250));

// ---------- toolbar & panel ----------
const actions = {
  keyboard: toggleKeyboard,
  more: () => $('#more').classList.toggle('hidden'),
  close: () => $('#more').classList.add('hidden'),
  mode: () => setMode(settings.mode === 'trackpad' ? 'touch' : 'trackpad'),
  typeText: () => send({ t: 'text', s: $('#clipText').value }),
  sendClip: () => { send({ t: 'clip-set', s: $('#clipText').value }); flash('Put on the Mac clipboard'); },
  pasteToMac: async () => {
    try {
      const text = await navigator.clipboard.readText();
      send({ t: 'clip-set', s: text });
      sendKey('v', ['cmd']);
      flash('Pasted on the Mac');
    } catch { flash('Clipboard access was blocked'); }
  },
  copyFromMac: () => {
    const text = new Promise((resolve) => { clipWaiter = resolve; send({ t: 'clip-get' }); setTimeout(() => resolve(macClip), 1500); });
    copyToPhone(text).then(() => flash('Copied from the Mac'), () => flash('Clipboard access was blocked'));
  },
  toastCopy: () => {
    copyToPhone(Promise.resolve(macClip)).then(() => flash('Copied'), () => flash('Clipboard access was blocked'));
  },
  toastClose: () => $('#toast').classList.add('hidden'),
  fit: () => { fitMode('fit'); $('#more').classList.add('hidden'); },
  fillScreen: () => { fitMode('fill'); $('#more').classList.add('hidden'); },
};

function setMode(m) {
  settings.mode = m;
  store.set('mode', m);
  document.body.dataset.mode = m;
  $('#modeBtn').textContent = m === 'trackpad' ? '◎' : '☝︎';
}

document.addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b) return;
  const d = b.dataset;
  if (d.act) actions[d.act]?.();
  if (d.key) sendKey(d.key);
  if (d.media) send({ t: 'media', k: d.media });
  if (d.combo) {
    const parts = d.combo.split('+');
    const k = parts.pop();
    sendKey(k, parts);
  }
  if (d.mod) {
    sticky.has(d.mod) ? sticky.delete(d.mod) : sticky.add(d.mod);
    b.classList.toggle('active', sticky.has(d.mod));
  }
});
// Toolbar taps shouldn't close the on-screen keyboard
document.querySelectorAll('#bar button, #keys button').forEach((b) => b.addEventListener('mousedown', (e) => e.preventDefault()));
document.querySelectorAll('#bar button, #keys button').forEach((b) => b.addEventListener('touchstart', () => {}, { passive: true }));
$('#status').addEventListener('click', () => document.body.classList.toggle('bar-hidden'));

// Settings
$('#quality').value = settings.quality;
$('#fps').value = String(settings.fps);
$('#speed').value = settings.speed;
$('#natural').checked = settings.natural;
$('#quality').addEventListener('change', (e) => { settings.quality = e.target.value; store.set('quality', settings.quality); ws?.close(); });
$('#fps').addEventListener('change', (e) => { settings.fps = +e.target.value; store.set('fps', settings.fps); ws?.close(); });
$('#speed').addEventListener('input', (e) => { settings.speed = +e.target.value; store.set('speed', settings.speed); });
$('#natural').addEventListener('change', (e) => { settings.natural = e.target.checked; store.set('natural', settings.natural); });

setMode(settings.mode);
boot();
