// Mac Remote — a small, hackable front-end on top of noVNC's RFB client.
// noVNC docs: web/novnc/docs/API.md
import RFB from './novnc/core/rfb.js';
import KeyTable from './novnc/core/input/keysym.js';
import keysyms from './novnc/core/input/keysymdef.js';

// ---------- config you might want to tweak ----------
const CONFIG = {
  wsPath: 'websockify',     // websockify endpoint, same host as this page
  defaultQuality: 6,        // 0 (fast/ugly) .. 9 (sharp)
  compression: 2,           // 0..9
  // macOS Screen Sharing treats Super as ⌘. If ⌘ shortcuts don't work, try XK_Meta_L.
  cmdKeysym: KeyTable.XK_Super_L,
};

const MODS = {
  cmd:   [CONFIG.cmdKeysym,  'MetaLeft'],
  opt:   [KeyTable.XK_Alt_L, 'AltLeft'],
  ctrl:  [KeyTable.XK_Control_L, 'ControlLeft'],
  shift: [KeyTable.XK_Shift_L, 'ShiftLeft'],
};

const NAMED_KEYS = {
  esc: KeyTable.XK_Escape, tab: KeyTable.XK_Tab, return: KeyTable.XK_Return,
  backspace: KeyTable.XK_BackSpace, delete: KeyTable.XK_Delete, space: KeyTable.XK_space,
  left: KeyTable.XK_Left, right: KeyTable.XK_Right, up: KeyTable.XK_Up, down: KeyTable.XK_Down,
  home: KeyTable.XK_Home, end: KeyTable.XK_End, pageup: KeyTable.XK_Page_Up, pagedown: KeyTable.XK_Page_Down,
};

// ---------- tiny helpers ----------
const $ = (s) => document.querySelector(s);
const store = {
  get: (k, d) => { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } },
  set: (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch {} },
};

// Device name comes from the URL: /mac/ → "mac"
const DEVICE = location.pathname.split('/').filter(Boolean)[0] || location.hostname.split('.')[0];

let rfb = null;
let creds = null;
const sticky = new Set();   // modifiers that apply to the next key press

function setStatus(text, state) {
  $('#statusText').textContent = text;
  document.body.dataset.state = state;   // 'off' | 'connecting' | 'on' — styled in app.css
}

// ---------- connect / disconnect ----------
function connect() {
  // Relative to the page, so it works at /, /mac/, /mac2/ …
  const url = new URL(CONFIG.wsPath, location.href);
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  setStatus('Connecting…', 'connecting');
  rfb = new RFB($('#screen'), url.href, { credentials: creds });
  rfb.scaleViewport = store.get('fit', true);
  rfb.clipViewport = !rfb.scaleViewport;
  rfb.viewOnly = store.get('viewOnly', false);
  rfb.qualityLevel = store.get('quality', CONFIG.defaultQuality);
  rfb.compressionLevel = CONFIG.compression;
  rfb.focusOnClick = false;   // we drive the soft keyboard ourselves

  rfb.addEventListener('connect', () => {
    setStatus('Connected', 'on');
    store.set('user', creds.username);
    document.title = `${DEVICE} — Mac Remote`;
  });
  rfb.addEventListener('disconnect', (e) => {
    setStatus(e.detail.clean ? 'Disconnected' : 'Connection lost', 'off');
    rfb = null;
  });
  rfb.addEventListener('credentialsrequired', () => {
    // Wrong or missing credentials — go back to the login sheet
    rfb.disconnect();
    $('#loginMsg').textContent = 'Username and password are required';
  });
  rfb.addEventListener('securityfailure', (e) => {
    $('#loginMsg').textContent = `Login failed: ${e.detail.reason || 'check username/password'}`;
  });
  rfb.addEventListener('clipboard', (e) => { $('#clipText').value = e.detail.text; });
}

$('#login').addEventListener('submit', (e) => {
  e.preventDefault();
  creds = { username: $('#user').value.trim(), password: $('#pass').value };
  connect();
});
$('#user').value = store.get('user', '');
$('#loginTitle').textContent = DEVICE;

// ---------- keys ----------
function press(keysym, code = '') {
  if (!rfb) return;
  const mods = [...sticky];
  mods.forEach((m) => rfb.sendKey(MODS[m][0], MODS[m][1], true));
  rfb.sendKey(keysym, code);   // down + up
  mods.reverse().forEach((m) => rfb.sendKey(MODS[m][0], MODS[m][1], false));
  clearSticky();
}

function pressCombo(combo) {          // e.g. "cmd+shift+3"
  const parts = combo.toLowerCase().split('+');
  const key = parts.pop();
  parts.forEach((m) => sticky.add(m));
  press(NAMED_KEYS[key] ?? keysyms.lookup(key.codePointAt(0)));
}

function typeText(text) {
  for (const ch of text) {
    if (ch === '\n') press(KeyTable.XK_Return, 'Enter');
    else press(keysyms.lookup(ch.codePointAt(0)));
  }
}

function clearSticky() {
  sticky.clear();
  document.querySelectorAll('[data-mod]').forEach((b) => b.classList.remove('active'));
}

// Soft keyboard: a hidden textarea that we keep empty and translate into key presses
const kbd = $('#kbd');
kbd.addEventListener('beforeinput', (e) => {
  e.preventDefault();
  switch (e.inputType) {
    case 'insertText':
    case 'insertReplacementText':
      typeText(e.data ?? e.dataTransfer?.getData('text/plain') ?? ''); break;
    case 'insertLineBreak':
    case 'insertParagraph':
      press(KeyTable.XK_Return, 'Enter'); break;
    case 'deleteContentBackward':
      press(KeyTable.XK_BackSpace, 'Backspace'); break;
    case 'deleteWordBackward':
      sticky.add('opt'); press(KeyTable.XK_BackSpace, 'Backspace'); break;
  }
});
kbd.addEventListener('focus', () => document.body.classList.add('kbd-open'));
kbd.addEventListener('blur', () => document.body.classList.remove('kbd-open'));

// Keep the toolbar above the on-screen keyboard (iOS/iPadOS)
if (window.visualViewport) {
  const fit = () => {
    const vv = window.visualViewport;
    document.documentElement.style.setProperty('--kb-offset', `${Math.max(0, window.innerHeight - vv.height - vv.offsetTop)}px`);
  };
  visualViewport.addEventListener('resize', fit);
  visualViewport.addEventListener('scroll', fit);
}

// ---------- buttons ----------
const actions = {
  keyboard: () => (document.activeElement === kbd ? kbd.blur() : kbd.focus()),
  more: () => $('#more').classList.toggle('hidden'),
  close: () => $('#more').classList.add('hidden'),
  disconnect: () => { rfb?.disconnect(); $('#more').classList.add('hidden'); },
  fullscreen: () => (document.fullscreenElement ? document.exitFullscreen() : document.documentElement.requestFullscreen?.()),
  paste: () => typeText($('#clipText').value),
};

document.addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b) return;
  if (b.dataset.act) actions[b.dataset.act]?.();
  if (b.dataset.key) press(NAMED_KEYS[b.dataset.key]);
  if (b.dataset.combo) pressCombo(b.dataset.combo);
  if (b.dataset.mod) {
    const m = b.dataset.mod;
    sticky.has(m) ? sticky.delete(m) : sticky.add(m);
    b.classList.toggle('active', sticky.has(m));
  }
});
// Toolbar buttons shouldn't steal focus from the soft keyboard
document.querySelectorAll('#bar button, #keys button').forEach((b) =>
  b.addEventListener('mousedown', (e) => e.preventDefault()));

$('#status').addEventListener('click', () => document.body.classList.toggle('bar-hidden'));

// ---------- settings ----------
$('#quality').value = store.get('quality', CONFIG.defaultQuality);
$('#quality').addEventListener('input', (e) => {
  store.set('quality', +e.target.value);
  if (rfb) rfb.qualityLevel = +e.target.value;
});
$('#fit').checked = store.get('fit', true);
$('#fit').addEventListener('change', (e) => {
  store.set('fit', e.target.checked);
  if (rfb) { rfb.scaleViewport = e.target.checked; rfb.clipViewport = !e.target.checked; }
});
$('#viewOnly').checked = store.get('viewOnly', false);
$('#viewOnly').addEventListener('change', (e) => {
  store.set('viewOnly', e.target.checked);
  if (rfb) rfb.viewOnly = e.target.checked;
});

setStatus('Disconnected', 'off');
