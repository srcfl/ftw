import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import {test} from 'node:test';
import assert from 'node:assert/strict';

// Home Assistant signs its users in, so the setup guide skips the house
// password (setup.js reads the flag in asksHousePassword).
test('HA ingress marks the page as Home Assistant', () => {
  const window = {};
  vm.runInContext(readFileSync(new URL('./home-assistant.js', import.meta.url), 'utf8'), vm.createContext({window}));
  assert.equal(window.FTWHomeAssistant, true);
  assert.match(readFileSync(new URL('./setup.js', import.meta.url), 'utf8'), /firstRun && !window\.FTWHomeAssistant/);
});
