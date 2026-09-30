import test from 'node:test';
import assert from 'node:assert/strict';
import { feedbackStatus,feedbackText,feedbackPower,feedbackProof,feedbackRows,feedbackValues,feedbackSite,feedbackCurve } from './control-feedback.js';

test('command acknowledgement, device measurement and site confirmation stay distinct',()=>{
 const row={driver:'battery',kind:'battery',reason:'power_observed',response:'device_reported'};
 assert.match(feedbackProof({...row,verification_tier:0}),/Tier 0/);
 assert.match(feedbackProof({...row,verification_tier:1}),/Tier 1/);
 assert.match(feedbackProof({...row,verification_tier:2}),/Tier 2/);
 assert.doesNotMatch(feedbackProof({...row,verification_tier:2},false),/Confirmed|Tier 2/);
 assert.match(feedbackText(row).detail,/device’s own/);
 assert.match(feedbackSite({...row,site_confirmation:'other_flows_changed'}),/cannot isolate/);
});

test('unknown causes and missing readings never become success or zero',()=>{
 assert.equal(feedbackPower(null,'battery'),'Unknown');
 assert.equal(feedbackPower(NaN,'battery'),'Unknown');
 assert.equal(feedbackPower(0,'battery'),'0 W');
 assert.equal(feedbackPower(-1000,'battery'),'1.0 kW discharge');
 assert.match(feedbackText({reason:'power_differs'}).action,/not reported a confirmed cause/);
 assert.match(feedbackText({reason:'future_reason'}).title,/not verified/);
 assert.deepEqual(feedbackRows(undefined),[]);
 assert.deepEqual(feedbackRows([null,{driver:'a'},42]),[]);
 const values=feedbackValues({actual_w:1000,device_limit_a:8},false);
 assert.equal(values.find(v=>v[0]==='Measured')[1],'Not current');
});

test('known charger limits do not disappear while power flows',()=>{
 const text=feedbackText({reason:'device_limit',actual_w:5500});
 assert.match(text.detail,/charger’s own current limit/);
 const values=feedbackValues({requested_a:16,device_limit_a:8,actual_w:5500,kind:'ev'});
 assert.deepEqual(values.find(v=>v[0]==='Charger limit'),['Charger limit','8.0 A']);
});

test('curve evidence expires with status and rejects invalid samples',()=>{
 const row={driver:'battery',reason:'power_observed',site_evidence:{trace:[0,1,2].map(i=>({at_ms:1000+i*5000,device_change_w:i*500,adjusted_site_change_w:i*500+10}))}};
 assert.match(feedbackCurve(row).label,/10 seconds/);
 assert.equal(feedbackCurve(row,false),null);
 row.site_evidence.trace[1].device_change_w=NaN;
 assert.equal(feedbackCurve(row),null);
});

test('comparison time gaps use milliseconds and require measured evidence',()=>{
 const gap=(e)=>feedbackValues({site_evidence:e}).find(v=>v[0]==='Largest time gap');
 assert.equal(gap({samples:0,max_skew_ms:0}),undefined);
 assert.equal(gap({samples:3,max_skew_ms:null}),undefined);
 assert.deepEqual(gap({samples:3,max_skew_ms:0}),['Largest time gap','0 ms']);
 assert.deepEqual(gap({samples:3,max_skew_ms:37}),['Largest time gap','37 ms']);
 assert.deepEqual(gap({samples:3,max_skew_ms:1250}),['Largest time gap','1250 ms']);
});

test('a missing measurement names the source holding back site confirmation',()=>{
 const row={site_confirmation:'measurement_sources_unclear',site_source_issue:'missing_fresh_power:easee:ev'};
 assert.match(feedbackSite(row),/Fresh power readings from easee \(ev\) are missing/);
 assert.doesNotMatch(feedbackSite(row,false),/Fresh power readings/);
});

test('each device has its own status and unknown background is not a veto',()=>{
 const row={driver:'battery',reason:'power_observed',verification_tier:2,site_confirmation:'confirmed',site_evidence:{unmeasured_flows:['offline-ev:ev']}};
 assert.equal(feedbackStatus(row).tone,'confirmed');
 assert.match(feedbackSite(row),/separate site meter/);
 assert.deepEqual(feedbackValues(row).find(v=>v[0]==='Background, not required sources'),['Background, not required sources','offline-ev:ev']);
 assert.equal(feedbackStatus({driver:'ev',reason:'waiting_response',verification_tier:0}).tone,'waiting');
 assert.equal(feedbackStatus({driver:'ev',reason:'telemetry_stale',verification_tier:0,verification_lost:true}).tone,'alarm');
 assert.equal(feedbackStatus(row,false).tone,'unknown');
});

test('current warnings override a measured response and normal waits remain quiet',async()=>{
 const {withControlProof}=await import('./control-feedback.js');
 const row={driver:'battery',kind:'battery',reason:'waiting_response',verification_tier:0};
 const proof=withControlProof([], [row])[0].controlProof;
 assert.equal(proof.detail,'Verifying the response');
 assert.equal(proof.tone,'waiting');
 assert.match(feedbackProof(row),/Tier 0/);
 assert.equal(withControlProof([], [{...row,reason:'not_connected'}])[0].controlProof.inactive,true);
 for(const verification_tier of [0,1,2]) {
  assert.equal(feedbackStatus({...row,verification_tier,reason:'device_fault',severity:'warning'}).tone,'alarm');
 }
 assert.match(feedbackSite({site_confirmation:'flows_changing'}),/before this command/);
});

test('bubble details select a function and combined bubbles retain every device',async()=>{
 const {feedbackForPlanet}=await import('./control-feedback.js');
 const rows=[{driver:'hybrid',kind:'battery',reason:'power_observed'},{driver:'hybrid',kind:'pv',reason:'power_observed'},{driver:'other',kind:'battery',reason:'telemetry_stale'}];
 assert.deepEqual(feedbackForPlanet(rows,{role:'battery',name:'hybrid'}),[rows[0]]);
 assert.deepEqual(feedbackForPlanet(rows,{role:'battery',name:'2×',id:'agg-top-right'}),[rows[0],rows[2]]);
});
