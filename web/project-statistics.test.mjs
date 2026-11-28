import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';
import vm from 'node:vm';
import {setImmediate} from 'node:timers/promises';

// FTW Cloud's panel. Statistics are part of Cloud's terms and nothing else,
// and the owner accepts once, at pairing (Fredrik, 28 November 2026). The box
// never prompts on the Overview; only Cloud under earlier terms offers the
// current ones, in Settings.
const source=readFileSync(new URL('./project-statistics.js',import.meta.url),'utf8');

class Element {
  constructor(tag){this.tag=tag;this.children=[];this.events={};this.style={};this.attributes={};this.ownText='';}
  appendChild(e){this.children.push(e);return e;}
  insertBefore(e){this.children.unshift(e);return e;}
  set textContent(v){this.ownText=v;this.children=[];}
  get textContent(){return [this.ownText,...this.children.map(e=>e.textContent)].filter(Boolean).join(' ');}
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(k,f){this.events[k]=f;}
}

const STATES={
  off:{link:'off',state:'off',cloudTermsAccepted:false,statisticsOnly:false},
  on:{link:'on',state:'on',cloudTermsAccepted:true,statisticsOnly:false,statistics:true},
  earlier:{link:'on',state:'on',cloudTermsAccepted:false,statisticsOnly:false,statisticsNeedTerms:true},
  ending:{link:'ending',state:'revoking',cloudTermsAccepted:false,statisticsOnly:false},
};

function page(cloud='off',authorised=true,options={}) {
  const main=new Element('main');
  const head=new Element('head');
  const app=main.appendChild(new Element('div'));app.attributes['data-project-statistics-settings']='';
  const assistant=main.appendChild(new Element('div'));assistant.id='access-cloud-settings';assistant.attributes['data-project-statistics-settings']='';
  const all=()=>{const out=[];const visit=e=>{out.push(e);e.children.forEach(visit);};visit(main);visit(head);return out;};
  let state={...STATES[cloud]};
  const writes=[];
  const policy={policy:'stats-current',cloud_policy:'cloud-current',cloud_scopes:['ftw.live.read'],available:false,cloud_includes_statistics:true,standalone:false};
  const window={addEventListener(){},confirm:()=>true,fetch:async(path,req)=>{
    if(path==='/api/auth/status') {
      if(options.authError)throw Error('status unavailable');
      return {ok:true,json:async()=>({lan_auth:options.lanAuth===true,home_assistant:options.homeAssistant===true})};
    }
    if(req){
      writes.push({path,...req});
      if(options.refreshDuringWrite)await window.FTWStatistics.refresh();
      if(options.writeError)return {ok:false,json:async()=>({error:options.writeError})};
      state=path==='/api/cloud/grant'?{...STATES.on}:{...STATES.ending};
    }
    return {ok:authorised,json:async()=>path.endsWith('stats-policy')?policy:state};
  }};
  const buttons=()=>all().filter(e=>e.tag==='button');
  const document={head,readyState:'loading',addEventListener(){},createElement:t=>new Element(t),
    getElementById:id=>all().find(e=>e.id===id),querySelector:()=>main,
    querySelectorAll:sel=>sel.includes('data-project-statistics-settings')?all().filter(e=>'data-project-statistics-settings' in e.attributes):sel.includes('button')?buttons():[]};
  vm.runInNewContext(source,{window,document,Promise,Error});
  const labels=host=>{const out=[];const visit=e=>{if(e.tag==='button')out.push(e.textContent);e.children.forEach(visit);};visit(host);return out;};
  return {window,writes,main,app,assistant,all,buttons,labels};
}

test('nothing prompts on the Overview, whatever Cloud is doing',async()=>{
  for(const cloud of Object.keys(STATES)) {
    const p=page(cloud);await p.window.FTWStatistics.refresh();
    assert.equal(p.all().some(e=>e.id==='project-statistics-prompt'),false,cloud);
    assert.equal(p.writes.length,0,cloud);
  }
});

test('a box without Cloud sends nothing and offers no statistics choice',async()=>{
  const p=page('off');await p.window.FTWStatistics.refresh();
  assert.deepEqual(p.labels(p.app),[]);
  assert.match(p.app.textContent,/FTW Cloud is off\. The box works locally and sends no statistics/);
  assert.doesNotMatch(p.main.textContent,/anonymous statistics|Yes, contribute|keep it local/i);
  // The hourly reports are linked to the home, and the details say so.
  assert.match(p.assistant.textContent,/They are not anonymous and stay for 48 hours/);
});

test('Cloud accepted at pairing is not asked again',async()=>{
  const p=page('on');await p.window.FTWStatistics.refresh();
  for(const host of [p.app,p.assistant]) {
    assert.deepEqual(p.labels(host),['Turn FTW Cloud off']);
    assert.match(host.textContent,/FTW Cloud is on\. Its terms include project statistics/);
    assert.doesNotMatch(host.textContent,/Review|Accept|house password/);
  }
  assert.equal(p.writes.length,0);
});

test('Cloud under earlier terms is offered the current ones once, in Settings',async()=>{
  const p=page('earlier');await p.window.FTWStatistics.refresh();
  assert.deepEqual(p.labels(p.app),['Accept the current terms','Turn FTW Cloud off']);
  assert.match(p.app.textContent,/earlier terms\. The current terms add project statistics/);
  assert.match(p.app.textContent,/No house password is required/);
  p.buttons()[0].events.click();await setImmediate();await setImmediate();
  assert.equal(p.writes.length,1);assert.equal(p.writes[0].path,'/api/cloud/grant');
  assert.equal(JSON.parse(p.writes[0].body).policy,'cloud-current');
  assert.deepEqual(p.labels(p.app),['Turn FTW Cloud off']);
});

test('an assistant without Cloud accepts its terms once, here',async()=>{
  const p=page('off');await p.window.FTWStatistics.refresh();
  assert.deepEqual(p.labels(p.assistant),['Accept Cloud terms and turn it on']);
  assert.match(p.assistant.textContent,/readable copy of your energy data/);
  assert.match(p.assistant.textContent,/terms include project statistics/);
});

test('turning Cloud off says statistics stop too',async()=>{
  const p=page('on');let asked='';p.window.confirm=q=>{asked=q;return true;};
  await p.window.FTWStatistics.refresh();
  p.buttons()[0].events.click();await setImmediate();await setImmediate();
  assert.match(asked,/stops sending statistics/);
  assert.deepEqual(p.writes.map(w=>[w.method,w.path]),[['DELETE','/api/cloud']]);
  assert.match(p.app.textContent,/stopping and deleting its stored copy/);
});

test('a protected box explains sign-in without asking for password setup',async()=>{
  const p=page('earlier',true,{lanAuth:true});await p.window.FTWStatistics.refresh();
  assert.match(p.app.textContent,/house password protects this choice/);
  assert.match(p.app.textContent,/ask you to log in if needed/);
  assert.equal(p.writes.length,0);
});

test('a failed save shows one error and lets the owner retry',async()=>{
  const p=page('earlier',true,{writeError:'The choice could not be stored.',refreshDuringWrite:true});
  await p.window.FTWStatistics.refresh();
  for(let i=0;i<2;i++) {
    const accept=p.labels(p.app).indexOf('Accept the current terms');
    const b=p.buttons()[accept];
    b.events.click();
    assert.equal(b.textContent,'Saving…');
    await setImmediate();
    assert.equal(b.disabled,false);
    assert.equal(b.textContent,'Accept the current terms');
    const errors=p.app.children[0].children.filter(e=>e.className==='project-statistics-error');
    assert.equal(errors.length,1);
    assert.equal(errors[0].hidden,false);
    assert.equal(errors[0].attributes.role,'alert');
    assert.equal(errors[0].textContent,'The choice could not be stored.');
  }
  assert.equal(p.writes.length,2);
});

test('missing password status does not block the choice',async()=>{
  const p=page('earlier',true,{authError:true});await p.window.FTWStatistics.refresh();
  assert.match(p.app.textContent,/house password is optional/);
  assert.equal(p.labels(p.app)[0],'Accept the current terms');
});

test('an unauthenticated page neither asks nor writes on behalf of an owner',async()=>{
  const p=page('earlier',false);await p.window.FTWStatistics.refresh();
  assert.equal(p.buttons().length,0);assert.equal(p.writes.length,0);
});

test('HA ingress explains the existing HA login instead of an open LAN',async()=>{
  const p=page('earlier',true,{homeAssistant:true});await p.window.FTWStatistics.refresh();
  assert.match(p.app.textContent,/Home Assistant manages sign-in/);
  assert.doesNotMatch(p.app.textContent,/anyone who can reach/);
});
