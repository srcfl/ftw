import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
const app=readFileSync(new URL('./app.js',import.meta.url),'utf8');
const daily=readFileSync(new URL('./energy-history.js',import.meta.url),'utf8');
function fn(source,name,next) {return source.slice(source.indexOf('  function '+name+'('),source.indexOf('  function '+next+'('));}

test('chart smoothing leaves missing samples and does not cross a gap',()=>{
 const c={};vm.createContext(c);vm.runInContext(fn(app,'smoothSeriesForChart','statusClass'),c);
 const result=c.smoothSeriesForChart([100,200,null,800,1000],[0,1000,2000,3000,4000],10000);
 assert.deepEqual(Array.from(result),[100,150,null,800,900]);
});

test('daily totals distinguish no measurements, measured zero and partial coverage',()=>{
 const c={};vm.createContext(c);vm.runInContext(fn(daily,'sum','formatDay'),c);
 assert.equal(c.sum([{import_wh:null}],'import_wh'),null);
 assert.equal(c.formatKWh(null),'Unknown');
 assert.equal(c.sum([{import_wh:0}],'import_wh'),0);
 assert.equal(c.sum([{import_wh:null},{import_wh:2500}],'import_wh'),2500);
});

test('daily view renders unknown home use and a measured grid total',()=>{
 const els={};for(const id of ['energy-history-summary','energy-history-insight']) els[id]={innerHTML:''};
 const c={document:{getElementById:id=>els[id]||null},dailySeries:[{key:'load_wh'},{key:'pv_wh'},{key:'import_wh'},{key:'export_wh'}]};
 vm.createContext(c);vm.runInContext(fn(daily,'sum','formatDay')+fn(daily,'recordingGaps','drawDailyChart'),c);
 const days=[{day:'2026-10-12',load_wh:null,pv_wh:3000,import_wh:1200,export_wh:0,bat_charged_wh:null,bat_discharged_wh:null,history_quality:'incomplete'}];
 c.renderSummary(days);c.renderInsights(days);
 assert.match(els['energy-history-summary'].innerHTML,/Unknown/);
 assert.match(els['energy-history-summary'].innerHTML,/1.2 kWh/);
 assert.match(els['energy-history-insight'].innerHTML,/Incomplete measurements/);
 assert.doesNotMatch(els['energy-history-insight'].innerHTML,/%/);
});

function liveTipsFor(data, timestamps=data.map((_,i)=>i)) {
 const noop=()=>{};
 const ctx={createLinearGradient:()=>({addColorStop:noop}),beginPath:noop,moveTo:noop,lineTo:noop,closePath:noop,fill:noop,stroke:noop,setLineDash:noop};
 const c={ctx,series:[{data,color:'#ef4444',width:2}],chartHistory:{timestamps},tsToX:x=>x,valToY:y=>y,pad:{top:0},plotH:100,hexAlpha:()=>''};
 vm.createContext(c);
 const start=app.indexOf('    var liveTips = [];');
 const end=app.indexOf('    // ---- Forward-looking forecast',start);
 assert.ok(start>=0 && end>start);
 vm.runInContext(app.slice(start,end),c);
 return Array.from(c.liveTips,tip=>({x:tip.x,y:tip.y}));
}

test('live chart pulses only the current segment after a measurement gap',()=>{
 assert.deepEqual(liveTipsFor([100,200,null,300,400]),[{x:4,y:400}]);
 assert.deepEqual(liveTipsFor([100,200,null,300]),[{x:3,y:300}]);
});

test('live chart leaves old endpoints quiet when the latest measurement is unknown',()=>{
 assert.deepEqual(liveTipsFor([100,200,null,300,400,null]),[]);
 assert.deepEqual(liveTipsFor([100,200],[0,1,2]),[]);
});
