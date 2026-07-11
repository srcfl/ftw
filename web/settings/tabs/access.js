// Settings -> Access tab: local owner surface for remote-access opt-in,
// passkeys, remembered browser keys, and active owner sessions.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function fmtTime(ms) {
    if (!ms) return "never";
    try { return new Date(ms).toLocaleString(); }
    catch (_) { return "never"; }
  }

  function esc(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }

  function rowHTML(label, value, actionHTML) {
    return '<div class="access-row">' +
      '<div><div class="access-row-title">' + esc(label) + '</div>' +
      '<div class="access-row-sub">' + value + '</div></div>' +
      '<div class="access-row-action">' + (actionHTML || '') + '</div>' +
      '</div>';
  }

  S.tabs.access = {
    render: function (ctx) {
      var cfg = ctx.config || {};
      var enabled = !!(cfg.remote_access && cfg.remote_access.enabled);
      return '' +
        '<style>' +
        '  .access-stack { display:grid; gap:12px; }' +
        '  .access-note { color:var(--text-dim); font-size:.84rem; line-height:1.45; margin:6px 0 0; }' +
        '  .access-state { display:inline-flex; align-items:center; gap:7px; font-size:.84rem; }' +
        '  .access-dot { width:8px; height:8px; border-radius:50%; background:var(--red-e,#ef4444); }' +
        '  .access-dot.on { background:var(--green-e,#22c55e); }' +
        '  .access-row { display:grid; grid-template-columns:minmax(0,1fr) auto; gap:10px; align-items:center; padding:9px 0; border-top:1px solid var(--line,rgba(255,255,255,.1)); }' +
        '  .access-row:first-child { border-top:0; }' +
        '  .access-row-title { font-weight:600; }' +
        '  .access-row-sub { color:var(--text-dim); font-size:.8rem; line-height:1.35; margin-top:2px; }' +
        '  .access-row-action button { width:auto; padding:.35rem .65rem; font-size:.8rem; }' +
        '  .access-danger { border-color:rgba(239,68,68,.5)!important; color:var(--red-e,#ef4444)!important; }' +
        '  .access-muted { color:var(--text-dim); font-size:.82rem; }' +
        '  .access-actions { display:flex; flex-wrap:wrap; gap:8px; margin-top:8px; }' +
        '  .access-actions button, .access-actions a { width:auto; padding:.45rem .75rem; font-size:.84rem; }' +
        '</style>' +
        '<div class="access-stack">' +
        '<fieldset>' +
        '<legend>Remote Access</legend>' +
        '<label><input type="checkbox" data-checkbox-path="remote_access.enabled"' + (enabled ? ' checked' : '') + '> Enable remote access</label>' +
        '<div class="access-note">' +
        'Remote access is opt-in. When it is off, this Pi stays local and does not register with the home relay. ' +
        'When it is on, the Pi registers for rendezvous so your browser can open an end-to-end encrypted P2P channel to the Pi. ' +
        'The relay forwards signaling/ciphertext only; owner data still goes to the Pi.' +
        '</div>' +
        '<p class="access-note"><strong>To turn it off:</strong> uncheck Enable remote access, Save, then restart when prompted.</p>' +
        '<p class="access-state"><span class="access-dot ' + (enabled ? 'on' : '') + '"></span><span id="access-enabled-label">' +
        (enabled ? 'Opted in. Changes take effect after restart.' : 'Off. Local dashboard continues to work.') +
        '</span></p>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Current Browser</legend>' +
        '<div id="access-current" class="access-muted">Loading...</div>' +
        '<div class="access-actions">' +
        '<button type="button" id="access-signout">Sign out this browser</button>' +
        '<a href="/owner-access/" role="button">Open passkey setup</a>' +
        '</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Passkeys</legend>' +
        '<div id="access-passkeys" class="access-muted">Loading...</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Remembered Browsers</legend>' +
        '<div class="access-note">These are browser-local keys used for silent sign-in after a passkey login. Removing one stops that browser from silent-login until it signs in with passkey again.</div>' +
        '<div id="access-browser-keys" class="access-muted" style="margin-top:8px">Loading...</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Active Sessions</legend>' +
        '<div class="access-note">Sessions are the current remote logins. Remove a session to kick that browser out immediately.</div>' +
        '<div id="access-sessions" class="access-muted" style="margin-top:8px">Loading...</div>' +
        '</fieldset>' +
        '</div>';
    },

    after: function (ctx) {
      var ownerFetch = ctx.ownerFetch || fetch;
      var body = ctx.bodyEl;

      function setHTML(id, html) {
        var el = document.getElementById(id);
        if (el) el.innerHTML = html;
      }

      function fetchJSON(path) {
        return ownerFetch(path, { credentials: "same-origin" })
          .then(function (r) {
            if (!r.ok) throw new Error("HTTP " + r.status);
            return r.json();
          });
      }

      function refreshAll() {
        fetchJSON("/api/owner-access/whoami")
          .then(function (me) {
            var mode = me.can_sign_out ? "Remote session" : "Local LAN access";
            setHTML("access-current", rowHTML(mode, "Signed in as " + esc(me.friendly_name || "owner"), ""));
            var btn = document.getElementById("access-signout");
            if (btn) {
              btn.disabled = !me.can_sign_out;
              btn.textContent = me.can_sign_out ? "Sign out this browser" : "No remote session to sign out";
            }
          })
          .catch(function () {
            setHTML("access-current", '<div class="access-muted">Not signed in remotely.</div>');
            var btn = document.getElementById("access-signout");
            if (btn) btn.disabled = true;
          });

        fetchJSON("/api/owner-access/devices")
          .then(function (d) {
            var devices = d.devices || [];
            if (!devices.length) {
              setHTML("access-passkeys", '<div class="access-muted">No passkeys enrolled.</div>');
              return;
            }
            setHTML("access-passkeys", devices.map(function (dev) {
              var sub = "Created " + fmtTime(dev.created_at_ms) +
                " · last used " + fmtTime(dev.last_used_ms) +
                " · remembered browsers " + (dev.browser_keys || 0);
              return rowHTML(dev.friendly_name || "Unnamed passkey", esc(sub),
                '<button type="button" class="access-danger" data-delete-passkey="' + esc(dev.credential_id_b64) + '">Remove</button>');
            }).join(""));
          })
          .catch(function (e) { setHTML("access-passkeys", '<div class="access-muted">' + esc(e.message) + '</div>'); });

        fetchJSON("/api/owner-access/browser-keys")
          .then(function (d) {
            var keys = d.browser_keys || [];
            if (!keys.length) {
              setHTML("access-browser-keys", '<div class="access-muted">No remembered browsers.</div>');
              return;
            }
            setHTML("access-browser-keys", keys.map(function (k) {
              var name = k.friendly_name || "Passkey";
              var sub = "Key ..." + (k.key_hint || k.id) + " · created " + fmtTime(k.created_at_ms) +
                " · last silent login " + fmtTime(k.last_used_ms);
              return rowHTML(name, esc(sub),
                '<button type="button" class="access-danger" data-delete-browser-key="' + esc(k.id) + '">Forget</button>');
            }).join(""));
          })
          .catch(function (e) { setHTML("access-browser-keys", '<div class="access-muted">' + esc(e.message) + '</div>'); });

        fetchJSON("/api/owner-access/sessions")
          .then(function (d) {
            var sessions = d.sessions || [];
            if (!sessions.length) {
              setHTML("access-sessions", '<div class="access-muted">No active remote sessions.</div>');
              return;
            }
            setHTML("access-sessions", sessions.map(function (s) {
              var title = (s.current ? "This browser" : "Remote session") + (s.friendly_name ? " · " + s.friendly_name : "");
              var sub = "Expires " + fmtTime(s.expires_at_ms) + " · id " + s.id;
              return rowHTML(title, esc(sub),
                '<button type="button" class="access-danger" data-delete-session="' + esc(s.id) + '">' + (s.current ? "Sign out" : "Revoke") + '</button>');
            }).join(""));
          })
          .catch(function (e) { setHTML("access-sessions", '<div class="access-muted">' + esc(e.message) + '</div>'); });
      }

      var signout = document.getElementById("access-signout");
      if (signout) {
        signout.addEventListener("click", function () {
          ownerFetch("/api/owner-access/logout", { method: "POST", credentials: "same-origin" })
            .catch(function () {})
            .then(function () { window.location.reload(); });
        });
      }

      if (body && !body._accessClickWired) {
        body._accessClickWired = true;
        body.addEventListener("click", function (e) {
          var passkey = e.target && e.target.getAttribute("data-delete-passkey");
          var browserKey = e.target && e.target.getAttribute("data-delete-browser-key");
          var session = e.target && e.target.getAttribute("data-delete-session");
          if (!passkey && !browserKey && !session) return;
          var msg = passkey
            ? "Remove this passkey? This also revokes its active sessions."
            : browserKey
              ? "Forget this remembered browser key? The browser will need passkey login again."
              : "Revoke this session now?";
          if (!confirm(msg)) return;
          var path = passkey
            ? "/api/owner-access/devices/" + encodeURIComponent(passkey)
            : browserKey
              ? "/api/owner-access/browser-keys/" + encodeURIComponent(browserKey)
              : "/api/owner-access/sessions/" + encodeURIComponent(session);
          var deletingCurrentSession = !!(session && e.target.textContent === "Sign out");
          ownerFetch(path, { method: "DELETE", credentials: "same-origin" })
            .then(function (r) {
              if (!r.ok) throw new Error("HTTP " + r.status);
              if (deletingCurrentSession) window.location.reload();
              else refreshAll();
            })
            .catch(function (err) { alert("Could not update access: " + err.message); });
        });
      }

      refreshAll();
    },
  };
})();
