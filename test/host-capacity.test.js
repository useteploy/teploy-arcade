const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const source=fs.readFileSync('cmd/teploy-arcade/frontend/app.js','utf8');
function fn(name){return source.match(new RegExp(`^(?:async )?function ${name}\\([^]*?^\\}`,'m'))[0];}
function fixture(){
 const c={esc:value=>String(value).replaceAll('"','&quot;')};vm.createContext(c);
 vm.runInContext(source.match(/^const capKnown = .*;$/m)[0]+'\n'+source.match(/^const capPct = .*;$/m)[0]+'\n'+['usageKnown','fmtMB','scopeLabel','measurementTitle','runtimeCapacity','capacityLabel','memoryScopesComparable','hostTiles'].map(fn).join('\n'),c);return c;
}
function host(){return{cpu:{used_percent:null,total_vcpu:2,allocated_vcpu:1,scope:'panel_host',source:'counters',sampled_at:'2026-10-07T00:00:00Z'},memory:{used_mb:null,total_mb:2048,allocated_mb:1024,usage:{scope:'panel_cgroup',source:'memory.current',sampled_at:'2026-10-07T00:00:00Z'}},disk:{used_gb:0,total_gb:0},running:0,servers:1,docker:true};}
test('missing CPU/memory usage stays unknown and has no usage bars',()=>{
 const c=fixture(),html=c.hostTiles(host());
 assert.match(html,/Host CPU<\/div>\s*<div class="v">-<\/div>/);
 assert.match(html,/Panel memory<\/div>\s*<div class="v">—/);
 assert.doesNotMatch(html,/0\.0|NaN|null|class="bar"/);
 assert.match(html,/Scope: panel cgroup · Source: memory.current/);
});
test('a real zero sample remains zero; stale CPU is suppressed',()=>{
 const c=fixture(),h=host();h.cpu.used_percent=0;h.memory.used_mb=0;
 assert.match(c.hostTiles(h),/0<small> %/);assert.match(c.hostTiles(h),/0\.0<small> GB used/);
 h.cpu.stale=true;h.cpu.used_percent=75;const html=c.hostTiles(h);assert.doesNotMatch(html,/75<small>/);assert.match(html,/stale/);
});
test('proc-visible host memory is never drawn as percent of panel quota',()=>{
 const c=fixture(),h=host();h.memory.used_mb=32768;h.memory.usage.scope='proc_visible_host';
 const html=c.hostTiles(h);assert.match(html,/32\.0<small> GB used/);assert.match(html,/host visible through \/proc · Panel limit: 2\.0 GB/);
 assert.doesNotMatch(html,/class="bar"/);assert.doesNotMatch(html,/32\.0.*\/ 2 GB/);
});
test('Docker forms use known daemon capacity rather than panel quota',()=>{
 const c=fixture(),h=host();h.docker_capacity={known:true,total_mb:65536,total_vcpu:16,scope:'docker_daemon',source:'Docker Engine info'};
 assert.equal(c.runtimeCapacity(h,'docker').total_mb,65536);assert.equal(c.runtimeCapacity(h,'docker').total_vcpu,16);
 assert.equal(c.runtimeCapacity(h,'sim').total_mb,2048);assert.equal(c.runtimeCapacity(h,'sim').total_vcpu,2);
 assert.match(c.capacityLabel(h,'docker'),/Docker daemon capacity: 16 vCPU · 64\.0 GB/);
});
test('unknown Docker capacity does not fall back to panel quota or invent zero',()=>{
 const c=fixture(),h=host();h.docker_capacity={known:false,total_mb:0,total_vcpu:0};
 assert.equal(c.runtimeCapacity(h,'docker').total_mb,null);assert.equal(c.runtimeCapacity(h,'docker').total_vcpu,null);
 assert.equal(c.capacityLabel(h,'docker'),'Docker daemon capacity: CPU unknown · memory unknown');
});
test('invalid capacity and usage numbers are not measurements',()=>{
 const c=fixture();assert.equal(c.fmtMB(null),'—');assert.equal(c.fmtMB(NaN),'—');assert.equal(c.fmtMB(0),'0 MB');
 assert.equal(vm.runInContext('capKnown(Infinity)',c),false);
});
test('create runtime switching refreshes budget and individual capacity gate',async()=>{
 const c=fixture(),h=host();h.docker_capacity={known:true,total_mb:65536,total_vcpu:16};
 class El {constructor(){this.nodes={};this.listeners={};this.value='';this.classList={contains:()=>false,toggle(){}};}querySelector(sel){return this.nodes[sel]||(this.nodes[sel]=new El());}querySelectorAll(){return [];}addEventListener(name,fn){this.listeners[name]=fn;}appendChild(){}remove(){}}
 const modal=new El(),holder=new El();
 c.state={host:h};c.h=()=>modal;c.$=(sel,root)=>root?root.querySelector(sel):holder;
 c.api=async()=>({docker:true,templates:[{recommended:true,slug:'paper',name:'Paper',group:'Java',mark:'paper',versions:['1.0'],memory_mb:4096,cpu:4,disk_gb:0,image:'fixture'}]});
 c.window={extraViews:{}};c.toast=()=>{};
 vm.runInContext(source.match(/^const capOver = .*;$/m)[0]+'\n'+fn('openCreate'),c);
 await c.openCreate();assert.match(modal.querySelector('#budget').innerHTML,/panel process limit capacity/);assert.equal(modal.querySelector('#createBtn').disabled,true);
 modal.querySelector('#rtDocker').listeners.click();assert.match(modal.querySelector('#budget').innerHTML,/Docker daemon capacity: 16 vCPU · 64\.0 GB/);assert.equal(modal.querySelector('#createBtn').disabled,false);
 h.docker_capacity.known=false;modal.querySelector('#rtDocker').listeners.click();assert.match(modal.querySelector('#budget').innerHTML,/CPU unknown · memory unknown/);assert.equal(modal.querySelector('#createBtn').disabled,false);assert.doesNotMatch(modal.querySelector('#budget').innerHTML,/class="bar"/);
});
