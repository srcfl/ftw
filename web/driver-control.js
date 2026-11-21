// Whether FTW can control a configured device, in the words every view uses.
//
// Core states the facts: a catalog entry's read_only, capabilities and
// read_only_reason, or a loadpoint's driver_read_only and
// driver_read_only_reason. This file only chooses the words, so Settings,
// setup and the EV card say the same thing.
//
// A new owner added a Zaptec Go through a read-only driver. FTW accepted the
// charger and never controlled it, and nothing on the box said why.
(function () {
  'use strict';

  // Kinds an owner expects FTW to steer, with the word for each. A battery
  // usually sits behind an inverter or a gateway such as Zap, so it is "this
  // device". A meter or a solar inverter that FTW only reads is complete, so
  // it gets no note.
  var KINDS = [
    ['ev', 'charger'],
    ['v2x_charger', 'charger'],
    ['vehicle', 'car'],
    ['heatpump', 'heat pump'],
    ['battery', 'device'],
  ];

  // {label, text, reason} for a read-only driver of a kind FTW would
  // otherwise control; null for everything else.
  function readOnlyNote(entry) {
    if (!entry || entry.read_only !== true) return null;
    var caps = entry.capabilities || [];
    for (var i = 0; i < KINDS.length; i++) {
      if (caps.indexOf(KINDS[i][0]) >= 0) {
        return {
          label: 'reads only',
          text: 'FTW reads this ' + KINDS[i][1] + ' but cannot control it.',
          reason: typeof entry.read_only_reason === 'string' ? entry.read_only_reason.trim() : '',
        };
      }
    }
    return null;
  }

  // The note as one line of text, the driver's own reason last.
  function sentence(note) {
    if (!note) return '';
    return note.reason ? note.text + ' Its driver says: ' + note.reason : note.text;
  }

  // A loadpoint carries the facts under its own names; it is always a charger.
  function loadpointNote(lp) {
    if (!lp) return null;
    return readOnlyNote({
      read_only: lp.driver_read_only === true,
      capabilities: ['ev'],
      read_only_reason: lp.driver_read_only_reason,
    });
  }

  window.FTWDriverControl = { readOnlyNote: readOnlyNote, loadpointNote: loadpointNote, sentence: sentence };
})();
