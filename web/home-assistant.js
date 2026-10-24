// Loaded only by Core's trusted Home Assistant ingress response.
(function () {
  'use strict';
  window.FTWHomeAssistant = true;
  var script = document.querySelector('script[src$="/home-assistant.js"]');
  if (!script) return;
  var base = new URL('.', script.src).pathname;
  var password = document.getElementById('assistant-password');
  if (!password) return;
  ['assistant-password', 'assistant-password-confirm'].forEach(function (id) {
    document.getElementById(id).parentElement.hidden = true;
  });
  var intro = password.parentElement.previousElementSibling;
  if (intro) intro.textContent = 'Home Assistant manages access to FTW. Start this new box, then approve your assistant here. You choose whether to enable FTW Cloud.';
  window.startAssistantSetup = async function () {
    var button = document.getElementById('assistant-start');
    var note = document.getElementById('assistant-status');
    button.disabled = true;
    note.textContent = 'Starting FTW…';
    try {
      var response = await fetch(base + 'api/setup/assistant', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({confirmed: true})
      });
      if (!response.ok) {
        var problem = await response.json();
        note.textContent = problem.error || 'Could not start setup.';
        button.disabled = false;
        return;
      }
    } catch (_) { /* The process may restart before its reply arrives. */ }
    for (var attempt = 0; attempt < 30; attempt++) {
      await new Promise(function (resolve) { setTimeout(resolve, 1000); });
      try {
        var status = await fetch(base + 'api/auth/status', {cache: 'no-store'});
        if (status.ok) { window.location.href = base + '?setup=assistant'; return; }
      } catch (_) { /* Wait for Core's restart. */ }
    }
    note.textContent = 'FTW has not answered yet. Reopen it from Home Assistant to check setup before trying again.';
    document.getElementById('assistant-resume').hidden = false;
  };
}());
