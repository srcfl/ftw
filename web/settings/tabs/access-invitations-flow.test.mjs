import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { webcrypto } from 'node:crypto';
import { test } from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('./access.js', import.meta.url), 'utf8');
const vector = JSON.parse(readFileSync(new URL('../../../go/protocol/testdata/invitation-vectors.json', import.meta.url), 'utf8'));
const settle = async () => { for (let n = 0; n < 30; n++) await Promise.resolve(); };

function rig({ loseReply = false } = {}) {
  function element(tag) {
    const e = { tag, children: [], listeners: {}, checked: false, value: '',
      setAttribute(k, v) { this[k] = v; }, addEventListener(k, v) { this.listeners[k] = v; },
      appendChild(c) { this.children.push(c); if (tag === 'select' && (!this.value || c.selected)) this.value = c.value; return c; },
      all() { return this.children.flatMap(c => [c, ...c.all()]); },
      querySelectorAll(q) { return this.all().filter(n => {
        if (q === 'input, select') return ['input', 'select'].includes(n.tag);
        if (q.startsWith('[id^=')) return n.id?.startsWith(q.slice(6, -2));
        const m = /^(input|select)\[name="(\w+)"\]$/.exec(q);
        return m && n.tag === m[1] && n.name === m[2];
      }); },
      querySelector(q) { return this.querySelectorAll(q)[0] || null; },
      submit() { this.listeners.submit({ preventDefault() {} }); },
      click() { this.listeners.click?.(); this.onclick?.(); }, select() { this.selectedText = true; },
    };
    let text = '';
    Object.defineProperty(e, 'textContent', { get() { return text; }, set(v) { text = v; this.children = []; } });
    return e;
  }
  const roots = Object.fromEntries(['access-ready', 'access-code', 'access-code-form', 'access-lookup', 'access-note', 'access-offer', 'access-rows', 'access-rows-section', 'access-requests', 'access-requests-section', 'access-invites'].map(id => [id, Object.assign(element('div'), { id })]));
  const all = () => Object.values(roots).flatMap(e => [e, ...e.all()]);
  const posts = [], rows = [], alias = { fails:false, code:'K7M-4PX' };
  const answer = body => Promise.resolve({ ok: true, status: 200, json: async () => body });
  const context = vm.createContext({ window: {}, document: { createElement: element, getElementById: id => all().find(e => e.id === id) || null },
    Uint8Array, crypto: webcrypto, atob, btoa, navigator: {}, setTimeout() {}, clearTimeout() {}, console,
    fetch: (path, opts) => {
      if (path === '/api/cloud') return answer({link:'on',cloudTermsAccepted:true,statisticsOnly:false});
      if (path === '/api/auth/status') return answer({ lan_auth: true, configured: true });
      if (path === '/api/agents') return answer({ agents: [], requests: [], registry: { agentOps: {}, writeScopes: ['ftw.setup.write'], callScopes: ['ftw.setup.write'] }, box: { widen: true, offers: true, clock: 'ok', nowMs: Date.now() } });
      if (path === 'https://cloud.ftw.energy/oauth/invitations') {
        if(alias.fails) return Promise.reject(new Error('closed test publication failure'));
        assert.equal(opts.credentials, 'omit');
        assert.ok(JSON.parse(opts.body).invitation.length > 100, 'only the TLS publishing proof uses the strong format');
        return answer({code:alias.code,expiresAtMs:Date.now()+600000});
      }
      if (path === '/api/agent-invites' && !opts) return answer({ invitations: rows, home: vector.home, scopes: ['ftw.live.read', 'ftw.diag.run', 'ftw.setup.write'], ops: [], caps: { perMin: 6 } });
      if (path === '/api/agent-invites' && opts?.method === 'POST') {
        const body = JSON.parse(opts.body); posts.push(body);
        let row = rows.find(r => r.id === body.id);
        if (!row) { row = { ...body, box: Buffer.from(vector.boxHex, 'hex').toString('base64'), state: 'pending' }; delete row.secret; rows.push(row); }
        if (loseReply) { loseReply = false; return Promise.reject(new Error('closed test connection')); }
        return answer({ invitation: row });
      }
      throw new Error(`Unexpected request ${path}`);
    },
  });
  vm.runInContext(source, context);
  const tab = context.window.FTWSettings.tabs.access;
  const refresh = () => tab.after({ navigateTab() {} }); refresh();
  const button = text => all().find(e => e.tag === 'button' && e.textContent === text);
  return { roots, posts, rows, alias, all, button, refresh, pure: tab._pure };
}

test('the LAN page makes the same invitation code as Go and Swift', () => {
  const r = rig();
  const code = r.pure.inviteCode({ home: vector.home, box: Buffer.from(vector.boxHex, 'hex').toString('base64') }, Buffer.from(vector.secretHex, 'hex'));
  assert.equal(code, vector.code);
});

test('lost creation reply keeps the exact body and recovers its copyable code', async () => {
  const r = rig({ loseReply: true }); await settle();
  r.button('Create an invitation').click();
  const form = r.roots['access-invites'].all().find(e => e.tag === 'form');
  assert.equal(form.querySelector('input[name="call"]').checked, false, 'setup starts off');
  r.all().find(e => e.id === 'invite-label').value = 'Hannes';
  form.submit(); await settle();
  const first = structuredClone(r.posts[0]);
  assert.ok(first);
  assert.equal(first.scopes.includes('ftw.setup.write'), false);
  // Access lasts 90 days unless the owner picks another length.
  assert.ok(Math.abs(first.expiresAtMs - first.claimUntilMs - (90 * 86400000 - 600000)) < 5000, 'invitation access starts at 90 days');
  form.submit(); await settle();
  assert.deepEqual(r.posts[1], first, 'a retry never makes a new invitation');
  const code = r.all().find(e => e['aria-label'] === 'Invitation code');
  assert.equal(code.value, 'K7M-4PX');
  r.button('Copy invitation code').click();
  assert.equal(code.selectedText, true, 'copy works on plain HTTP without clipboard APIs');
  r.rows[0].state = 'claimed'; r.rows[0].agentId = 'recipient-key'; r.rows[0].client = 'ChatGPT';
  r.button('Check status and code').click(); await settle();
  assert.equal(r.all().some(e => e['aria-label'] === 'Invitation code'), false, 'a used code disappears');
});

test('a lost reply followed by a list read recovers the code without resending', async () => {
  const r = rig({ loseReply: true }); await settle();
  r.button('Create an invitation').click();
  r.roots['access-invites'].all().find(e => e.tag === 'form').submit(); await settle();
  r.refresh(); await settle();
  assert.equal(r.posts.length, 1);
  r.button('Get the six-character code').click(); await settle();
  assert.equal(r.all().find(e => e['aria-label'] === 'Invitation code').value, 'K7M-4PX');
});

test('publication failure survives refresh and a Cloud restart can replace the code without a new grant', async () => {
  const r=rig();await settle();r.button('Create an invitation').click();
  r.roots['access-invites'].all().find(e=>e.tag==='form').submit();await settle();
  r.alias.fails=true;r.button('Check status and code').click();await settle();
  assert.ok(r.all().some(e=>e.textContent==='closed test publication failure'));
  r.alias.fails=false;r.alias.code='P3K-9MX';r.button('Check status and code').click();await settle();
  assert.equal(r.all().find(e=>e['aria-label']==='Invitation code').value,'P3K-9MX');
  assert.equal(r.posts.length,1,'republishing does not create a second Core invitation');
});
