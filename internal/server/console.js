"use strict";
const $ = s => document.querySelector(s);
let STATE = {nodes:[], over:[], sel:null};

function toast(msg, bad){const t=$("#toast");t.textContent=msg;t.className="toast show"+(bad?" bad":"");setTimeout(()=>t.className="toast",2600);}
async function api(method, path, body){
  const opt={method, headers:{}, credentials:"same-origin"};
  if(body!==undefined){opt.headers["Content-Type"]="application/json";opt.body=JSON.stringify(body);}
  const r=await fetch(path, opt);
  if(r.status===401){show("login");throw new Error("unauthorized");}
  const txt=await r.text();
  let data=null; try{data=txt?JSON.parse(txt):null;}catch(e){}
  if(!r.ok) throw new Error((data&&data.error&&data.error.message)||("HTTP "+r.status));
  return data;
}
function show(view){$("#login").classList.toggle("hidden",view!=="login");$("#app").classList.toggle("hidden",view!=="app");}
function esc(s){return String(s).replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));}
function fmtLimits(l){const p=[];if(l.max_usd_per_day)p.push("$"+l.max_usd_per_day+"/d");if(l.max_usd_per_min)p.push("$"+l.max_usd_per_min+"/m");if(l.max_concurrent)p.push(l.max_concurrent+" conc");if(l.max_usd_per_run)p.push("$"+l.max_usd_per_run+"/run");return p.join(" · ")||"—";}

// ---- boot ----
async function boot(){
  let info; try{info=await fetch("/console/info",{credentials:"same-origin"}).then(r=>r.json());}catch(e){info={providers:[],authenticated:false};}
  const sso=$("#ssoBtns"); sso.innerHTML="";
  (info.providers||[]).forEach(p=>{
    const a=document.createElement("a"); a.href="/console/auth/"+p+"/start"; a.className="btn"; a.style.textAlign="center";
    a.textContent="Continue with "+p.charAt(0).toUpperCase()+p.slice(1); sso.appendChild(a);
  });
  $("#ssoDivider").classList.toggle("hidden",!(info.providers||[]).length);
  if(info.authenticated){$("#who").textContent=info.name||"admin";show("app");load();}
  else show("login");
}
$("#localForm").addEventListener("submit", async e=>{
  e.preventDefault(); $("#loginErr").textContent="";
  try{ await api("POST","/console/login",{username:$("#u").value,password:$("#p").value}); boot(); }
  catch(err){ $("#loginErr").textContent="Invalid credentials or not authorized."; }
});
$("#logout").addEventListener("click", async()=>{try{await api("POST","/console/logout");}catch(e){}show("login");});
$("#addOrg").addEventListener("click",()=>{STATE.sel={_new:"org"};render();});

// ---- load + render ----
async function load(){
  const d=await api("GET","/v1/policy"); STATE.nodes=d.nodes||[]; STATE.over=d.over_allocations||[];
  if(STATE.sel&&!STATE.sel._new){STATE.sel=STATE.nodes.find(n=>n.type===STATE.sel.type&&n.id===STATE.sel.id)||null;}
  if(!STATE.sel&&STATE.nodes.length)STATE.sel=STATE.nodes[0];
  render();
}
function key(n){return n.type+":"+n.id;}
function render(){
  const orgs=STATE.nodes.filter(n=>n.type==="org"), teams=STATE.nodes.filter(n=>n.type==="team"), apps=STATE.nodes.filter(n=>n.type==="app");
  $("#kpis").innerHTML=[
    ["Nodes",STATE.nodes.length,""],["Teams",teams.length,""],
    ["Apps",apps.length,""],["Killed",STATE.nodes.filter(n=>n.killed).length,"crit"]
  ].map(([k,v,c])=>`<div class="kpi"><div class="k">${k}</div><div class="v ${c}">${v}</div></div>`).join("");

  // tree
  let t="";
  orgs.forEach(o=>{
    t+=nodeRow(o,0);
    teams.filter(x=>x.parent_id===o.id).forEach(tm=>{
      t+=nodeRow(tm,1);
      apps.filter(a=>a.parent_id===tm.id).forEach(a=>t+=nodeRow(a,2));
    });
  });
  $("#treeBox").innerHTML=t||`<div style="color:var(--faint);padding:10px 8px;font-size:13px">No orgs yet — add one.</div>`;
  document.querySelectorAll("#treeBox .node").forEach(el=>el.addEventListener("click",()=>{
    STATE.sel=STATE.nodes.find(n=>key(n)===el.dataset.k); render();
  }));
  renderDetail();
}
function nodeRow(n,depth){
  const sel=STATE.sel&&!STATE.sel._new&&key(STATE.sel)===key(n)?" sel":"";
  const tick=n.killed?"tick crit":"tick";
  return `<div class="node t${depth}${sel}${n.killed?' killed':''}" data-k="${key(n)}"><span class="${tick}"></span>${esc(n.name||n.id)} <span class="amt">${n.type==='app'?'$'+(n.limits.max_usd_per_run||0):(n.limits.max_usd_per_day?'$'+n.limits.max_usd_per_day+'/d':'')}</span></div>`;
}
function renderDetail(){
  const d=$("#detail");
  if(STATE.sel&&STATE.sel._new){ d.innerHTML=createForm(STATE.sel._new); wireCreate(); return; }
  const n=STATE.sel;
  if(!n){d.innerHTML="";return;}
  const l=n.limits;
  const childType=n.type==="org"?"team":n.type==="team"?"app":null;
  let over=STATE.over.filter(w=>w.startsWith(key(n))).map(w=>`<div class="warn">⚠ ${esc(w)}</div>`).join("");
  d.innerHTML=`
    <h2>${esc(n.name||n.id)} <span class="badge">${n.type}</span> ${n.killed?'<span class="badge" style="color:var(--crit);border-color:#6d2f31">killed</span>':''}</h2>
    <div class="path">${key(n)}${n.parent_id?" · under "+esc(n.parent_id):""}</div>
    ${over}
    <div class="grid">
      <div class="bcard"><div class="k">Daily budget</div><div class="amt">${l.max_usd_per_day?"$"+l.max_usd_per_day:"—"}</div></div>
      <div class="bcard"><div class="k">Velocity</div><div class="amt">${l.max_usd_per_min?"$"+l.max_usd_per_min+"/min":"—"}</div></div>
      <div class="bcard"><div class="k">Concurrency</div><div class="amt">${l.max_concurrent||"—"}</div></div>
      <div class="bcard"><div class="k">Per-run ceiling</div><div class="amt">${l.max_usd_per_run?"$"+l.max_usd_per_run:"—"}</div></div>
    </div>
    <div class="actions">
      <button class="btn sm" id="editLimits">Edit budget</button>
      ${childType?`<button class="btn sm" id="addChild">+ ${childType}</button>`:""}
      <button class="btn sm" id="issueKey">Issue key</button>
      <button class="btn sm ${n.killed?'':'danger'}" id="killBtn">${n.killed?"Un-kill":"Kill "+n.type}</button>
    </div>
    <div id="inlineForm"></div>`;
  $("#editLimits").addEventListener("click",()=>showLimitsForm(n));
  if(childType)$("#addChild").addEventListener("click",()=>{STATE.sel={_new:childType,parent:n.id};render();});
  $("#issueKey").addEventListener("click",()=>showKeyForm(n));
  $("#killBtn").addEventListener("click",()=>killNode(n));
}

function limitInputs(l){l=l||{};return `
  <div class="row"><div class="field"><label>$/day</label><input id="f_day" value="${l.max_usd_per_day||''}"></div>
  <div class="field"><label>$/min</label><input id="f_min" value="${l.max_usd_per_min||''}"></div></div>
  <div class="row"><div class="field"><label>max concurrent</label><input id="f_conc" value="${l.max_concurrent||''}"></div>
  <div class="field"><label>$/run</label><input id="f_run" value="${l.max_usd_per_run||''}"></div></div>`;}
function readLimits(){return {max_usd_per_day:+($("#f_day").value||0),max_usd_per_min:+($("#f_min").value||0),max_concurrent:+($("#f_conc").value||0),max_usd_per_run:+($("#f_run").value||0)};}

function createForm(type){
  const parentField=type==="team"?`<div class="field"><label>Org id</label><input id="c_parent" value="${STATE.sel.parent||''}"></div>`
    :type==="app"?`<div class="field"><label>Team id</label><input id="c_parent" value="${STATE.sel.parent||''}"></div>`:"";
  return `<h2>New ${type}</h2><div class="path">create a ${type}</div>
    <div class="form">
      <div class="row"><div class="field"><label>id</label><input id="c_id"></div><div class="field"><label>name</label><input id="c_name"></div></div>
      ${parentField?'<div class="row">'+parentField+'<div></div></div>':""}
      ${limitInputs({})}
      <div class="actions"><button class="btn primary sm" id="createBtn">Create</button><button class="btn sm" id="cancelBtn">Cancel</button></div>
    </div>`;
}
function wireCreate(){
  $("#cancelBtn").addEventListener("click",()=>{STATE.sel=STATE.nodes[0]||null;render();});
  $("#createBtn").addEventListener("click",async()=>{
    const type=STATE.sel._new, body={id:$("#c_id").value,name:$("#c_name").value,limits:readLimits()};
    let path;
    if(type==="org")path="/v1/policy/orgs";
    else if(type==="team"){path="/v1/policy/teams";body.org_id=$("#c_parent").value;}
    else{path="/v1/policy/apps";body.team_id=$("#c_parent").value;}
    try{await api("POST",path,body);toast(type+" created");STATE.sel={type,id:body.id};load();}
    catch(e){toast(e.message,true);}
  });
}
function showLimitsForm(n){
  $("#inlineForm").innerHTML=`<div class="sect">Edit budget</div><div class="form">${limitInputs(n.limits)}
    <div class="actions"><button class="btn primary sm" id="saveLimits">Save</button></div></div>`;
  $("#saveLimits").addEventListener("click",async()=>{
    try{await api("PUT","/v1/policy/nodes/"+n.type+"/"+encodeURIComponent(n.id)+"/limits",readLimits());toast("budget updated");load();}
    catch(e){toast(e.message,true);}
  });
}
function showKeyForm(n){
  $("#inlineForm").innerHTML=`<div class="sect">Issue key → ${key(n)}</div><div class="form">
    <div class="field"><label>API key (raw bearer — hashed server-side)</label><input id="k_key" placeholder="sk-..."></div>
    <div class="actions"><button class="btn primary sm" id="saveKey">Issue</button></div></div>`;
  $("#saveKey").addEventListener("click",async()=>{
    try{const r=await api("POST","/v1/policy/keys",{key:$("#k_key").value,node_type:n.type,node_id:n.id});toast("key issued ("+(r.key_sha256||"").slice(0,12)+"…)");$("#inlineForm").innerHTML="";}
    catch(e){toast(e.message,true);}
  });
}
async function killNode(n){
  const verb=n.killed?"unkill":"kill";
  try{await api("POST","/v1/policy/nodes/"+n.type+"/"+encodeURIComponent(n.id)+"/"+verb);toast(n.type+" "+verb+"ed");load();}
  catch(e){toast(e.message,true);}
}

boot();
