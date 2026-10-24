import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import {test} from 'node:test';
import assert from 'node:assert/strict';

test('HA setup hides the extra password and starts without creating one',async () => {
  const prefix='/api/hassio_ingress/example/';
  const fields = Object.fromEntries(['assistant-password','assistant-password-confirm','assistant-start','assistant-status','assistant-resume'].map(id=>[id,{parentElement:{hidden:false,previousElementSibling:{textContent:''}}}]));
  const calls=[];
  const window={location:{}};
  const context=vm.createContext({window,URL,Promise,document:{
    querySelector:()=>({src:'https://ha.example'+prefix+'home-assistant.js'}),
    getElementById:id=>fields[id]
  },fetch:async (url,options)=>{calls.push([url,options]);return {ok:true};},setTimeout:fn=>fn()});
  vm.runInContext(readFileSync(new URL('./home-assistant.js',import.meta.url),'utf8'),context);
  await window.startAssistantSetup();
  assert.equal(window.FTWHomeAssistant,true);
  assert.equal(fields['assistant-password'].parentElement.hidden,true);
  assert.equal(fields['assistant-password-confirm'].parentElement.hidden,true);
  assert.deepEqual(JSON.parse(calls[0][1].body),{confirmed:true});
  assert.equal(calls[0][0],prefix+'api/setup/assistant');
  assert.equal(window.location.href,prefix+'?setup=assistant');
});
