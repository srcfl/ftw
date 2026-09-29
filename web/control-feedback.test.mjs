import test from 'node:test';
import assert from 'node:assert/strict';
import { feedbackText,feedbackPower,feedbackProof,feedbackRows,feedbackValues,feedbackSite,feedbackCurve } from './control-feedback.js';

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
