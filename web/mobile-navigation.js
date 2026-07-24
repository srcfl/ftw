export function shouldResetMobileScroll(sourceId, currentView, nextView) {
  return sourceId === "mobile-destinations" &&
    Boolean(nextView) &&
    currentView !== nextView;
}

export function initMobileDestinationScroll(root = document, viewport = window) {
  const nav = root.getElementById("mobile-destinations");
  if (!nav) return () => {};

  let pendingView = "";

  const onClick = (event) => {
    const target = event.target;
    const button = target && typeof target.closest === "function"
      ? target.closest(".app-nav-btn[data-view]")
      : null;
    if (!button || !nav.contains(button)) return;

    const currentView = root.body && root.body.dataset.view;
    const nextView = button.dataset.view;
    if (!shouldResetMobileScroll(nav.id, currentView, nextView)) return;
    pendingView = nextView;
  };

  const onHashChange = () => {
    if (!pendingView) return;
    const expectedView = pendingView;
    pendingView = "";
    viewport.requestAnimationFrame(() => {
      if (!root.body || root.body.dataset.view !== expectedView) return;
      viewport.scrollTo({ top: 0, left: 0, behavior: "auto" });
    });
  };

  nav.addEventListener("click", onClick);
  viewport.addEventListener("hashchange", onHashChange);

  return () => {
    nav.removeEventListener("click", onClick);
    viewport.removeEventListener("hashchange", onHashChange);
  };
}

if (typeof document !== "undefined") {
  const start = () => initMobileDestinationScroll(document, window);
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start, { once: true });
  } else {
    start();
  }
}
