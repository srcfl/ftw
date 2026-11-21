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

  // Kinds an owner expects FTW to steer, with the word for each. A meter, a
  // solar inverter, or a gateway or hybrid inverter that also reads a
  // battery (Zap, SMA, Fronius) is complete when FTW only reads it, so it
  // gets no note. A battery counts only when it is the driver's main job:
  // the driver reads no solar.
  var KINDS = [
    ['ev', 'charger'],
    ['v2x_charger', 'charger'],
    ['vehicle', 'car'],
    ['heatpump', 'heat pump'],
  ];

  function kindWord(caps) {
    for (var i = 0; i < KINDS.length; i++) {
      if (caps.indexOf(KINDS[i][0]) >= 0) return KINDS[i][1];
    }
    if (caps.indexOf('battery') >= 0 && caps.indexOf('pv') < 0) return 'battery';
    return null;
  }

  // {label, text, reason} for a read-only driver of a kind FTW would
  // otherwise control; null for everything else.
  function readOnlyNote(entry) {
    if (!entry || entry.read_only !== true) return null;
    var word = kindWord(entry.capabilities || []);
    if (!word) return null;
    return {
      label: 'reads only',
      text: 'FTW reads this ' + word + ' but cannot control it.',
      reason: typeof entry.read_only_reason === 'string' ? entry.read_only_reason.trim() : '',
    };
  }

  // The note as one line of text, the driver's own reason after it. Core
  // has already dropped a reason that only repeats the note.
  function sentence(note) {
    if (!note) return '';
    return note.reason ? note.text + ' ' + note.reason : note.text;
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
