// PAD Analyzer service worker — offline shell + smart caching for web mode.
//
// Strategy:
//   - Navigations (SPA route loads): network-first → cached shell → the
//     precached /offline.html page, so a cold start with the backend down
//     still renders something useful instead of the browser error page.
//   - Hashed build assets (/assets/*): cache-first. Vite content-hashes
//     these files, so a cached copy is immutable and safe to serve forever.
//   - API requests (/api/*): network-only. The backend stamps every API
//     response `Cache-Control: no-store, private` (sensitive, per-user JSON
//     such as /api/auth/me, flows, findings, chat); persisting those in
//     Cache Storage would outlive logout on a shared machine. Offline API
//     calls fail with a 503 JSON error the app already handles.
//   - SSE (/api/events or any text/event-stream request): never intercepted —
//     caching or buffering an event stream breaks live chat/AI streaming.
//   - Cross-origin (e.g. CDN fonts): pass-through, no caching.
//   - PURGE_CACHES message: the app posts it on logout so nothing — not even
//     the shell — survives a user session on a shared device.
//
// Only registered in web mode (not Tauri, which serves from localhost sidecar).

const SHELL_CACHE = 'baki-shell-v3';
const ASSET_CACHE = 'baki-assets-v3';
const KNOWN_CACHES = [SHELL_CACHE, ASSET_CACHE];
const OFFLINE_URL = '/offline.html';
const API_PREFIX = '/api/';

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches
      .open(SHELL_CACHE)
      .then((c) => c.add(OFFLINE_URL))
      .catch(() => {})
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      // Dropping a cache name from KNOWN_CACHES (e.g. the removed baki-api-v2)
      // purges it — and any sensitive responses it may already hold — on upgrade.
      .then((keys) =>
        Promise.all(keys.filter((k) => !KNOWN_CACHES.includes(k)).map((k) => caches.delete(k))),
      )
      .then(() => self.clients.claim()),
  );
});

// Logout (and session teardown) asks the worker to drop every cache so a
// subsequent user on a shared device can't recover anything from storage.
self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'PURGE_CACHES') {
    // waitUntil keeps the SW alive until the deletions finish — without it the
    // worker can be terminated mid-purge, leaving cached entries behind.
    event.waitUntil(
      caches
        .keys()
        .then((keys) => Promise.all(keys.map((k) => caches.delete(k))))
        .catch(() => {}),
    )
  }
});

function offlineApiResponse() {
  return new Response('{"error":"offline"}', {
    status: 503,
    headers: {'Content-Type': 'application/json'},
  });
}

// Navigations: try the network for a fresh shell, then the cache, then the
// static offline page (always precached at install).
function handleNavigation(req) {
  return fetch(req)
    .then((res) => {
      if (res.ok) {
        caches
          .open(SHELL_CACHE)
          .then((c) => c.put('/', res.clone()))
          .catch(() => {});
      }
      return res;
    })
    .catch(() =>
      caches
        .open(SHELL_CACHE)
        .then((c) => c.match('/') || c.match(OFFLINE_URL))
        .then((cached) => cached || Response.error()),
    );
}

// Immutable hashed assets: cache-first; populate on miss.
function handleAsset(req) {
  return caches.open(ASSET_CACHE).then(async (cache) => {
    const cached = await cache.match(req);
    if (cached) return cached;
    const res = await fetch(req);
    if (res.ok) cache.put(req, res.clone()).catch(() => {});
    return res;
  });
}

// API: network-only (responses are no-store/private — never persisted).
function handleApi(req) {
  return fetch(req).catch(() => offlineApiResponse());
}

// Everything else same-origin (root static files, fonts, manifest): stale-
// while-revalidate.
function handleStatic(req) {
  return caches.open(SHELL_CACHE).then(async (cache) => {
    const cached = await cache.match(req);
    const fetchPromise = fetch(req)
      .then((res) => {
        if (res.ok) cache.put(req, res.clone()).catch(() => {});
        return res;
      })
      .catch(() => cached);
    return cached || fetchPromise;
  });
}

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return; // cross-origin: pass-through

  // Never intercept the SSE stream (or any event-stream request): buffering
  // it in respondWith/cache breaks incremental chat/AI chunks.
  const accept = req.headers.get('accept') || '';
  if (url.pathname === '/api/events' || accept.includes('text/event-stream')) return;

  if (req.mode === 'navigate') {
    event.respondWith(handleNavigation(req));
    return;
  }

  if (url.pathname.startsWith('/assets/')) {
    event.respondWith(handleAsset(req));
    return;
  }

  if (url.pathname.startsWith(API_PREFIX)) {
    event.respondWith(handleApi(req));
    return;
  }

  event.respondWith(handleStatic(req));
});
