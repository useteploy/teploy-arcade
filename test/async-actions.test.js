const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const source=fs.readFileSync('cmd/teploy-arcade/frontend/app.js','utf8').match(/^function startAsyncAction\([^]*?^\}/m)[0];
class El{
 constructor(){this.isConnected=true;this.listeners=new Map();this.innerHTML='Start';this.textContent='Start';}
 addEventListener(name,fn){this.listeners.set(name,fn);}
 removeEventListener(name,fn){if(this.listeners.get(name)===fn)this.listeners.delete(name);}
 emit(name){this.listeners.get(name)?.();}
}
function fixture(){
 const root=new El(),button=new El(),window=new El(),timers=new Map(),requests=[],finished=[],toasts=[];let next=1;
 const context={state:{},location:{hash:'#/s/s1/backups'},crypto:{randomUUID:()=>`key-${next++}`},AbortController,root,window,setTimeout:fn=>{const id=next++;timers.set(id,fn);return id;},clearTimeout:id=>timers.delete(id),api:(url,opts={})=>new Promise((resolve,reject)=>requests.push({url,opts,resolve,reject})),toast:(...args)=>toasts.push(args)};
 vm.createContext(context);vm.runInContext(source,context);
 const action=context.startAsyncAction(root,button,{serverId:'s1',url:'/api/servers/s1/backups',body:{note:'fixture'},label:'Backing up…',onFinish:job=>finished.push(job)});
 return {root,button,window,timers,requests,finished,toasts,context,action,tick(){const [id,fn]=timers.entries().next().value;timers.delete(id);return fn();}};
}
const flush=async()=>{await Promise.resolve();await Promise.resolve();};
async function accept(f){f.requests[0].resolve({id:'job /1',state:'running'});await flush();}
test('accepted action sends Prefer and unique key; polling never overlaps',async()=>{
 const f=fixture();assert.equal(f.requests[0].opts.headers.Prefer,'respond-async');assert.equal(f.requests[0].opts.headers['Idempotency-Key'],'key-1');
 f.action.retry();assert.equal(f.requests.length,1);
 await accept(f);const poll=f.tick();assert.equal(f.requests[1].url,'/api/servers/s1/operations/job%20%2F1');
 f.action.retry();assert.equal(f.requests.length,2);
 f.requests[1].resolve({id:'job /1',state:'running'});await poll;assert.equal(f.timers.size,1);
});
test('failed poll retries same accepted job without another POST',async()=>{
 const f=fixture();await accept(f);const poll=f.tick();f.requests[1].reject(Error('status unavailable'));await poll;
 assert.equal(f.button.textContent,'Retry status');assert.equal(f.button.disabled,false);assert.equal(f.timers.size,0);
 f.action.retry();f.action.retry();assert.equal(f.requests.length,3);assert.equal(f.requests[2].url,f.requests[1].url);
 assert.equal(f.requests.filter(r=>r.opts.method==='POST').length,1);
 f.requests[2].resolve({id:'job /1',state:'done'});await flush();assert.equal(f.finished.length,1);assert.equal(f.timers.size,0);
 f.action.retry();assert.equal(f.requests.length,3);
});
test('navigation cancels pending browser read and suppresses late completion',async()=>{
 const f=fixture();await accept(f);const poll=f.tick(),request=f.requests[1];
 f.context.location.hash='#/settings';f.window.emit('hashchange');assert.equal(request.opts.signal.aborted,true);
 request.resolve({id:'job /1',state:'done'});await poll;assert.equal(f.finished.length,0);assert.equal(f.timers.size,0);assert.equal(f.context.state.asyncActions.size,0);
});
test('teardown before acceptance ignores a late POST and never polls',async()=>{
 const f=fixture();f.root.emit('gss:teardown');assert.equal(f.requests[0].opts.signal.aborted,true);
 await accept(f);assert.equal(f.timers.size,0);assert.equal(f.finished.length,0);
});
test('terminal failure remains failure and stops owned polling',async()=>{
 const f=fixture();f.requests[0].resolve({id:'j',state:'failed',error:'disk full'});await flush();
 assert.equal(f.finished.length,1);assert.equal(f.finished[0].state,'failed');assert.deepEqual(f.toasts,[['disk full','err']]);assert.equal(f.timers.size,0);
});
test('lost POST response exposes uncertainty and refuses blind resubmission',async()=>{
 const f=fixture();f.requests[0].reject(Error('network lost'));await flush();
 assert.equal(f.button.textContent,'Submission outcome unknown');assert.equal(f.button.disabled,true);
 f.action.retry();assert.equal(f.requests.length,1);assert.equal(f.finished.length,0);
});
test('auth loss disposes a pending operation read without claiming to cancel work',async()=>{
 const f=fixture();await accept(f);const poll=f.tick();f.context.state.authLost=true;f.action.dispose();
 assert.equal(f.requests[1].opts.signal.aborted,true);f.requests[1].resolve({id:'job /1',state:'done'});await poll;
 assert.equal(f.finished.length,0);assert.equal(f.context.state.asyncActions.size,0);
});
