import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';
import vm from 'node:vm';
import {setImmediate} from 'node:timers/promises';

const source=readFileSync(new URL('./project-statistics.js',import.meta.url),'utf8');
function page(cloud=false,authorised=true,options={}) {
  class Element {
    constructor(tag){this.tag=tag;this.children=[];this.events={};this.style={};this.attributes={};this.ownText='';}
    appendChild(e){this.children.push(e);return e;}
    insertBefore(e){this.children.unshift(e);}
    set textContent(v){this.ownText=v;this.children=[];}
    get textContent(){return this.ownText+this.children.map(e=>e.textContent).join(' ');}
    setAttribute(name,value){this.attributes[name]=value;}
    addEventListener(k,f){this.events[k]=f;}
  }
  const main=new Element('main');
  const head=new Element('head');
  const all=()=>{const out=[];const visit=e=>{out.push(e);e.children.forEach(visit);};visit(main);visit(head);return out;};
  let state={link:cloud?'on':'off',statisticsDecisionRequired:true,statisticsOnly:false};
  const writes=[];
  const policy={policy:'stats-current',cloud_policy:'cloud-current',cloud_scopes:['ftw.live.read'],available:false,cloud_includes_statistics:true,standalone:true};
  const window={addEventListener(){},confirm:()=>true,fetch:async(path,req)=>{
    if(path==='/api/auth/status') {
      if(options.authError)throw Error('status unavailable');
      return {ok:true,json:async()=>({lan_auth:options.lanAuth===true,home_assistant:options.homeAssistant===true})};
    }
    if(req){
      writes.push({path,...req});
      if(options.refreshDuringWrite)await window.FTWStatistics.refresh();
      if(options.writeError)return {ok:false,json:async()=>({error:options.writeError})};
      state={...state,statisticsDecisionRequired:false,statisticsActive:req.method==='POST',cloudTermsAccepted:path==='/api/cloud/grant' && req.method==='POST'};
    }
    return {ok:authorised,json:async()=>path.endsWith('stats-policy')?policy:state};
  }};
  const document={head,readyState:'loading',addEventListener(){},createElement:t=>new Element(t),getElementById:id=>all().find(e=>e.id===id),querySelector:()=>main,querySelectorAll:()=>[]};
  vm.runInNewContext(source,{window,document,Promise,Error});
  return {window,writes,main,all,buttons:()=>all().filter(e=>e.tag==='button')};
}

test('a box without Cloud is not asked, and Settings offers Cloud only',async()=>{
  const p=page();await p.window.FTWStatistics.refresh();
  assert.equal(p.writes.length,0);
  assert.equal(p.buttons().length,0);
  const prompt=p.all().find(e=>e.id==='project-statistics-prompt');
  assert.equal(prompt.hidden,true);
  const settings={tag:'div',children:[],events:{},style:{},attributes:{},ownText:'',appendChild(e){this.children.push(e);return e;},set textContent(v){this.children=[];},get textContent(){return this.children.map(e=>e.textContent).join(' ');}};
  p.window.FTWStatistics.mount(settings);
  const labels=[];const visit=e=>{if(e.tag==='button')labels.push(e.textContent);(e.children||[]).forEach(visit);};visit(settings);
  assert.deepEqual(labels,['Accept Cloud terms and turn it on']);
  assert.match(settings.textContent,/FTW Cloud is off/);
  assert.match(settings.textContent,/Anonymous statistics below/);
  assert.doesNotMatch(settings.textContent,/Yes, contribute|keep it local/);
  assert.equal(p.writes.length,0);
});

test('Cloud has bundled terms and a whole-service off action',async()=>{
  const p=page(true);await p.window.FTWStatistics.refresh();
  assert.deepEqual(p.buttons().map(e=>e.textContent),['Accept Cloud terms','Turn FTW Cloud off']);
  assert.match(p.main.textContent,/FTW Cloud includes project statistics/);
  assert.match(p.main.textContent,/No house password is required/);
  assert.equal(p.writes.length,0);
  p.buttons()[0].events.click();await setImmediate();
  assert.equal(p.writes.length,1);assert.equal(p.writes[0].path,'/api/cloud/grant');
  assert.equal(JSON.parse(p.writes[0].body).policy,'cloud-current');
  assert.match(p.main.textContent,/Cloud terms accepted and saved on this box/);
});

test('a protected box explains sign-in without asking for password setup',async()=>{
  const p=page(true,true,{lanAuth:true});await p.window.FTWStatistics.refresh();
  assert.match(p.main.textContent,/house password protects this choice/);
  assert.match(p.main.textContent,/ask you to log in if needed/);
  assert.equal(p.writes.length,0);
  p.buttons()[0].events.click();await setImmediate();
  assert.deepEqual(p.writes.map(w=>w.path),['/api/cloud/grant']);
});

test('a failed save shows one error and lets the owner retry',async()=>{
  const p=page(true,true,{writeError:'The choice could not be stored.',refreshDuringWrite:true});
  await p.window.FTWStatistics.refresh();
  for(let i=0;i<2;i++) {
    p.buttons()[0].events.click();
    assert.equal(p.buttons()[0].textContent,'Saving…');
    await setImmediate();
    assert.equal(p.buttons()[0].disabled,false);
    assert.equal(p.buttons()[0].textContent,'Accept Cloud terms');
    const errors=p.all().filter(e=>e.className==='project-statistics-error');
    assert.equal(errors.length,1);
    assert.equal(errors[0].hidden,false);
    assert.equal(errors[0].attributes.role,'alert');
    assert.equal(errors[0].textContent,'The choice could not be stored.');
  }
  assert.equal(p.writes.length,2);
});

test('missing password status does not block the consent choice',async()=>{
  const p=page(true,true,{authError:true});await p.window.FTWStatistics.refresh();
  assert.match(p.main.textContent,/house password is optional/);
  assert.equal(p.buttons()[0].textContent,'Accept Cloud terms');
});

test('an unauthenticated page neither asks nor writes on behalf of an owner',async()=>{
  const p=page(false,false);await p.window.FTWStatistics.refresh();
  assert.equal(p.buttons().length,0);assert.equal(p.writes.length,0);
});


test('HA ingress explains the existing HA login instead of an open LAN',async()=>{
  const p=page(true,true,{homeAssistant:true});await p.window.FTWStatistics.refresh();
  assert.match(p.main.textContent,/Home Assistant manages sign-in/);
  assert.doesNotMatch(p.main.textContent,/anyone who can reach/);
});
