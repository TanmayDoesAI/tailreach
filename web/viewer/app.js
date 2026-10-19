// Mac Remote — fast viewer.
// Hardware H.264 from the Mac → WebRTC <video> (or WebSocket → WebCodecs → <canvas>).
// Touch, mouse and keyboard go back as small JSON messages, on WebRTC data channels
// when they're open and the WebSocket otherwise.
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
  display: store.get('display', 0),      // the Mac's display to show; 0 = its main one
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
let stopped = false;   // Stop was pressed on the Mac: don't reconnect until asked
let wakeLock = null;
const stats = { frames: 0, fps: 0, rtt: 0, kbps: 0 };

// ---------- connection ----------
let hasFrame = false;   // once we've shown the screen, reconnects keep it up instead of a splash

function setState(state, text) {
  if (state === 'connecting' && hasFrame) {
    document.body.classList.add('stale');   // last frame stays; the pill says what's happening
    $('#statusText').textContent = text;
    return;
  }
  document.body.classList.remove('stale');
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
  if (ws || stopped) return;
  setState('connecting', retry ? 'Reconnecting' : 'Connecting');
  const url = new URL('stream', location.href);
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    retry = 0;
    sendStart();
    keepAwake();
    startRTC();   // video starts on the WebSocket and moves to WebRTC once it connects
  };
  ws.onmessage = (e) => (typeof e.data === 'string' ? onEvent(JSON.parse(e.data)) : onBinary(e.data));
  let opened = false;
  ws.addEventListener('open', () => { opened = true; });
  ws.onclose = async (e) => {
    ws = null;
    stopRTC(false);
    resetDecoder();
    if (e.code === 4001) return stoppedOnMac();
    if (!opened && retry >= 1) {
      // Refused before it opened: find out why instead of retrying blindly.
      const r = await fetch('stream', { cache: 'no-store' }).catch(() => null);
      if (r?.status === 401) return location.replace('/login?next=' + encodeURIComponent(location.pathname));
      if (r?.status === 423) return stoppedOnMac();
    }
    if (document.visibilityState === 'visible') {
      setState('connecting', 'Reconnecting');
      setTimeout(connect, Math.min(5000, 300 * 2 ** retry++));
    }
  };
}

function sendStart() {
  const p = PRESETS[settings.quality] || PRESETS.auto;
  const side = Math.min(p.maxSide, Math.round(Math.max(screen.width, screen.height) * devicePixelRatio));
  send({ t: 'start', w: side, h: side, fps: +settings.fps, maxbr: p.maxbr, localCursor: true, display: settings.display });
}

function stoppedOnMac() {
  stopped = true;
  hasFrame = false;
  setState('off', 'Screen sharing was stopped on the Mac.');
  const a = $('#splashAction');
  splashAction('Try again', '');
  a.onclick = (e) => {
    e.preventDefault();
    a.classList.add('hidden');
    a.onclick = null;
    stopped = false;
    retry = 0;
    connect();
  };
}

// Input goes over WebRTC once it's up: UDP, so one lost packet never holds up the rest.
// Pointer positions take the channel that never resends (the next one supersedes it);
// everything that must arrive, in order, takes the reliable one. Big or control
// messages (start, clipboard, WebRTC setup) stay on the WebSocket.
const OVER_RTC = new Set(['rel', 'btn', 'click', 'scroll', 'key', 'text', 'media', 'ping']);
function send(obj) {
  const dc = obj.t === 'abs' ? moveDC : OVER_RTC.has(obj.t) ? inputDC : null;
  // While WebRTC is reconnecting (pc 'disconnected'), the WebSocket may still get through.
  if (dc?.readyState === 'open' && pc?.connectionState === 'connected') { dc.send(JSON.stringify(obj)); return; }
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
      error: (err) => { console.warn('decoder', err); resetDecoder(); send({ t: 'resend' }); },
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

// Frames are shown in step with the display's refresh: if two arrive within one
// refresh, only the newest is drawn. Keeps motion even and never queues up lag.
let pendingFrame = null;
let drawScheduled = false;

function drawFrame(frame) {
  pendingFrame?.close();
  pendingFrame = frame;
  if (!drawScheduled) {
    drawScheduled = true;
    requestAnimationFrame(paint);
  }
}

function paint() {
  drawScheduled = false;
  const frame = pendingFrame;
  pendingFrame = null;
  if (!frame) return;
  ctx2d.drawImage(frame, 0, 0, canvas.width, canvas.height);
  frame.close();
  stats.frames++;
  hasFrame = true;
  if (document.body.dataset.state !== 'on' || document.body.classList.contains('stale')) { setState('on', 'Connected'); drawCursor(); }
}

// ---------- WebRTC video ----------
// Same H.264, carried like AirPlay/FaceTime: UDP over the tailnet, a minimal jitter
// buffer, congestion control. Safari's hardware decoder plays it in a <video>. If it
// can't connect, video simply stays on the WebSocket.
const video = $('#video');
let pc = null;
let rtcTimer = 0;
let rtcStall = 0;
let rtcRetry = 0;
let rtcRetries = 0;
let inputDC = null;   // ordered, reliable
let moveDC = null;    // unordered, never resent

async function startRTC() {
  if (!window.RTCPeerConnection || store.get('transport', 'auto') === 'ws') return;
  stopRTC(false);
  try {
    pc = new RTCPeerConnection({ iceServers: [] });   // tailnet only: no STUN/TURN
    const tr = pc.addTransceiver('video', { direction: 'recvonly' });
    inputDC = pc.createDataChannel('input');
    moveDC = pc.createDataChannel('move', { ordered: false, maxRetransmits: 0 });
    for (const dc of [inputDC, moveDC]) dc.onmessage = (e) => onEvent(JSON.parse(e.data));   // pongs, cursor reports
    try {
      const codecs = RTCRtpReceiver.getCapabilities('video').codecs.filter((c) => /h264/i.test(c.mimeType));
      if (codecs.length) tr.setCodecPreferences(codecs);
    } catch {}
    pc.ontrack = (e) => {
      try { e.receiver.jitterBufferTarget = 0; } catch {}   // show frames as soon as they're decodable
      try { e.receiver.playoutDelayHint = 0; } catch {}
      video.srcObject = new MediaStream([e.track]);
      video.play().catch(() => {});
      if (video.requestVideoFrameCallback) video.requestVideoFrameCallback(onVideoFrame);
      else watchVideo();   // Firefox: no per-frame callback, so poll playback instead
    };
    pc.onconnectionstatechange = () => {
      if (!pc) return;
      clearTimeout(rtcStall);
      if (['failed', 'closed'].includes(pc.connectionState)) fallbackToWS();
      // 'disconnected' can recover on its own; if it doesn't soon, keep the picture moving over the WebSocket.
      else if (pc.connectionState === 'disconnected') rtcStall = setTimeout(fallbackToWS, 2000);
    };
    await pc.setLocalDescription(await pc.createOffer());
    await gathered(pc, 1000);
    send({ t: 'rtc-offer', sdp: pc.localDescription.sdp });
    rtcTimer = setTimeout(() => { if (!document.body.classList.contains('rtc')) fallbackToWS(); }, 6000);
  } catch {
    fallbackToWS();
  }
}

function gathered(conn, ms) {
  return new Promise((resolve) => {
    if (conn.iceGatheringState === 'complete') return resolve();
    const t = setTimeout(resolve, ms);
    conn.addEventListener('icegatheringstatechange', () => {
      if (conn.iceGatheringState === 'complete') { clearTimeout(t); resolve(); }
    });
  });
}

function fallbackToWS() {
  if (!pc) return;
  stopRTC(true);
  // Video carries on over the WebSocket; try WebRTC again a little later (backing off).
  clearTimeout(rtcRetry);
  if (rtcRetries < 4) rtcRetry = setTimeout(() => { if (ws?.readyState === 1) startRTC(); }, 5000 * 2 ** rtcRetries++);
}

function stopRTC(tellServer) {
  clearTimeout(rtcTimer);
  clearTimeout(rtcStall);
  inputDC = moveDC = null;
  if (pc) {
    pc.close();
    pc = null;
    if (tellServer) send({ t: 'rtc-off' });
  }
  video.srcObject = null;
  document.body.classList.remove('rtc');
}

// Browsers without requestVideoFrameCallback: detect the first frame and count frames by polling.
function watchVideo() {
  let seen = 0;
  const tick = () => {
    if (!video.srcObject) return;
    const q = video.getVideoPlaybackQuality?.();
    const total = q ? q.totalVideoFrames : (video.readyState >= 2 ? seen + 1 : seen);
    if (total > seen && video.videoWidth) {
      stats.frames += total - seen;
      seen = total;
      onVideoFrame(0, { width: video.videoWidth, height: video.videoHeight }, true);
    }
    setTimeout(tick, 100);
  };
  tick();
}

function onVideoFrame(now, meta, polled) {
  if (!video.srcObject) return;
  if (!polled) stats.frames++;
  if (canvas.width !== meta.width || canvas.height !== meta.height) {
    canvas.width = meta.width;   // canvas carries the stream size for layout, cursor and input maths
    canvas.height = meta.height;
    layout();
  }
  if (!document.body.classList.contains('rtc')) {
    document.body.classList.add('rtc');
    clearTimeout(rtcTimer);
    rtcRetries = 0;
  }
  hasFrame = true;
  if (document.body.dataset.state !== 'on' || document.body.classList.contains('stale')) { setState('on', 'Connected'); drawCursor(); }
  if (!polled) video.requestVideoFrameCallback(onVideoFrame);
}

function onEvent(ev) {
  switch (ev.t) {
    case 'started':
      screenPts.w = ev.ptW; screenPts.h = ev.ptH;
      shownDisplay = ev.display;
      markDisplays();
      drawCursor();
      break;
    case 'displays': displays = ev.list; markDisplays(); break;
    case 'cursor':
      // The Mac's position is the truth, but while you're moving it we trust our prediction.
      macCursor = ev;
      settleCursor();
      break;
    case 'cursorImage': setCursorImage(ev); break;
    case 'stats':
      stats.kbps = ev.kbps;
      stats.path = ev.path || '';
      document.body.classList.toggle('relayed', !!stats.path && stats.path !== 'direct');
      break;
    case 'pong': stats.rtt = Math.round(performance.now() - ev.at); break;
    case 'lock': setLocked(ev.locked); break;
    case 'rtc-answer': pc?.setRemoteDescription({ type: 'answer', sdp: ev.sdp }).catch(() => fallbackToWS()); break;
    case 'rtc-failed': stopRTC(false); break;   // the server has already switched back
    case 'error':
      if (locked) break;   // capture pauses while the Mac is locked; that's expected
      setState('off', ev.needsPermission
        ? 'Allow Screen Recording for mydevices-capture on the Mac (System Settings → Privacy & Security).'
        : `Mac: ${ev.msg}`);
      break;
    case 'warn':
      // Input blocked on the Mac: say so instead of letting the local cursor pretend.
      $('#toastText').textContent = 'Mac is blocking input. Allow mydevices-capture in Privacy & Security → Accessibility.';
      $('#toast').classList.remove('hidden');
      clearTimeout(onMacClipboard.t);
      break;
    case 'clip': onMacClipboard(ev); break;
  }
}

setInterval(() => {
  send({ t: 'ping', at: performance.now() });
  stats.fps = stats.frames; stats.frames = 0;
  if (document.body.dataset.state === 'on') {
    if (!document.body.classList.contains('stale')) {
      const relayed = stats.path && stats.path !== 'direct';
      $('#statusText').textContent = `${stats.rtt} ms${relayed ? ' · relayed' : ''}`;
      $('#status').classList.toggle('rtc', document.body.classList.contains('rtc'));
    }
    $('#status').title = `${stats.path || 'unknown path'} · ${stats.fps} fps · ${(stats.kbps / 1000).toFixed(1)} Mbps`;
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

// ---------- displays ----------
// A Mac with more than one display gets a picker in the panel. Switching is live: the
// stream restarts on the other display and picks up there without reconnecting.
let displays = [];
let shownDisplay = 0;

function displayLabel(d) {
  if (d.builtin) return 'Built-in';
  return d.name.length > 14 ? d.name.slice(0, 13) + '…' : d.name;
}

function markDisplays() {
  const seg = $('#displaySeg');
  $('#displayRow').classList.toggle('hidden', displays.length < 2);
  seg.replaceChildren(...displays.map((d) => {
    const b = document.createElement('button');
    b.textContent = displayLabel(d);
    b.title = `${d.name} · ${d.w}×${d.h}`;
    b.dataset.display = d.id;
    b.classList.toggle('on', d.id === shownDisplay);
    return b;
  }));
}

function chooseDisplay(id) {
  settings.display = id;
  store.set('display', id);
  shownDisplay = id;
  markDisplays();
  view.z = 1;
  sendStart();   // the Mac switches capture, input and cursor over to that display
}

// ---------- view: fit, zoom and pan ----------
const view = { z: 1, x: 0, y: 0 };   // zoom factor and top-left offset in CSS px
const cursor = { x: 0.5, y: 0.5 };   // remote cursor, normalised 0..1
const screenPts = { w: 1440, h: 900 }; // the Mac's screen size in points
const cursorShape = { w: 0, h: 0, hx: 0, hy: 0 };
let lastLocalMove = 0;
let macCursor = null;   // latest position reported by the Mac
let settleTimer = 0;

// The Mac's position is the truth, but while you're moving we trust our prediction.
// The latest report is kept and applied once movement settles, so we never miss the final one.
function settleCursor() {
  clearTimeout(settleTimer);
  if (!macCursor) return;
  const wait = 250 - (performance.now() - lastLocalMove);
  if (wait > 0) { settleTimer = setTimeout(settleCursor, wait + 5); return; }
  cursor.x = macCursor.x; cursor.y = macCursor.y;
  macCursor = null;
  drawCursor();
  followCursor();
}
const cursorEl = document.createElement('img');
cursorEl.id = 'cursor';
cursorEl.alt = '';
document.body.append(cursorEl);

// ---------- local cursor ----------
// The pointer is drawn here, not in the video, so it moves the instant your finger does.
function setCursorImage(ev) {
  Object.assign(cursorShape, { w: ev.w, h: ev.h, hx: ev.hx, hy: ev.hy });
  cursorEl.src = 'data:image/png;base64,' + ev.png;
  drawCursor();
}

function drawCursor() {
  if (!canvas.width || !cursorShape.w) return;
  // The Mac's pointer is on another display: nothing to draw here.
  cursorEl.style.visibility = cursor.x < 0 || cursor.x > 1 || cursor.y < 0 || cursor.y > 1 ? 'hidden' : '';
  const s = baseScale() * view.z;
  const ptToPx = (canvas.width / screenPts.w) * s;   // Mac points → CSS px on this screen
  const x = view.x + cursor.x * canvas.width * s - cursorShape.hx * ptToPx;
  const y = view.y + cursor.y * canvas.height * s - cursorShape.hy * ptToPx;
  cursorEl.style.width = `${cursorShape.w * ptToPx}px`;
  cursorEl.style.height = `${cursorShape.h * ptToPx}px`;
  cursorEl.style.transform = `translate3d(${x}px, ${y}px, 0)`;
}

// Move the local cursor right away (trackpad: relative in Mac points; touch/mouse: absolute).
function predictRel(dx, dy) {
  cursor.x = Math.min(1, Math.max(0, cursor.x + dx / screenPts.w));
  cursor.y = Math.min(1, Math.max(0, cursor.y + dy / screenPts.h));
  lastLocalMove = performance.now();
  drawCursor();
  followCursor();
  settleCursor();
}
function predictAbs(p) {
  cursor.x = p.x; cursor.y = p.y;
  lastLocalMove = performance.now();
  drawCursor();
  settleCursor();
}

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
  video.style.width = `${canvas.width}px`;
  video.style.height = `${canvas.height}px`;
  video.style.transform = canvas.style.transform;
  placeSpaceArrows(w);
  drawCursor();
}

// Put the ‹ › Space arrows in the empty side margins when there is room, else at the edges.
function placeSpaceArrows(shownWidth) {
  const gutter = view.z === 1 ? (innerWidth - shownWidth) / 2 : 0;
  const roomy = gutter >= 52;
  document.body.classList.toggle('roomy', roomy);
  const left = roomy ? Math.max(4, gutter / 2 - 22) : 6;
  $('#spacePrev').style.left = `calc(${left}px + env(safe-area-inset-left, 0px) * ${roomy ? 0 : 1})`;
  $('#spaceNext').style.right = `calc(${left}px + env(safe-area-inset-right, 0px) * ${roomy ? 0 : 1})`;
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
// A click aimed at a spot (touch mode) carries that spot, so it can't land anywhere else.
let lastClick = { at: 0, n: 0 };
function click(button = 0, at) {
  const now = performance.now();
  const n = button === 0 && now - lastClick.at < 400 ? Math.min(3, lastClick.n + 1) : 1;
  lastClick = { at: now, n };
  send({ t: 'click', b: button, n, m: mods(), ...at });
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

// Trackpad-style scrolling: the Mac gets gesture phases while fingers are down, then a
// decaying momentum tail after they lift, so apps glide and rubber-band natively.
let momentumRaf = 0;
function startMomentum(vx, vy) {   // px per ms
  if (Math.hypot(vx, vy) < 0.2) return;
  let last = performance.now(), first = true;
  const step = (now) => {
    const dt = Math.min(34, now - last);
    last = now;
    send({ t: 'scroll', dx: vx * dt, dy: vy * dt, mo: first ? 'begin' : 'continue' });
    first = false;
    const decay = Math.pow(0.9965, dt);   // close to iOS's normal deceleration
    vx *= decay; vy *= decay;
    if (Math.hypot(vx, vy) < 0.02) { momentumRaf = 0; send({ t: 'scroll', dx: 0, dy: 0, mo: 'end' }); return; }
    momentumRaf = requestAnimationFrame(step);
  };
  momentumRaf = requestAnimationFrame(step);
}
function stopMomentum() {
  if (!momentumRaf) return;
  cancelAnimationFrame(momentumRaf);
  momentumRaf = 0;
  send({ t: 'scroll', dx: 0, dy: 0, mo: 'end' });
}

surface.addEventListener('touchstart', (e) => {
  e.preventDefault();
  stopMomentum();   // touching again catches the glide, like on a real trackpad
  const t = touchInfo(e.touches);
  const now = performance.now();
  if (!g.active) {
    Object.assign(g, {
      active: true, start: now, maxN: t.n, moved: false, mode: null,
      sx: t.x, sy: t.y, lx: t.x, ly: t.y, lt: now,
      tapDrag: settings.mode === 'trackpad' && now - (g.lastTapEnd || 0) < 280, lastP: null,
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
    click(1, toRemote(g.sx, g.sy));
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
        predictRel(dx * gain, dy * gain);
      }
    } else if (g.moved && g.mode !== 'done') {
      const p = toRemote(t.x, t.y);
      if (!g.mode) {
        send({ t: 'btn', b: 0, down: true, n: 1, m: mods(), ...toRemote(g.sx, g.sy) });
        g.mode = 'drag';
      }
      send({ t: 'abs', x: p.x, y: p.y });
      predictAbs(p);
      g.lastP = p;
    }
  } else if (t.n === 2) {
    if (!g.mode) {
      const ratio = t.dist / (g.sdist || 1);
      if (Math.abs(ratio - 1) > 0.12) g.mode = 'pinch';
      else if (Math.hypot(t.x - g.sx, t.y - g.sy) > MOVE_SLOP) {
        g.mode = 'scroll';
        g.samples = [];
        send({ t: 'scroll', dx: 0, dy: 0, ph: 'begin' });
      }
    }
    if (g.mode === 'scroll') {
      const k = SCROLL_GAIN * (settings.natural ? 1 : -1);
      send({ t: 'scroll', dx: dx * k, dy: dy * k, ph: 'change' });
      g.samples.push({ t: now, dx: dx * k, dy: dy * k });
      while (g.samples.length && now - g.samples[0].t > 100) g.samples.shift();
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

  if (g.mode === 'scroll') {
    send({ t: 'scroll', dx: 0, dy: 0, ph: 'end' });
    // Fling velocity from the last ~100 ms of movement.
    const smp = g.samples.filter((p) => performance.now() - p.t < 120);
    if (smp.length >= 2) {
      const span = Math.max(16, smp[smp.length - 1].t - smp[0].t);
      const sx = smp.reduce((a, p) => a + p.dx, 0), sy = smp.reduce((a, p) => a + p.dy, 0);
      startMomentum(sx / span, sy / span);
    }
  } else if (g.mode === 'drag') {
    send({ t: 'btn', b: 0, down: false, n: 1, ...(settings.mode === 'touch' && g.lastP) });
  } else if (g.maxN === 1 && !g.moved && !g.mode && dur < LONG_PRESS_MS) {
    if (settings.mode === 'touch') {
      const p = toRemote(g.sx, g.sy);
      predictAbs(p);
      click(0, p);
    } else click(0);
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
surface.addEventListener('touchcancel', (e) => {
  e.preventDefault();
  if (!g.active) return;
  clearTimeout(g.longTimer);
  if (g.mode === 'scroll') send({ t: 'scroll', dx: 0, dy: 0, ph: 'cancel' });
  else if (g.mode === 'drag') send({ t: 'btn', b: 0, down: false, n: 1, ...(settings.mode === 'touch' && g.lastP) });
  g.active = false;
}, { passive: false });
document.addEventListener('gesturestart', (e) => e.preventDefault());

// ---------- real mouse / trackpad (iPad with trackpad, laptops) ----------
surface.addEventListener('pointermove', (e) => {
  if (e.pointerType !== 'mouse') return;
  const p = toRemote(e.clientX, e.clientY);
  send({ t: 'abs', x: p.x, y: p.y });
  predictAbs(p);
});
surface.addEventListener('pointerdown', (e) => {
  if (e.pointerType !== 'mouse') return;
  surface.setPointerCapture(e.pointerId);
  send({ t: 'btn', b: e.button === 2 ? 1 : e.button === 1 ? 2 : 0, down: true, n: Math.max(1, e.detail), m: physicalMods(e), ...toRemote(e.clientX, e.clientY) });
});
surface.addEventListener('pointerup', (e) => {
  if (e.pointerType !== 'mouse') return;
  send({ t: 'btn', b: e.button === 2 ? 1 : e.button === 1 ? 2 : 0, down: false, n: Math.max(1, e.detail), m: physicalMods(e), ...toRemote(e.clientX, e.clientY) });
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
  if (e.target.closest?.('#more, #unlock')) return;   // typing into our own panel or the unlock field
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

// ---------- no page zoom ----------
// iOS Safari ignores user-scalable=no, so a pinch on the toolbar or menu would zoom the
// whole page and leave it stuck. Block that; pinch only ever zooms the Mac screen.
for (const t of ['gesturestart', 'gesturechange', 'gestureend']) {
  document.addEventListener(t, (e) => e.preventDefault(), { passive: false });
}
document.addEventListener('touchmove', (e) => {
  if (e.touches.length > 1 || (e.scale && e.scale !== 1)) e.preventDefault();
}, { passive: false });
let lastTouchEnd = 0;
document.addEventListener('touchend', (e) => {   // no double-tap-to-zoom on buttons
  const now = performance.now();
  if (now - lastTouchEnd < 320 && e.target.closest('button, #more')) e.preventDefault();
  lastTouchEnd = now;
}, { passive: false });

// If the page did get zoomed somehow, snap it back.
function resetPageZoom() {
  if (!window.visualViewport || visualViewport.scale <= 1.01) return;
  const m = document.querySelector('meta[name=viewport]');
  const c = m.content;
  m.content = c + ', minimum-scale=1';
  requestAnimationFrame(() => { m.content = c; });
}
window.visualViewport?.addEventListener('resize', () => setTimeout(resetPageZoom, 300));

// ---------- icons ----------
// One line-icon set (24px grid, 1.75 stroke), injected wherever data-icon is set.
const ICON_PATHS = {
  keyboard:     '<rect x="2.5" y="6" width="19" height="12" rx="2.5"/><path d="M6.5 10h.01M10 10h.01M14 10h.01M17.5 10h.01M8 14h8"/>',
  trackpad:     '<rect x="3.5" y="4" width="17" height="16" rx="3"/><path d="M3.5 15.5h17M12 15.5V20"/>',
  touch:        '<path d="M9 11.5V5.5a1.5 1.5 0 0 1 3 0V11m0-1.5a1.5 1.5 0 0 1 3 0V11m0-.5a1.5 1.5 0 0 1 3 0V15a6 6 0 0 1-6 6h-.6a6 6 0 0 1-4.6-2.2L4.5 15.6a1.5 1.5 0 0 1 2.3-1.9L9 16"/>',
  more:         '<circle cx="5.5" cy="12" r="1.2" fill="currentColor"/><circle cx="12" cy="12" r="1.2" fill="currentColor"/><circle cx="18.5" cy="12" r="1.2" fill="currentColor"/>',
  arrowLeft:    '<path d="M19 12H5M11 6l-6 6 6 6"/>',
  arrowRight:   '<path d="M5 12h14M13 6l6 6-6 6"/>',
  arrowUp:      '<path d="M12 19V5M6 11l6-6 6 6"/>',
  arrowDown:    '<path d="M12 5v14M6 13l6 6 6-6"/>',
  chevronLeft:  '<path d="M14.5 5.5L8 12l6.5 6.5"/>',
  chevronRight: '<path d="M9.5 5.5L16 12l-6.5 6.5"/>',
  mission:      '<rect x="3" y="4.5" width="7.5" height="6" rx="1.5"/><rect x="13.5" y="4.5" width="7.5" height="6" rx="1.5"/><rect x="3" y="13.5" width="7.5" height="6" rx="1.5"/><rect x="13.5" y="13.5" width="7.5" height="6" rx="1.5"/>',
  search:       '<circle cx="10.5" cy="10.5" r="6"/><path d="M15 15l5 5"/>',
  apps:         '<rect x="3" y="7" width="12" height="11" rx="2"/><path d="M8 4h11a2 2 0 0 1 2 2v9"/>',
  desktop:      '<rect x="2.5" y="4" width="19" height="13" rx="2"/><path d="M8 20.5h8"/>',
  camera:       '<path d="M3.5 8.5A1.5 1.5 0 0 1 5 7h2.2l1.6-2.2h6.4L16.8 7H19a1.5 1.5 0 0 1 1.5 1.5V18A1.5 1.5 0 0 1 19 19.5H5A1.5 1.5 0 0 1 3.5 18z"/><circle cx="12" cy="13" r="3.5"/>',
  xCircle:      '<circle cx="12" cy="12" r="8.5"/><path d="M9 9l6 6M15 9l-6 6"/>',
  paste:        '<rect x="5" y="4.5" width="14" height="16" rx="2"/><path d="M9 4.5V3.5h6v1"/>',
  copy:         '<rect x="8.5" y="8.5" width="11.5" height="11.5" rx="2"/><path d="M15.5 8.5V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v7.5a2 2 0 0 0 2 2h2.5"/>',
  lock:         '<rect x="5" y="10.5" width="14" height="10" rx="2.5"/><path d="M8.5 10.5V7.5a3.5 3.5 0 0 1 7 0v3"/>',
  prev:         '<path d="M18 6.5v11L10 12zM6 6v12"/>',
  play:         '<path d="M5 6v12l7-6zM15 6.5v11M19 6.5v11"/>',
  next:         '<path d="M6 6.5v11l8-5.5zM18 6v12"/>',
  volDown:      '<path d="M4 9.5h3.5L12 6v12l-4.5-3.5H4zM16 10a3 3 0 0 1 0 4"/>',
  volUp:        '<path d="M4 9.5h3.5L12 6v12l-4.5-3.5H4zM15.5 9.5a3.5 3.5 0 0 1 0 5M18 7a7 7 0 0 1 0 10"/>',
  mute:         '<path d="M4 9.5h3.5L12 6v12l-4.5-3.5H4zM16 10l4 4M20 10l-4 4"/>',
  sunDim:       '<circle cx="12" cy="12" r="3.5"/><path d="M12 5.5v.01M12 18.5v.01M5.5 12h.01M18.5 12h.01M7.4 7.4h.01M16.6 16.6h.01M7.4 16.6h.01M16.6 7.4h.01"/>',
  sun:          '<circle cx="12" cy="12" r="3.5"/><path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6L7 7M17 17l1.4 1.4M5.6 18.4L7 17M17 7l1.4-1.4"/>',
  x:            '<path d="M7 7l10 10M17 7L7 17"/>',
};
function icon(name) {
  return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICON_PATHS[name] || ''}</svg>`;
}
document.querySelectorAll('[data-icon]').forEach((el) => el.insertAdjacentHTML('afterbegin', icon(el.dataset.icon)));

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
  more: () => openSheet($('#more').classList.contains('hidden')),
  close: () => openSheet(false),
  mode: () => setMode(settings.mode === 'trackpad' ? 'touch' : 'trackpad'),
  showBar: () => document.body.classList.remove('bar-hidden'),
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
  fit: () => fitMode('fit'),
  fillScreen: () => fitMode('fill'),
};

function setMode(m) {
  settings.mode = m;
  store.set('mode', m);
  document.body.dataset.mode = m;
  $('#modeBtn').innerHTML = icon(m === 'trackpad' ? 'trackpad' : 'touch');
  markSegments();
}

function openSheet(open) {
  $('#more').classList.toggle('hidden', !open);
  $('#backdrop').classList.toggle('hidden', !open);
  document.body.classList.toggle('sheet-open', open);
  if (open) kbd.blur();
  else resetPageZoom();
}

document.addEventListener('click', (e) => {
  const b = e.target.closest('button, [data-act]');
  if (!b) return;
  const d = b.dataset;
  if ('close' in d && b.closest('#more')) openSheet(false);
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
// Segmented controls: <button data-set="quality" data-value="auto">
function markSegments() {
  document.querySelectorAll('[data-set]').forEach((b) =>
    b.classList.toggle('on', String(settings[b.dataset.set]) === b.dataset.value));
}
document.addEventListener('click', (e) => {
  const d = e.target.closest('[data-display]');
  if (d && +d.dataset.display !== shownDisplay) chooseDisplay(+d.dataset.display);
});
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-set]');
  if (!b) return;
  const key = b.dataset.set, v = b.dataset.value;
  if (key === 'mode') return setMode(v);
  if (String(settings[key]) === v) return;
  settings[key] = key === 'fps' ? +v : v;
  store.set(key, settings[key]);
  markSegments();
  ws?.close();   // reconnect with the new stream settings
});
$('#speed').value = settings.speed;
$('#natural').checked = settings.natural;
$('#speed').addEventListener('input', (e) => { settings.speed = +e.target.value; store.set('speed', settings.speed); });
$('#natural').addEventListener('change', (e) => { settings.natural = e.target.checked; store.set('natural', settings.natural); });

setMode(settings.mode);
boot();
