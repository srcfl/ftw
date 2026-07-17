// Shared local API accessor for web components.
export function apiFetch(path, opts) {
  return fetch(path, opts);
}
