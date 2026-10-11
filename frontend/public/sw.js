
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
      .then((keys) =>
        Promise.all(keys.filter((k) => !KNOWN_CACHES.includes(k)).map((k) => caches.delete(k))),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'PURGE_CACHES') {
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

const ASSET_RETRY_DELAYS_MS = [250, 1000];

async function fetchAsset(req) {
  for (let attempt = 0; ; attempt++) {
    try {
      return await fetch(req);
    } catch (err) {
      if (attempt >= ASSET_RETRY_DELAYS_MS.length || (req.signal && req.signal.aborted)) throw err;
      await new Promise((resolve) => setTimeout(resolve, ASSET_RETRY_DELAYS_MS[attempt]));
    }
  }
}

function handleAsset(req) {
  return caches.open(ASSET_CACHE).then(async (cache) => {
    const cached = await cache.match(req);
    if (cached) return cached;
    let res;
    try {
      res = await fetchAsset(req);
    } catch {
      return Response.error();
    }
    if (res.ok) cache.put(req, res.clone()).catch(() => {});
    return res;
  });
}

function handleApi(req) {
  return fetch(req).catch(() => offlineApiResponse());
}

function handleStatic(req) {
  return caches.open(SHELL_CACHE).then(async (cache) => {
    const cached = await cache.match(req);
    const fetchPromise = fetch(req)
      .then((res) => {
        if (res.ok) cache.put(req, res.clone()).catch(() => {});
        return res;
      })
      .catch(() => cached || Response.error());
    return cached || fetchPromise;
  });
}

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

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
