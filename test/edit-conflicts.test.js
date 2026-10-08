const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
function fn(file,name){return fs.readFileSync(`cmd/teploy-arcade/frontend/${file}`,'utf8').match(new RegExp(`^(?:async )?function ${name}\\([^]*?^\\}`,'m'))[0];}
class El{
 constructor(){this.listeners={};this.nodes={};this.dataset={};this.value='';this.isConnected=true;this.classList={toggle(){},contains(){return false;}};}
 addEventListener(e,fn){this.listeners[e]=fn;}
 querySelector(sel){return this.nodes[sel]||(this.nodes[sel]=new El());}
 querySelectorAll(sel){return sel==='select[data-key], input[data-key]'?[this.field]:[];}
 remove(){this.isConnected=false;}
 appendChild(){}
 focus(){}
}
function fixture(name,file){
 const root=new El(),requests=[],conflicts=[],routes=[],toasts=[];
 root.field=new El();root.field.dataset.key='motd';
 const context={state:{host:null},h:()=>root,$:(sel,host=root)=>host.querySelector(sel),api:(url,opts={})=>new Promise((resolve,reject)=>requests.push({url,opts,resolve,reject})),esc:String,serverHeader:()=>'',wireHeaderActions(){},showEditConflict:(...args)=>conflicts.push(args),toast:(...args)=>toasts.push(args),router:()=>routes.push('router'),window:{addEventListener(){},removeEventListener(){},extraViews:{}},setTimeout(){},refreshServers(){}};
 vm.createContext(context);
 const app=fs.readFileSync('cmd/teploy-arcade/frontend/app.js','utf8');
 const helpers=['usageKnown','fmtMB','scopeLabel','measurementTitle','runtimeCapacity','capacityLabel'].map(n=>fn('app.js',n)).join('\n');
 vm.runInContext(app.match(/^const capKnown = .*;$/m)[0]+'\n'+app.match(/^const capOver = .*;$/m)[0]+'\n'+helpers+'\n'+fn(file,name),context);
 return {root,requests,conflicts,routes,toasts,context};
}
test('file conflict keeps draft and original ETag; explicit reload adopts new version',async()=>{
 const f=fixture('openEditor','views.js');const opened=f.context.openEditor('s','world/config');
 f.requests[0].resolve({content:'original',etag:'"old"'});await opened;
 const editor=f.root.querySelector('#ed');editor.value='my draft';
 const saving=f.root.querySelector('#save').listeners.click();
 assert.equal(f.requests[1].opts.headers['If-Match'],'"old"');
 assert.equal(JSON.parse(f.requests[1].opts.body).content,'my draft');
 f.requests[1].reject(Object.assign(Error('conflict'),{status:412}));await saving;
 assert.equal(editor.value,'my draft');assert.equal(f.root.isConnected,true);assert.equal(f.conflicts.length,1);
 const [host,load,describe,reload]=f.conflicts[0];assert.equal(describe({content:'theirs'}),'theirs');
 reload({content:'theirs',etag:'"new"'});assert.equal(editor.value,'theirs');
 const retry=f.root.querySelector('#save').listeners.click();assert.equal(f.requests[2].opts.headers['If-Match'],'"new"');
 f.requests[2].resolve({});await retry;assert.equal(f.root.isConnected,false);
});
test('settings conflict preserves changed fields and original ETag without restarting',async()=>{
 const f=fixture('viewSettings','app.js');const opened=f.context.viewSettings('s');
 const server={memory:{limit_mb:100,heap_mb:80},cpu:{limit_vcpu:1}};
 f.requests[0].resolve({server,etag:'"settings-old"',groups:[{group:'general',keys:[{key:'motd',label:'MOTD',type:'string',value:'old',applies:'next_restart'}]}]});await opened;
 f.root.field.value='draft motd';f.root.field.listeners.input();
 const saving=f.root.querySelector('#saveRestartBtn').listeners.click();
 assert.equal(f.requests[1].opts.headers['If-Match'],'"settings-old"');
 assert.deepEqual(JSON.parse(f.requests[1].opts.body),{motd:'draft motd'});
 f.requests[1].reject(Object.assign(Error('conflict'),{status:412}));await saving;
 assert.equal(f.root.field.value,'draft motd');assert.equal(f.requests.length,2);assert.equal(f.routes.length,0);assert.equal(f.conflicts.length,1);
 assert.equal(f.conflicts[0][2]({groups:[{keys:[{key:'motd',value:'new'}]}]}),'motd=new');
});
test('comparison leaves draft untouched; reload requires confirmation',async()=>{
 const root=new El(),panel=new El();let reloads=0,reads=0;
 panel.querySelector('[data-current]').value='';
 const context={h:()=>panel,confirm:()=>false};vm.createContext(context);vm.runInContext(fn('app.js','showEditConflict'),context);
 root.querySelector=()=>null;
 context.showEditConflict(root,async()=>{reads++;return{content:'current'};},x=>x.content,()=>reloads++);
 await panel.querySelector('[data-compare]').listeners.click();
 assert.equal(panel.querySelector('[data-current]').value,'current');assert.equal(reads,1);assert.equal(reloads,0);
 await panel.querySelector('[data-reload]').listeners.click();assert.equal(reads,1);assert.equal(reloads,0);
 context.confirm=()=>true;await panel.querySelector('[data-reload]').listeners.click();assert.equal(reloads,1);
});
test('resource form validates against daemon capacity instead of panel quota',async()=>{
 const f=fixture('viewSettings','app.js');
 f.context.state.host={memory:{total_mb:2048},cpu:{total_vcpu:2},docker_capacity:{known:true,total_mb:65536,total_vcpu:16}};
 const opened=f.context.viewSettings('s');
 f.requests[0].resolve({server:{runtime:'docker',memory:{limit_mb:4096,heap_mb:3000},cpu:{limit_vcpu:4}},groups:[]});await opened;
 assert.match(f.root.innerHTML,/max="65536"/);assert.match(f.root.innerHTML,/max="16"/);
 f.root.querySelector('#resMem').value='4096';f.root.querySelector('#resCpu').value='4';
 const saving=f.root.querySelector('#resSave').listeners.click();assert.equal(f.requests.length,2);f.requests[1].resolve({});await saving;
 f.root.querySelector('#resCpu').value='17';await f.root.querySelector('#resSave').listeners.click();assert.equal(f.requests.length,2);assert.match(f.toasts.at(-1)[0],/Docker daemon capacity/);
});
