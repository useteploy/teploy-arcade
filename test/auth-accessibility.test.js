/* Behavior tests execute shipped functions. Layout/assistive-technology
 * acceptance is a separate browser gate; these are not browser assertions. */
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const src = fs.readFileSync('cmd/teploy-arcade/frontend/app.js', 'utf8');
function fn(name) {
  const m = src.match(new RegExp(`^(?:async )?function ${name}\\([^]*?^\\}`, 'm'));
  assert.ok(m, name); return m[0];
}
function run(names, context) {
  vm.createContext(context);
  vm.runInContext(names.map(fn).join('\n'), context);
  return context;
}
for (const value of [null, 'off', '', 'true']) {
  test(`heads do not request external images for preference ${value}`, () => {
    const c = run(['headsEnabled', 'skinHue', 'skinMarkup'], {localStorage: {getItem: () => value}, HEADS_KEY: 'arcade.heads', HEADS_URL: 'https://mc-heads.net/avatar'});
    assert.doesNotMatch(c.skinMarkup('private name'), /<img|mc-heads/);
  });
}
test('blocked storage keeps heads private', () => {
  const c = run(['headsEnabled', 'skinHue', 'skinMarkup'], {localStorage: {getItem() {throw Error('blocked');}}});
  assert.doesNotMatch(c.skinMarkup('private'), /<img/);
});
test('heads require explicit consent, encode names, omit referrer, retain fallback', () => {
  const c = run(['headsEnabled', 'skinHue', 'skinMarkup'], {localStorage: {getItem: () => 'on'}, HEADS_KEY: 'arcade.heads', HEADS_URL: 'https://mc-heads.net/avatar'});
  assert.match(c.skinMarkup('a/b'), /avatar\/a%2Fb\/32/);
  assert.match(c.skinMarkup('a/b'), /referrerpolicy="no-referrer"/);
  assert.match(c.skinMarkup('a/b'), /linear-gradient/);
});
for (const [host, want] of [['game.example','game.example:25565'],['192.0.2.5','192.0.2.5:25565'],['2001:db8::5','[2001:db8::5]:25565'],['[2001:db8::5]','[2001:db8::5]:25565'],['','Port 25565 · configure -game-host to share a join address']]) {
  test(`join address ${host || 'unset'} never uses panel listener`, () => {
    const c = run(['gameAddress'], {state: {host: {public_game_host: host}}, location: {hostname: 'panel.example'}});
    assert.equal(c.gameAddress({address: {host: '0.0.0.0', port: 25565}}), want);
  });
}
for (const [me,want] of [[{unclaimed:true},0],[{auth_disabled:true,effective_role:'admin'},3],[{user:{role:'viewer'}},1],[{user:{role:'operator'}},2]]) {
  test(`role rank requires explicit backend capability: ${JSON.stringify(me)}`, () => {
    const c = run(['myRoleRank'], {state: {me}, ROLE_RANK:{viewer:1,operator:2,admin:3}});
    assert.equal(c.myRoleRank(), want);
  });
}
test('concurrent 401s make one login transition; protected retry sends nothing', async () => {
  let transitions=0, fetches=0;
  const c = run(['api'], {state: {},fetch:async () => {fetches++; return {ok:false,status:401,json:async()=>({error:'sign in'})};},loseSession() {if(!c.state.authLost) {transitions++;c.state.authLost=true;c.state.authEpoch=1;}}});
  await Promise.allSettled([c.api('/api/servers'),c.api('/api/host')]);
  assert.equal(transitions,1);
  await assert.rejects(c.api('/api/servers'), /sign in/);
  assert.equal(fetches,2);
});
test('session generation rejects a successful response that predates revocation', async () => {
  let resolve;
  const c = run(['api'], {state:{},fetch:()=>new Promise(r=>resolve=r)});
  const pending=c.api('/api/servers');
  c.state.authEpoch=1;c.state.authLost=true;
  resolve({ok:true,status:200,json:async()=>({servers:['stale']})});
  await assert.rejects(pending,/sign in/);
});
test('anonymous me revokes session; transient outage keeps it',async () => {
  let lost=0;
  const c=run(['checkSession'],{state:{},api:async()=>({}),loseSession:()=>lost++,applyRole(){},document:{body:{}}});
  assert.equal(await c.checkSession(),false);assert.equal(lost,1);
  c.api=async()=>{throw Error('outage')};
  assert.equal(await c.checkSession(),true);assert.equal(lost,1);
});
test('skip link focuses content without changing route hash', () => {
  let handler,focused=0,prevented=0;
  const c=run(['installAccessibility'],{$:s=>s==='.skip-link'?{addEventListener:(n,h)=>handler=h}:{focus:()=>focused++},MutationObserver:class{observe(){}},document:{body:{}},location:{hash:'#/s/a/console'}});
  c.installAccessibility();handler({preventDefault:()=>prevented++});
  assert.equal(focused,1);assert.equal(prevented,1);assert.equal(c.location.hash,'#/s/a/console');
});
test('custom control supports keyboard activation and refuses denied controls', () => {
  const listeners={},attrs={};let clicks=0;
  const el={dataset:{},title:'Toggle task',textContent:'',setAttribute:(k,v)=>attrs[k]=v,addEventListener:(k,v)=>listeners[k]=v,matches:()=>true,classList:{contains:()=>false},click:()=>clicks++};
  const root={querySelectorAll:s=>s.startsWith('.swatch')?[el]:[]};
  const c=run(['enhanceAccessibility'],{});c.enhanceAccessibility(root);
  assert.equal(attrs.role,'button');assert.equal(el.tabIndex,0);
  listeners.keydown({key:' ',preventDefault(){}});assert.equal(clicks,1);
  el.dataset.denied='1';listeners.keydown({key:'Enter',preventDefault(){}});assert.equal(clicks,1);
});
test('dialog traps focus, delegates Escape to its owner, restores opener', () => {
  const listeners={},attrs={};let focused='',removed=0,close=0;
  const make=(name)=>({disabled:false,hidden:false,getClientRects:()=>[{}],focus:()=>focused=name});
  const first=make('first'),last=make('last'),opener={isConnected:true,focus:()=>focused='opener'};
  const dialog={setAttribute:(k,v)=>attrs[k]=v,querySelector:s=>s==='.modal-bar'?{textContent:'Edit task'}:{click:()=>close++},querySelectorAll:()=>[first,last],contains:el=>[first,last].includes(el),focus:()=>focused='dialog'};
  const scrim={dataset:{},querySelector:()=>dialog,remove:()=>removed++,addEventListener:(name,handler)=>listeners[name]=handler};
  const app={},document={activeElement:opener,querySelector:()=>app};
  const c=run(['manageDialog'],{state:{},document,$:()=>null});c.manageDialog(scrim);
  assert.equal(app.inert,true);assert.equal(focused,'first');assert.equal(attrs['aria-modal'],'true');
  document.activeElement=last;listeners.keydown({key:'Tab',preventDefault(){}});assert.equal(focused,'first');
  document.activeElement=first;listeners.keydown({key:'Tab',shiftKey:true,preventDefault(){}});assert.equal(focused,'last');
  listeners.keydown({key:'Escape',preventDefault(){}});assert.equal(close,1);
  scrim.remove();assert.equal(removed,1);assert.equal(app.inert,false);assert.equal(focused,'opener');
});
test('auth coordinator disposes owned streams, view and dialog requests once', () => {
  const calls=[];
  const view={firstElementChild:{dispatchEvent:()=>calls.push('teardown')}};
  const login={querySelector:()=>({focus:()=>calls.push('focus')})};
  const c=run(['loseSession'],{state:{servers:['private'],host:{},events:{close:()=>calls.push('sse')},console:{destroy:()=>calls.push('console')},hostTimer:1},routerGeneration:0,clearInterval:()=>calls.push('timer'),$:s=>s==='#view'?view:{replaceChildren:()=>calls.push('dialogs')},CustomEvent:class{},Event:class{},window:{dispatchEvent:()=>calls.push('hashchange'),extraViews:{viewLogin:()=>login}},document:{querySelector:()=>({style:{}}),body:{appendChild:()=>calls.push('login')}}});
  c.loseSession();c.loseSession();
  assert.deepEqual(calls,['sse','timer','console','teardown','hashchange','dialogs','login','focus']);
  assert.equal(c.state.servers.length,0);assert.equal(c.state.authEpoch,1);
});
for (const [ping,want] of [[null,'—'],[undefined,'—'],[0,'0 ms'],[32,'32 ms'],[-1,'—'],[NaN,'—']]) {
 test(`player latency ${String(ping)} is rendered honestly`,()=>{
  const c=run(['playerLatency'],{});assert.equal(c.playerLatency({ping_ms:ping}),want);
 });
}
test('conditional requests retain required JSON content type',async()=>{
 let options;
 const c=run(['api'],{state:{},fetch:async(path,opts)=>{options=opts;return{ok:true,status:200,json:async()=>({})};}});
 await c.api('/api/servers/s/file',{method:'PUT',headers:{'If-Match':'"old"'}});
 assert.equal(options.headers['If-Match'],'"old"');assert.equal(options.headers['Content-Type'],'application/json');
});
test('player provenance retains unknown UUID rather than inventing an identity',()=>{
 const c=run(['playerObservation'],{});
 assert.equal(c.playerObservation({source:'log',uuid:null}),'Observation: log · UUID: —');
 assert.equal(c.playerObservation({source:'unexpected',uuid:null}),'Observation: unknown · UUID: —');
});
for (const [server,want] of [[{status:'running',readiness:'unverified'},'Container running; game readiness is unverified'],[{status:'running',readiness:'observed'},'Game readiness observed in server output'],[{status:'running',runtime:'sim'},'Simulator running; readiness is simulated'],[{status:'stopped'},'Offline']]) {
 test(`readiness tooltip ${JSON.stringify(server)} keeps process and readiness distinct`,()=>{
  const c=run(['readinessTitle'],{STATUS_LABEL:{stopped:'Offline'}});assert.equal(c.readinessTitle(server),want);
 });
}
