// House-password login for the LAN. Patches same-origin /api/ fetch so a
// 401 "valid LAN password required" opens a modal. The session cookie is
// HttpOnly; the password stays in the form only.
(function (window, document) {
  "use strict";
  if (window.FTWLanAuth) return;
  if (typeof window.fetch !== "function") return;

  var nativeFetch = window.fetch.bind(window);
  var LAN_ERROR = "valid LAN password required";
  var pendingLogin = null;

  function requestURL(input) {
    if (typeof input === "string") return input;
    if (input && typeof input.url === "string") return input.url;
    return "";
  }

  function sameOriginAPI(url) {
    if (!url) return false;
    try {
      var resolved = new URL(url, window.location && window.location.href ? window.location.href : "http://localhost/");
      var origin = window.location && window.location.origin;
      if (origin && resolved.origin !== origin) return false;
      return resolved.pathname.indexOf("/api/") === 0;
    } catch (e) {
      return url.charAt(0) === "/" && url.indexOf("/api/") === 0;
    }
  }

  function isAuthPath(url) {
    try {
      var path = new URL(url, window.location && window.location.href ? window.location.href : "http://localhost/").pathname;
      return path === "/api/auth/login" || path === "/api/auth/logout" || path === "/api/auth/status";
    } catch (e) {
      return /\/api\/auth\/(login|logout|status)$/.test(url);
    }
  }

  function readError(res) {
    return res.clone().json().then(function (body) {
      return body && body.error ? String(body.error) : "";
    }).catch(function () {
      return "";
    });
  }

  function ensureModal() {
    var el = document.getElementById("lan-auth-modal");
    if (el) return el;
    el = document.createElement("div");
    el.id = "lan-auth-modal";
    el.className = "modal hidden";
    el.setAttribute("role", "dialog");
    el.setAttribute("aria-modal", "true");
    el.setAttribute("aria-labelledby", "lan-auth-title");
    el.style.zIndex = "1100";
    el.innerHTML =
      '<div class="modal-content" style="max-width:420px">' +
        '<div class="modal-header">' +
          '<h2 id="lan-auth-title">House password</h2>' +
        '</div>' +
        '<form id="lan-auth-form" class="modal-body">' +
          '<p style="margin-top:0;color:var(--text-dim);font-size:0.85rem">This box asks for the house password before settings and other writes. Live numbers stay visible.</p>' +
          '<label for="lan-auth-password">Password</label>' +
          '<input id="lan-auth-password" type="password" name="password" autocomplete="current-password" required>' +
          '<p id="lan-auth-error" style="color:var(--alarm);font-size:0.85rem;min-height:1.2em"></p>' +
          '<div class="modal-footer" style="padding:12px 0 0;border:0">' +
            '<button type="button" id="lan-auth-cancel" class="btn-ghost">Cancel</button>' +
            '<button type="submit" class="btn-send">Sign in</button>' +
          '</div>' +
        '</form>' +
      '</div>';
    (document.body || document.documentElement).appendChild(el);
    return el;
  }

  function openLoginModal() {
    if (pendingLogin) return pendingLogin;
    pendingLogin = new Promise(function (resolve) {
      var modal = ensureModal();
      var form = document.getElementById("lan-auth-form");
      var input = document.getElementById("lan-auth-password");
      var errEl = document.getElementById("lan-auth-error");
      var cancel = document.getElementById("lan-auth-cancel");
      var settled = false;

      function finish(ok) {
        if (settled) return;
        settled = true;
        modal.classList.add("hidden");
        if (input) input.value = "";
        if (errEl) errEl.textContent = "";
        document.removeEventListener("keydown", onKey);
        resolve(ok);
      }

      function onKey(e) {
        if (e.key === "Escape") finish(false);
      }

      modal.classList.remove("hidden");
      if (errEl) errEl.textContent = "";
      if (input) {
        input.value = "";
        input.focus();
      }
      document.addEventListener("keydown", onKey);

      if (cancel) cancel.onclick = function () { finish(false); };
      modal.onclick = function (e) {
        if (e.target === modal) finish(false);
      };
      if (form) form.onsubmit = function (e) {
        e.preventDefault();
        var password = input ? input.value : "";
        if (errEl) errEl.textContent = "";
        nativeFetch("/api/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password: password }),
        }).then(function (r) {
          return r.json().then(function (body) {
            if (!r.ok) {
              var said = (body && body.error) || "";
              throw new Error(said === "invalid password" ?
                "That is not the house password. After five wrong tries, wait 30 seconds." :
                "Could not sign in. Try again in a moment.");
            }
            finish(true);
            if (typeof window.dispatchEvent === "function" && typeof window.Event === "function") window.dispatchEvent(new window.Event("ftw-owner-ready"));
          });
        }).catch(function (err) {
          // Our own refusals are plain Errors; a network or parse failure
          // is not, and its message is not written for a household.
          if (errEl) errEl.textContent = err && err.name === "Error" && err.message ?
            err.message : "Could not reach the box. Try again in a moment.";
          if (input) input.focus();
        });
      };
    }).finally(function () {
      pendingLogin = null;
    });
    return pendingLogin;
  }

  window.fetch = function (input, init) {
    return nativeFetch(input, init).then(function (res) {
      if (res.status !== 401) return res;
      var url = requestURL(input);
      if (!sameOriginAPI(url) || isAuthPath(url)) return res;
      return readError(res).then(function (err) {
        if (err !== LAN_ERROR) return res;
        return openLoginModal().then(function (ok) {
          if (!ok) return res;
          return nativeFetch(input, init);
        });
      });
    });
  };

  function boot() {
    nativeFetch("/api/auth/status").catch(function () {});
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }

  window.FTWLanAuth = {
    ensure: function () { return openLoginModal(); },
    onUnauthorized: function () { return openLoginModal(); },
  };
})(window, document);
