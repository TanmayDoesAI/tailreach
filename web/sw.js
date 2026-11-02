// tailreach's service worker: one job only. When the Mac can't be reached at all (asleep,
// offline, or this device's Tailscale is off), a page load shows offline.html instead of
// the browser's generic error. Nothing else is cached or touched: never a signed-in page,
// never an API reply, and WebSockets don't pass through service workers at all.
const VERSION = 'tailreach-offline-1'; // bump to replace the cached page
const OFFLINE = ['/offline.html', '/icons/icon-192.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(VERSION).then((c) => c.addAll(OFFLINE.map((u) => new Request(u, { cache: 'reload' })))).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil((async () => {
    for (const k of await caches.keys()) if (k !== VERSION) await caches.delete(k);
    // The browser makes page loads itself (with its own cookies, redirects and all) and
    // hands us the result: we only ever step in when that fails.
    if (self.registration.navigationPreload) await self.registration.navigationPreload.enable();
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;
  if (req.mode === 'navigate') {
    e.respondWith((async () => {
      try {
        return (await e.preloadResponse) || (await fetch(req));
      } catch {
        return (await caches.match('/offline.html')) || Response.error();
      }
    })());
    return;
  }
  // The offline page's own icon: the network first, the cached copy if it's down.
  const url = new URL(req.url);
  if (url.origin === location.origin && OFFLINE.includes(url.pathname)) {
    e.respondWith(fetch(req).catch(async () => (await caches.match(url.pathname)) || Response.error()));
  }
});
