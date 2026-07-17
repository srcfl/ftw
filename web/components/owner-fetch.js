// Shared fetch helper for dashboard components. The complete dashboard is
// LAN-local, so API requests use the browser's normal same-origin transport.
export function ownerFetch(path, opts) {
  return fetch(path, opts);
}
