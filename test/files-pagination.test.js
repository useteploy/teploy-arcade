const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const source=fs.readFileSync('cmd/teploy-arcade/frontend/views.js','utf8').match(/^async function viewFiles\([^]*?^\}/m)[0];
function fixture() {
  const events=new Map(),nodes=new Map(),requests=[];
  const node=id=>{if(!nodes.has(id))nodes.set(id,{innerHTML:'',textContent:'',addEventListener:(event,cb)=>events.set(id+event,cb),querySelectorAll:()=>[]});return nodes.get(id);};
  const root={addEventListener:(event,cb)=>events.set(event,cb)};
  const context={serverById:()=>({}),h:()=>root,serverHeader:()=>'',wireHeaderActions(){},$:id=>node(id),api:(url,options)=>new Promise((resolve,reject)=>requests.push({url,options,resolve,reject})),AbortController,esc:String,humanBytes:String};
  vm.createContext(context);vm.runInContext(source,context);
  return {context,requests,events,nodes,open:()=>context.viewFiles('fixture')};
}
const flush=async()=>{await Promise.resolve();await Promise.resolve();};
test('next directory page uses opaque encoded cursor, first page resets it',async()=>{
 const f=fixture();await f.open();f.requests[0].resolve({entries:[{name:'first.txt',path:'first.txt',mod:0,size:1}],next_cursor:'a /+b',truncated:true});await flush();
 f.events.get('#nextPageclick')();assert.match(f.requests[1].url,/cursor=a%20%2F%2Bb$/);
 f.requests[1].resolve({entries:[{name:'second.txt',path:'second.txt',mod:0,size:1}],next_cursor:'',truncated:false});await flush();
 assert.match(f.nodes.get('#fileList').innerHTML,/last page/);
 assert.match(f.nodes.get('#fileList').innerHTML,/first.txt/);assert.match(f.nodes.get('#fileList').innerHTML,/second.txt/);
 f.events.get('#firstPageclick')();assert.doesNotMatch(f.requests[2].url,/cursor=/);
});
test('changed directory gets explicit refresh, disposed view ignores late pages',async()=>{
 const f=fixture();await f.open();f.requests[0].reject(Object.assign(Error('directory changed'),{status:409}));await flush();
 assert.match(f.nodes.get('#fileList').innerHTML,/Refresh changed directory/);
 f.events.get('#refreshListingclick')();const pending=f.requests[1];
 f.events.get('gss:teardown')();assert.equal(pending.options.signal.aborted,true);
 const before=f.nodes.get('#fileList').innerHTML;pending.resolve({entries:[],next_cursor:'late'});await flush();
 assert.equal(f.nodes.get('#fileList').innerHTML,before);
});
