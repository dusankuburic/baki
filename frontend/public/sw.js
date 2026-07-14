// PAD Analyzer service worker — runtime caching for offline shell loading.
//
// Strategy:
//   - Same-origin GET requests for assets (JS/CSS/HTML/fonts): stale-while-
//     revalidate (serve from cache immediately, fetch a fresh copy in the
//     background). This lets the SPA load even when the backend is unreachable.
//   - API requests (/api/*): network-first, fall back to cache on failure.
//     API responses are dynamic (findings, flows) so we prefer fresh data but
//     serve stale on network error so the user sees SOMETHING offline.
//   - Cross-origin (e.g. CDN fonts): pass-through, no caching.
//
// Only registered in web mode (not Tauri, which serves from localhost sidecar).

const CACHE = 'baki-shell-v1';
const API_PREFIX = '/api/';

self.addEventListener('install', (event) => {
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))),
    ),
  );
  self.clients.claim();
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return; // cross-origin: pass-through

  if (url.pathname.startsWith(API_PREFIX)) {
    // Network-first for API; fall back to cache on failure.
    event.respondWith(
      fetch(req)
        .then((res) => {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
          return res;
        })
        .catch(() => caches.match(req).then((cached) => cached || new Response('{"error":"offline"}', {status: 503, headers: {'Content-Type': 'application/json'}}))),
    );
    return;
  }

  // Stale-while-revalidate for same-origin assets.
  event.respondWith(
    caches.open(CACHE).then(async (cache) => {
      const cached = await cache.match(req);
      const fetchPromise = fetch(req)
        .then((res) => {
          if (res.ok) cache.put(req, res.clone());
          return res;
        })
        .catch(() => cached);
      return cached || fetchPromise;
    }),
  );
});
