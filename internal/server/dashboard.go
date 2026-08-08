package server

import "net/http"

// The built-in live dashboard: one static HTML page (no external assets,
// no build step) served unauthenticated — it contains no data. The page
// asks for the admin token once, then shows two views:
//
//	overview — every machine as a card with up/down status and live bars,
//	           polled from /admin/machines + /admin/live
//	detail   — click a machine to stream its samples over the SSE endpoint
//	           using fetch (EventSource can't set the Authorization header)
//
// Fleet health is deliberately global, not per-view: the header badge and
// the document title carry the down-count in both views, so a machine
// going offline is visible while drilled into a different one. Charts are
// hand-rolled canvas, netdata-style rolling window.
func (s *AdminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>fleetcore</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
/* Theme. Light is the base palette so every colour has exactly one
   unconditional definition; the dark values are then applied twice — once
   for the system preference (unless the viewer has explicitly chosen
   light) and once for an explicit dark choice — so the toggle wins in
   both directions. Canvas colours are read back from these same tokens at
   draw time, since a canvas cannot inherit CSS. */
:root{
  color-scheme:light dark;
  --bg:#f4f6f8; --panel:#ffffff; --line:#dde2e8; --text:#1a1f26; --muted:#5b636c;
  --sunken:#eef1f5; --track:#e1e6ec; --chip:#e9edf2; --grid:#e5e9ef;
  --accent:#0b8f6a; --danger:#c8342b; --warn:#9a5b00; --blue:#3350c8; --pink:#b53a72;
  --btn-fg:#ffffff; --down-border:#e9b7b4;
  --ok-bg:#e6f5ef; --bad-bg:#fdeceb; --warn-bg:#fbf0dd; --info-bg:#e8ecfa;
}
@media (prefers-color-scheme:dark){
  :root:not([data-theme="light"]){
    --bg:#101216; --panel:#181b21; --line:#2a2e36; --text:#d6d9de; --muted:#8b909a;
    --sunken:#12151a; --track:#22262d; --chip:#22262d; --grid:#262a32;
    --accent:#4fd1a5; --danger:#f56565; --warn:#f6ad55; --blue:#7f9cf5; --pink:#f687b3;
    --btn-fg:#08281d; --down-border:#4a2b2e;
    --ok-bg:#16281f; --bad-bg:#2a1a1c; --warn-bg:#2a2410; --info-bg:#1d2530;
  }
}
:root[data-theme="dark"]{
  --bg:#101216; --panel:#181b21; --line:#2a2e36; --text:#d6d9de; --muted:#8b909a;
  --sunken:#12151a; --track:#22262d; --chip:#22262d; --grid:#262a32;
  --accent:#4fd1a5; --danger:#f56565; --warn:#f6ad55; --blue:#7f9cf5; --pink:#f687b3;
  --btn-fg:#08281d; --down-border:#4a2b2e;
  --ok-bg:#16281f; --bad-bg:#2a1a1c; --warn-bg:#2a2410; --info-bg:#1d2530;
}
*{box-sizing:border-box;margin:0}
body{background:var(--bg);color:var(--text);font:14px/1.5 system-ui,-apple-system,sans-serif;padding:18px;max-width:1500px;margin:0 auto}
header{display:flex;align-items:center;gap:10px;margin-bottom:16px;flex-wrap:wrap}
h1{font-size:16px;font-weight:600;letter-spacing:.01em}
.spacer{flex:1}
input{background:var(--panel);border:1px solid var(--line);color:var(--text);padding:7px 10px;border-radius:6px;font-size:13px}
button{background:var(--accent);border:0;color:var(--btn-fg);font-weight:600;padding:7px 14px;border-radius:6px;cursor:pointer;font-size:13px}
button.ghost{background:transparent;border:1px solid var(--line);color:var(--text);font-weight:500}
button.ghost:hover{border-color:var(--accent)}
#status{color:var(--muted);font-size:12px}
#status.err{color:var(--danger)}
.hidden{display:none !important}

/* fleet health badge — visible in BOTH views */
#health{display:flex;align-items:center;gap:6px;font-size:12px;padding:5px 11px;border-radius:14px;
background:var(--panel);border:1px solid var(--line);cursor:pointer;white-space:nowrap}
#health.bad{border-color:var(--danger);color:var(--danger)}
#health.bad .dot{background:var(--danger)}

/* offline event log */
#events{margin-bottom:14px;display:flex;flex-direction:column;gap:6px}
.event{display:flex;align-items:center;gap:10px;background:var(--bad-bg);border:1px solid var(--danger);
border-radius:8px;padding:8px 12px;font-size:13px}
.event.ok{background:var(--ok-bg);border-color:var(--accent)}
.event .t{color:var(--muted);font-size:11px;margin-left:auto;font-variant-numeric:tabular-nums}
.event button{padding:2px 8px;font-size:11px}

.summary{display:flex;gap:26px;flex-wrap:wrap;background:var(--panel);border:1px solid var(--line);
border-radius:10px;padding:12px 18px;margin-bottom:16px}
.stat .k{font-size:11px;color:var(--muted);text-transform:uppercase;letter-spacing:.05em}
.stat .v{font-size:21px;font-weight:600;font-variant-numeric:tabular-nums}
.stat .v.bad{color:var(--danger)}

.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(310px,1fr));gap:13px}
/* Column layout with a fixed rhythm: every card must put cpu/mem/disk and
   the sparkline at the same y, or the grid reads as ragged. Anything
   variable-length (the os/arch line, the label chips) is pinned to one
   line or given reserved space rather than being allowed to push rows
   down on some cards and not others. */
.mcard{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px;
cursor:pointer;transition:border-color .15s;display:flex;flex-direction:column}
.mcard:hover{border-color:var(--accent)}
.mcard.down{border-color:var(--down-border)}
.mhead{display:flex;align-items:center;gap:8px}
.dot{width:8px;height:8px;border-radius:50%;flex:none;background:var(--accent)}
.dot.up{box-shadow:0 0 7px var(--accent)}
.dot.down{background:var(--danger);box-shadow:none}
.mname{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.mseen{margin-left:auto;font-size:11px;color:var(--muted);flex:none;font-variant-numeric:tabular-nums;white-space:nowrap}
.mseen.down{color:var(--danger)}
.mseen.live{color:var(--accent)}
/* Removal is destructive, so it stays quiet until you reach for it — but a
   machine that is down is exactly the one you came to remove, so show it. */
.mrm{opacity:.45;transition:opacity .15s;padding:1px 7px;font-size:11px;line-height:1.4;flex:none}
.mcard:hover .mrm,.mcard.down .mrm{opacity:1}
.mrm:hover{border-color:var(--danger);color:var(--danger)}
.mip{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:var(--blue);margin:1px 0 0 16px}
.mip .noip{color:var(--muted);font-style:italic;font-family:system-ui,sans-serif}
.msub{color:var(--muted);font-size:12px;margin:3px 0 11px 16px;
white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.bars{display:flex;flex-direction:column;gap:7px}
.brow{display:flex;align-items:center;gap:8px;font-size:12px}
.blabel{color:var(--muted);width:34px;flex:none}
/* Both must be block-level: they are spans, and an inline element ignores
   width/height entirely — which is why the usage bars rendered as empty
   tracks no matter what the value was. */
.btrack{display:block;flex:1;height:6px;background:var(--track);border-radius:3px;overflow:hidden}
.bfill{display:block;height:100%;width:0;border-radius:3px;transition:width .5s linear}
.spark{width:100%;height:34px;display:block;margin-top:10px}
/* auto margin pins the footer to the card bottom so unequal content above
   cannot leave one card's chips floating mid-card */
.mcard .chips{margin-top:auto;padding-top:11px;min-height:31px}
.bval{width:112px;text-align:right;color:var(--muted);flex:none;font-variant-numeric:tabular-nums;white-space:nowrap}
.chips{margin-top:11px;display:flex;gap:5px;flex-wrap:wrap}
.chip{background:var(--chip);color:var(--muted);font-size:11px;padding:2px 8px;border-radius:10px}
.nodata{color:var(--muted);font-size:12px;padding:14px 0;text-align:center}

.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(330px,1fr));gap:13px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:12px}
.card h2{font-size:12px;font-weight:600;color:var(--muted);text-transform:uppercase;
letter-spacing:.05em;display:flex;justify-content:space-between;gap:10px}
.card h2 span{color:var(--text);font-variant-numeric:tabular-nums}
canvas{width:100%;height:118px;display:block;margin-top:8px}
table{width:100%;border-collapse:collapse;font-size:12px;margin-top:6px}
th{color:var(--muted);text-align:left;font-weight:500;padding:4px 8px}
td{padding:4px 8px;border-top:1px solid var(--line);font-variant-numeric:tabular-nums}
td.name{font-variant-numeric:normal}
.kv{display:grid;grid-template-columns:auto 1fr;gap:5px 16px;font-size:12px;margin-top:6px}
.kv b{color:var(--muted);font-weight:500}
.detail-head{display:flex;align-items:center;gap:10px;margin-bottom:14px;flex-wrap:wrap}
.detail-head h2{font-size:15px;font-weight:600}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px;color:var(--muted)}

/* command console */
.console{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px;margin-bottom:16px}
.console h2{font-size:12px;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:9px}
.crow{display:flex;gap:9px;align-items:flex-start;flex-wrap:wrap}
textarea{flex:1;min-width:260px;min-height:62px;background:var(--sunken);border:1px solid var(--line);color:var(--text);
padding:9px 11px;border-radius:6px;font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;resize:vertical}
textarea:focus{outline:none;border-color:var(--accent)}
select{background:var(--sunken);border:1px solid var(--line);color:var(--text);padding:7px 10px;border-radius:6px;font-size:13px}
.chint{color:var(--muted);font-size:11px;margin-top:7px}
.cmsg{font-size:12px;margin-top:7px}
.cmsg.ok{color:var(--accent)} .cmsg.err{color:var(--danger)}

.cmdrow{border-top:1px solid var(--line);padding:9px 2px}
.cmdrow:first-child{border-top:0}
.cmdhead{display:flex;align-items:center;gap:9px;font-size:12px;flex-wrap:wrap}
.badge{font-size:10px;padding:2px 7px;border-radius:9px;text-transform:uppercase;letter-spacing:.04em;font-weight:600}
.badge.applied{background:var(--ok-bg);color:var(--accent)}
.badge.failed{background:var(--bad-bg);color:var(--danger)}
.badge.queued{background:var(--warn-bg);color:var(--warn)}
.cmdscript{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11.5px;color:var(--text);
background:var(--sunken);border-radius:5px;padding:7px 9px;margin-top:6px;white-space:pre-wrap;word-break:break-word}
.cmdout{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11.5px;color:var(--muted);
background:var(--sunken);border-left:2px solid var(--line);padding:7px 9px;margin-top:5px;white-space:pre-wrap;
word-break:break-word;max-height:220px;overflow:auto}
.cmdtime{margin-left:auto;color:var(--muted);font-size:11px;font-variant-numeric:tabular-nums}
.cmdbtn{padding:2px 9px;font-size:11px}
.cmdbtn[disabled]{opacity:.5;cursor:default}
.gchip{background:var(--info-bg);color:var(--blue);font-size:11px;padding:2px 8px;border-radius:10px}
.gchip .via{color:var(--muted)}
</style></head><body>

<header>
  <h1>fleetcore</h1>
  <div id="health" class="hidden" title="Click to return to the fleet overview">
    <span class="dot"></span><span id="health-text"></span>
  </div>
  <div class="spacer"></div>
  <button id="theme" class="ghost" title="Switch between system, light and dark"></button>
  <input id="token" type="password" placeholder="admin token" size="26">
  <button id="connect">Connect</button>
  <button id="forget" class="ghost hidden">Sign out</button>
  <span id="status"></span>
</header>

<div id="events"></div>

<section id="overview" class="hidden">
  <div class="summary" id="summary"></div>
  <div class="crow" style="margin-bottom:14px">
    <input id="filter" placeholder="filter by name, IP, OS, label or k8s role" style="flex:1;min-width:240px">
    <span id="filter-count" style="color:var(--muted);font-size:12px;align-self:center"></span>
  </div>
  <div class="console">
    <h2>run command on a group</h2>
    <div class="crow">
      <select id="g-target"></select>
      <input id="g-selector" placeholder="or labels: role=gpu region=hk" style="min-width:210px">
      <textarea id="g-script" placeholder="uptime&#10;df -h /"></textarea>
      <button id="g-run">Run</button>
    </div>
    <div class="chint">Runs as root via /bin/sh, working directory <b>/</b>. Agents <b>pull</b> it on their next
      reconcile &mdash; usually within a second &mdash; so machines behind NAT need no inbound access.
      Fill the label box to target every machine matching <i>all</i> those labels, across groups; leave it empty to use the group.</div>
    <div id="g-msg" class="cmsg"></div>
  </div>
  <div class="grid" id="mgrid"></div>
</section>

<section id="detail" class="hidden">
  <div class="detail-head">
    <button id="back" class="ghost">&larr; Fleet</button>
    <button id="d-remove" class="ghost mrm" style="opacity:1;order:99">Remove machine</button>
    <h2 id="d-name"></h2>
    <span id="d-state"></span>
    <span id="d-id" class="mono"></span>
  </div>
  <div id="d-groups" class="chips" style="margin:-6px 0 14px"></div>
  <div class="cards">
    <div class="card"><h2>cpu <span id="v-cpu"></span></h2><canvas id="c-cpu"></canvas></div>
    <div class="card"><h2>memory <span id="v-mem"></span></h2><canvas id="c-mem"></canvas></div>
    <div class="card"><h2>network <span id="v-net"></span></h2><canvas id="c-net"></canvas></div>
    <div class="card"><h2>disk <span id="v-disk"></span></h2><canvas id="c-disk"></canvas></div>
  </div>
  <div class="console" style="margin-top:13px">
    <h2>run command on this machine</h2>
    <div class="crow">
      <textarea id="m-script" placeholder="systemctl status fleet-agent&#10;journalctl -u fleet-agent -n 20"></textarea>
      <button id="m-run">Run</button>
    </div>
    <div class="chint">Queued into this machine's override. It overrides a group command of the same name.</div>
    <div id="m-msg" class="cmsg"></div>
  </div>
  <div id="queue-section" class="card hidden" style="margin-bottom:13px">
    <h2>pending &mdash; waiting for the agent to pull <span id="v-queued"></span></h2>
    <div id="queuelist"></div>
  </div>
  <div class="card" style="margin-bottom:13px"><h2>command history <span id="v-cmds"></span></h2>
    <div id="cmdlist"></div>
  </div>
  <div id="pod-section" class="card hidden" style="margin-top:13px">
    <h2>pods <span id="v-pods"></span></h2>
    <table><thead><tr><th>namespace</th><th>pod</th><th>containers</th>
      <th style="text-align:right">cpu</th><th style="text-align:right">memory</th></tr></thead>
      <tbody id="podbody"></tbody></table>
  </div>
  <div class="card" style="margin-top:13px"><h2>processes <span id="v-procs"></span></h2>
    <table><thead><tr><th>pid</th><th>process</th><th>service</th>
      <th style="text-align:right">cpu</th><th style="text-align:right">mem</th></tr></thead>
      <tbody id="procbody"></tbody></table>
    <div id="procnone" class="nodata hidden">no live sample &mdash; machine is not reporting</div>
  </div>
  <div class="card" style="margin-top:13px"><h2>network interfaces <span id="v-ifaces"></span></h2>
    <table><thead><tr><th>interface</th><th>addresses</th><th>mac</th>
      <th style="text-align:right">link</th><th style="text-align:right">rx</th><th style="text-align:right">tx</th></tr></thead>
      <tbody id="ifacebody"></tbody></table>
  </div>
  <div id="gpu-section" class="card hidden" style="margin-top:13px">
    <h2>gpus <span id="v-gpu"></span></h2><div id="gpubody" style="margin-top:6px;font-size:12px"></div>
  </div>
  <div class="cards" style="margin-top:13px">
    <div class="card"><h2>inventory</h2><div class="kv" id="d-inv"></div></div>
    <div class="card"><h2>modules</h2><div id="d-mods"></div></div>
  </div>
</section>

<script>
"use strict";
var WINDOW=120, DOWN_AFTER=120;
// One rolling history per machine, fed by BOTH the fleet poll and the
// per-machine SSE stream. Because the poll runs for every machine all the
// time, a machine already has history before you ever open it — clicking
// in draws instantly and keeps going, instead of blanking the charts and
// refilling them from scratch. Samples are keyed by at_unix so the two
// sources cannot double-count the same reading.
var hist={};
function histFor(id){
  if(!hist[id])hist[id]={at:[],cpu:[],mem:[],rx:[],tx:[],disk:[],lastAt:0,last:null};
  return hist[id];
}
function ingest(id,s,fromStream){
  if(!s||!s.at_unix)return false;
  var h=histFor(id);
  // The SSE stream emits exactly one frame per agent push, so while it is
  // connected it is the sole writer for that machine and every frame is
  // kept. The timestamp check applies only to the poll, which can hand
  // back the same sample twice — and must NOT be applied to the stream,
  // because at_unix has second resolution and sub-second sampling would
  // then lose every reading after the first in each second.
  if(!fromStream){
    if(streaming&&id===selected)return false; // stream owns this machine
    if(s.at_unix<=h.lastAt)return false;
  }
  h.lastAt=Math.max(h.lastAt,s.at_unix); h.last=s;
  push(h.at,s.at_unix); push(h.cpu,s.cpu_percent); push(h.mem,s.mem_used);
  push(h.rx,s.net_rx_bps); push(h.tx,s.net_tx_bps); push(h.disk,s.disk_used);
  return true;
}
// The control plane deliberately keeps only the newest sample per machine
// (durable series are Prometheus's job), so there is no server-side
// history to re-fetch after a browser reload — the charts would restart
// empty every time. Persisting the client's own rolling buffer makes a
// refresh resume instead of reset. Anything older than STALE_HIST is
// discarded so a tab reopened tomorrow does not show yesterday's line as
// if it were current.
var HIST_KEY="fleet_hist", STALE_HIST=120;
function saveHist(){
  try{
    var out={};
    Object.keys(hist).forEach(function(id){
      var h=hist[id];
      out[id]={at:h.at,cpu:h.cpu,mem:h.mem,rx:h.rx,tx:h.tx,disk:h.disk,lastAt:h.lastAt,last:h.last};
    });
    localStorage.setItem(HIST_KEY,JSON.stringify({v:1,saved:now(),hist:out}));
  }catch(e){/* quota or private mode: history is a nicety, never fatal */}
}
function loadHist(){
  try{
    var raw=localStorage.getItem(HIST_KEY); if(!raw)return;
    var d=JSON.parse(raw); if(!d||d.v!==1||!d.hist)return;
    var t=now();
    Object.keys(d.hist).forEach(function(id){
      var h=d.hist[id];
      if(!h||!h.lastAt||t-h.lastAt>STALE_HIST)return; // too old to be meaningful
      hist[id]=h;
    });
  }catch(e){}
}
var tok="", machines=[], live={}, selected=null, ctrl=null, poller=null, streaming=false;
var prevUp={};   // machine id -> was up, for offline/online transitions
var cards={};    // machine id -> cached DOM refs, so the grid updates in place

function $(id){return document.getElementById(id)}

/* ---- theme ----
   Three states, matching how browsers actually work: "system" sets no
   attribute and lets prefers-color-scheme decide; "light"/"dark" stamp
   data-theme and win over it. A canvas cannot inherit CSS, so chart
   colours are read back from the same custom properties and re-read
   whenever the effective theme changes. */
var THEME_KEY="fleet_theme", themePref="system", pal=null;
function cssVar(name){
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}
function palette(){
  if(!pal){
    pal={accent:cssVar("--accent"),blue:cssVar("--blue"),warn:cssVar("--warn"),
         pink:cssVar("--pink"),grid:cssVar("--grid")};
  }
  return pal;
}
function applyTheme(){
  var root=document.documentElement;
  if(themePref==="system")root.removeAttribute("data-theme");
  else root.setAttribute("data-theme",themePref);
  pal=null;                       // tokens changed; drop the cached palette
  $("theme").textContent=themePref==="system"?"Auto":themePref==="dark"?"Dark":"Light";
  // Repaint anything canvas-based, which does not restyle itself.
  if(selected)redraw(histFor(selected).last);
  Object.keys(cards).forEach(function(id){
    if(hist[id])drawSpark(cards[id].spark,hist[id].cpu);
  });
}
function setTheme(p){
  themePref=p;
  try{localStorage.setItem(THEME_KEY,p)}catch(e){}
  applyTheme();
}
(function initTheme(){
  try{themePref=localStorage.getItem(THEME_KEY)||"system"}catch(e){}
  // Following the system while it changes underneath us needs a listener;
  // the media query does the styling, this just refreshes canvas colours.
  if(window.matchMedia){
    var mq=window.matchMedia("(prefers-color-scheme: dark)");
    var onChange=function(){if(themePref==="system")applyTheme()};
    mq.addEventListener?mq.addEventListener("change",onChange):mq.addListener(onChange);
  }
})();
function esc(s){var d=document.createElement("div");d.textContent=s==null?"":String(s);return d.innerHTML}
function push(a,v){a.push(v); if(a.length>WINDOW)a.shift()}
function fmtB(b){var u=["B","KiB","MiB","GiB","TiB"],i=0;
  while(b>=1024&&i<u.length-1){b/=1024;i++}return b.toFixed(b<10&&i>0?1:0)+u[i]}
function fmtAge(s){s=Math.max(0,Math.round(s));
  if(s<60)return s+"s ago"; if(s<3600)return Math.floor(s/60)+"m ago";
  if(s<86400)return Math.floor(s/3600)+"h ago"; return Math.floor(s/86400)+"d ago"}
function fmtUp(s){s=Math.round(s);
  if(s<3600)return Math.floor(s/60)+"m"; if(s<86400)return Math.floor(s/3600)+"h";
  return Math.floor(s/86400)+"d "+Math.floor((s%86400)/3600)+"h"}
function fmtSecs(s){s=Math.max(0,Math.round(s));
  return s<60?s+"s":s<3600?Math.floor(s/60)+"m":Math.floor(s/3600)+"h"}
function now(){return Date.now()/1000}
// last_seen advances only on the heartbeat (30s by default); live metric
// pushes deliberately skip the store, so a streaming machine can show a
// heartbeat age of nearly 30s. A fresh live sample proves liveness just
// as well, so either one counts as up.
function isUp(m){return now()-m.last_seen<DOWN_AFTER||!!live[m.id]}
function seenLabel(m){
  var s=live[m.id], hb=fmtAge(now()-m.last_seen);
  if(s)return{text:"live · "+fmtSecs(now()-s.at_unix),cls:" live",
    title:"live sample streaming; last heartbeat "+hb};
  return isUp(m)?{text:hb,cls:"",title:"last heartbeat (no live metrics — is --metrics-interval 0?)"}
                :{text:"down "+hb,cls:" down",title:"no heartbeat since "+new Date(m.last_seen*1000).toLocaleString()};
}
function barColor(p){return p<70?"var(--accent)":p<90?"var(--warn)":"var(--danger)"}
// count of routable addresses beyond the primary, so a multi-homed box
// advertises that fact on the card without listing every veth
function otherIPs(inv){
  var n=0;
  (inv.interfaces||[]).forEach(function(i){
    if(i.virtual)return;
    n+=(i.ipv4||[]).length+(i.ipv6||[]).length;
  });
  return Math.max(0,n-1);
}
function clock(){var d=new Date();return d.toTimeString().slice(0,8)}

/* ---- api ---- */
function api(path){
  return fetch(path,{headers:{Authorization:"Bearer "+tok}}).then(function(r){
    if(r.status===401)throw new Error("unauthorized");
    if(!r.ok)throw new Error("HTTP "+r.status);
    return r.json();
  });
}

/* ---- fleet health: shown in BOTH views ---- */
function renderHealth(){
  var up=0,down=[],i;
  for(i=0;i<machines.length;i++){ if(isUp(machines[i]))up++; else down.push(machines[i].name||machines[i].id) }
  var el=$("health"), txt=$("health-text");
  el.classList.remove("hidden");
  if(down.length){
    el.classList.add("bad");
    txt.textContent=down.length+" offline: "+down.slice(0,3).join(", ")+(down.length>3?" +"+(down.length-3):"");
    document.title="("+down.length+" down) fleetcore";
  } else {
    el.classList.remove("bad");
    txt.textContent=up+" machine"+(up===1?"":"s")+" online";
    document.title="fleetcore";
  }
}

/* an offline/online transition raises a banner that persists until dismissed,
   so a machine dropping while you are inside a detail view is not missed */
function detectTransitions(){
  machines.forEach(function(m){
    var up=isUp(m), was=prevUp[m.id];
    if(was!==undefined && was!==up) raise(m,up);
    prevUp[m.id]=up;
  });
}
function raise(m,up){
  var d=document.createElement("div");
  d.className="event"+(up?" ok":"");
  d.innerHTML="<b>"+esc(m.name||m.id)+"</b> went "+(up?"back online":"OFFLINE")+
    "<span class='t'>"+clock()+"</span>";
  var b=document.createElement("button"); b.className="ghost"; b.textContent="dismiss";
  b.onclick=function(){d.remove()};
  d.appendChild(b);
  $("events").prepend(d);
}

/* ---- overview ---- */
// Free-text filter across the fields an operator actually searches by. At
// three machines a filter is pointless; at three hundred the grid is
// unusable without one.
var filterText="";
function matchesFilter(m){
  if(!filterText)return true;
  var inv=m.inventory||{}, labels=m.labels||{};
  var hay=[m.name,m.id,inv.primary_ip,inv.hostname,inv.os,inv.os_version,inv.arch,inv.kubernetes]
    .concat(Object.keys(labels).map(function(k){return k+"="+labels[k]}))
    .concat((inv.interfaces||[]).map(function(i){return (i.ipv4||[]).join(" ")}))
    .join(" ").toLowerCase();
  // every whitespace-separated term must match, so "gpu hk" narrows
  return filterText.split(/\s+/).every(function(t){return hay.indexOf(t)>=0});
}
function renderOverview(){
  var s=$("summary"), g=$("mgrid"), up=0, down=0, cpus=[], memU=0, memT=0, i;
  for(i=0;i<machines.length;i++){ isUp(machines[i])?up++:down++ }
  Object.keys(live).forEach(function(k){
    cpus.push(live[k].cpu_percent); memU+=live[k].mem_used; memT+=live[k].mem_total;
  });
  var avg=cpus.length?cpus.reduce(function(a,b){return a+b},0)/cpus.length:0;
  s.innerHTML=
    stat("machines",machines.length)+stat("online",up)+
    stat("offline",down,down>0)+stat("avg cpu",avg.toFixed(1)+"%")+
    stat("memory",memT?fmtB(memU)+" / "+fmtB(memT):"--");

  if(!machines.length){ g.innerHTML="<div class='nodata'>no machines enrolled yet</div>"; cards={}; return }

  // Update in place. Rebuilding the grid every tick replaced each bar with
  // a brand-new element already at its final width, so the CSS transition
  // never ran and the whole page looked frozen between repaints.
  var seen={};
  var shown=machines.filter(matchesFilter);
  $("filter-count").textContent=shown.length===machines.length?""
    :shown.length+" of "+machines.length+" shown";
  shown.slice().sort(function(a,b){return (a.name||"").localeCompare(b.name||"")})
  .forEach(function(m){
    seen[m.id]=true;
    if(!cards[m.id]){ cards[m.id]=machineCard(m); g.appendChild(cards[m.id].el) }
    updateCard(cards[m.id],m);
  });
  Object.keys(cards).forEach(function(id){
    if(!seen[id]){ cards[id].el.remove(); delete cards[id] }
  });
}
function stat(k,v,bad){
  return "<div class='stat'><div class='k'>"+k+"</div><div class='v"+(bad?" bad":"")+"'>"+v+"</div></div>";
}
// machineCard builds the static skeleton once and returns handles to the
// parts that change, so updateCard can touch text and widths only.
function machineCard(m){
  var el=document.createElement("div");
  el.className="mcard";
  el.dataset.id=m.id;   // delegation reads this; closures do not survive re-render
  el.innerHTML=
    "<div class='mhead'><span class='dot'></span><span class='mname'></span>"+
    "<span class='mseen'></span>"+
    "<button class='ghost mrm' title='Remove this machine from the fleet'>Remove</button></div>"+
    "<div class='mip'></div><div class='msub'></div>"+
    "<div class='bars'>"+barRow("cpu")+barRow("mem")+barRow("disk")+"</div>"+
    "<div class='nodata hidden'></div>"+
    "<canvas class='spark'></canvas>"+
    "<div class='chips'></div>";
  el.onclick=function(){openDetail(m.id)};
  var q=function(sel){return el.querySelector(sel)};
  var rows=el.querySelectorAll(".brow");
  return {
    el:el, id:m.id,
    dot:q(".dot"), name:q(".mname"), seen:q(".mseen"), ip:q(".mip"), sub:q(".msub"),
    bars:el.querySelector(".bars"), none:q(".nodata"), spark:q(".spark"), chips:q(".chips"),
    rows:[
      {fill:rows[0].querySelector(".bfill"), val:rows[0].querySelector(".bval")},
      {fill:rows[1].querySelector(".bfill"), val:rows[1].querySelector(".bval")},
      {fill:rows[2].querySelector(".bfill"), val:rows[2].querySelector(".bval")}
    ]
  };
}
// Deleting a machine is also the revocation path: identity is checked against
// the store on every request, so the certificate stops working immediately.
// An agent that still holds an enrolment token will re-enrol as a NEW machine,
// which is worth saying out loud before the operator clicks through.
function removeMachine(m){
  var name=m.name||m.id;
  var up=isUp(m);
  if(!confirm("Remove "+name+" from the fleet?\n\n"+
      "This deletes its record and revokes its certificate immediately.\n"+
      (up?"This machine is still ONLINE - if its agent holds an enrolment token it will re-enrol as a new machine.\n":"")+
      "\nThis cannot be undone."))return;
  fetch("/admin/machines/"+encodeURIComponent(m.id),
    {method:"DELETE",headers:{Authorization:"Bearer "+tok}})
  .then(function(r){
    if(!r.ok&&r.status!==204)throw new Error("HTTP "+r.status);
    // Drop local traces so the card cannot briefly reappear from cache.
    delete hist[m.id];
    if(cards[m.id]){cards[m.id].el.remove();delete cards[m.id]}
    delete prevUp[m.id];
    if(selected===m.id)closeDetail();
    return refresh();
  }).catch(function(e){alert("remove failed: "+e.message)});
}
function barRow(label){
  return "<div class='brow'><span class='blabel'>"+label+"</span>"+
    "<span class='btrack'><span class='bfill'></span></span>"+
    "<span class='bval'></span></div>";
}
function setBar(row,pct,text){
  pct=Math.max(0,Math.min(100,pct||0));
  row.fill.style.width=pct.toFixed(1)+"%";
  row.fill.style.background=barColor(pct);
  row.val.textContent=text;
}
function updateCard(c,m){
  var up=isUp(m), s=live[m.id], inv=m.inventory||{}, seen=seenLabel(m);
  c.el.className="mcard"+(up?"":" down");
  c.dot.className="dot "+(up?"up":"down");
  c.name.textContent=m.name||m.id;
  c.seen.className="mseen"+seen.cls;
  c.seen.title=seen.title;
  c.seen.textContent=seen.text;

  var extra=otherIPs(inv);
  c.ip.innerHTML=inv.primary_ip
    ?esc(inv.primary_ip)+(extra?"<span class='chip' style='margin-left:6px'>+"+extra+" more</span>":"")
    :"<span class='noip'>no address</span>";
  c.sub.textContent=(inv.os||"?")+" "+(inv.os_version||"")+" · "+(inv.arch||"?")+
    (inv.cpu_cores?" · "+inv.cpu_cores+" cores":"")+
    (inv.uptime_sec?" · up "+fmtUp(inv.uptime_sec):"")+
    (inv.kubernetes?" · k8s "+inv.kubernetes:"");

  if(s){
    c.bars.classList.remove("hidden"); c.none.classList.add("hidden");
    c.spark.classList.remove("hidden");
    setBar(c.rows[0],s.cpu_percent,s.cpu_percent.toFixed(1)+"%");
    setBar(c.rows[1],s.mem_total?100*s.mem_used/s.mem_total:0,fmtB(s.mem_used)+"/"+fmtB(s.mem_total));
    var dskP=s.disk_total?100*s.disk_used/s.disk_total:0;
    setBar(c.rows[2],dskP,Math.round(dskP)+"% of "+fmtB(s.disk_total));
    drawSpark(c.spark,histFor(m.id).cpu);
  } else {
    c.bars.classList.add("hidden"); c.spark.classList.add("hidden");
    c.none.classList.remove("hidden");
    c.none.textContent=up?"no live sample (metrics disabled?)":"not reporting";
  }

  var labels=m.labels||{}, keys=Object.keys(labels).sort();
  var sig=keys.map(function(k){return k+"="+labels[k]}).join(",");
  if(c.chips.dataset.sig!==sig){
    c.chips.dataset.sig=sig;
    c.chips.innerHTML=keys.map(function(k){
      return "<span class='chip'>"+esc(k)+"="+esc(labels[k])+"</span>"}).join("");
  }
}
// A CPU sparkline per card, drawn from the same rolling history the detail
// charts use — the fleet view shows movement rather than a static snapshot.
function drawSpark(cv,data){
  var dpr=window.devicePixelRatio||1;
  var w=cv.clientWidth, h=cv.clientHeight;
  if(!w||!h)return;
  cv.width=w*dpr; cv.height=h*dpr;
  var g=cv.getContext("2d"); g.scale(dpr,dpr);
  g.clearRect(0,0,w,h);
  if(data.length<2)return;
  // Span whatever history exists rather than a fixed 120-point window: the
  // overview is fed by the 2s poll, so a fixed span left the line crammed
  // into the right edge for the first minute.
  var step=w/(data.length-1);
  g.beginPath();
  data.forEach(function(v,i){
    var x=i*step, y=h-Math.max(0,Math.min(100,v))/100*(h-3)-2;
    i===0?g.moveTo(x,y):g.lineTo(x,y);
  });
  g.strokeStyle=palette().accent; g.lineWidth=1.5; g.stroke();
  g.lineTo(w,h); g.lineTo(0,h); g.closePath();
  g.fillStyle=fade(palette().accent); g.fill();
}

/* ---- commands ----
   A command is a module with a generated name, so its lifecycle is
   readable straight off the machine record: present in desired state and
   reported in status means it ran; present but unreported means the agent
   has not pulled it yet. */
var commands={};   // name -> {script, created}
var mgroups=[];    // groups the selected machine belongs to

function apiPost(path,body){
  return fetch(path,{method:"POST",headers:{Authorization:"Bearer "+tok,
    "Content-Type":"application/json"},body:JSON.stringify(body)}).then(function(r){
    if(!r.ok)return r.text().then(function(t){throw new Error(t.trim()||("HTTP "+r.status))});
    return r.json();
  });
}
// Command scripts are immutable once created, so they are fetched once and
// cached. Re-fetching every command on every poll made the detail view
// slow to open and hammered the store, since the server reads each
// command's payload individually.
var cmdCap=0;   // retention cap reported by the server
function loadCommands(force){
  var m=findMachine(selected);
  if(!force&&m){
    var missing=((m.desired&&m.desired.modules)||[]).some(function(sp){
      return sp.name.indexOf("cmd-")===0&&!commands[sp.name];
    });
    if(!missing)return Promise.resolve();   // nothing new; skip the round-trip
  }
  return fetch("/admin/commands",{headers:{Authorization:"Bearer "+tok}}).then(function(r){
    if(!r.ok)throw new Error("HTTP "+r.status);
    var cap=parseInt(r.headers.get("X-Fleet-Command-History")||"",10);
    if(cap>0)cmdCap=cap;
    return r.json();
  }).then(function(list){
    (list||[]).forEach(function(c){commands[c.name]=c});
  }).catch(function(){});
}
// Strip the shebang/set line the server prepends so the UI echoes what
// the operator actually typed.
function displayScript(s){
  if(!s)return "";
  return s.replace(/^#!\/bin\/sh\n(set -eu\n)?(cd \/\n)?/,"").replace(/\n+$/,"");
}
function cmdState(m,name){
  var st=(m.status||[]).filter(function(x){return x.name===name})[0];
  if(st)return st;
  return {state:"queued",detail:"",at_unix:0};
}
function cmdRow(sp,st,c){
  var when=c.created?new Date(c.created*1000).toLocaleTimeString():"";
  // Re-run creates a NEW command with the same script: modules execute once
  // per (name, version, config), so re-issuing the same name would be a
  // no-op the agent skips as already converged.
  return "<div class='cmdrow'><div class='cmdhead'>"+
    "<span class='badge "+esc(st.state)+"'>"+esc(st.state)+"</span>"+
    "<span class='mono'>"+esc(sp.name)+"</span>"+
    ((sp.config&&sp.config.label)?"<span class='chip'>"+esc(sp.config.label)+"</span>":"")+
    "<span class='cmdtime'>"+esc(when)+"</span>"+
    "<button class='ghost cmdbtn' data-act='rerun' data-name='"+esc(sp.name)+"'>Re-run</button>"+
    "<button class='ghost cmdbtn' data-act='cancel' data-name='"+esc(sp.name)+"'>"+
      (st.state==="queued"?"Cancel":"Remove")+"</button></div>"+
    "<div class='cmdscript'>"+esc(displayScript(c.script))+"</div>"+
    (st.detail?"<div class='cmdout'>"+esc(st.detail)+"</div>"
              :(st.state==="queued"?"<div class='cmdout'>waiting for the agent to pull…</div>":""))+
    "</div>";
}
// Pending and history are separate panels: a queued command is the thing an
// operator is actively waiting on, and burying it in a list sorted by time
// makes it hard to see whether anything is outstanding.
function renderCommands(){
  var m=findMachine(selected); if(!m)return;
  var specs=((m.desired&&m.desired.modules)||[]).filter(function(s){return s.name.indexOf("cmd-")===0});
  specs.sort(function(a,b){return (commands[b.name]||{}).created-(commands[a.name]||{}).created});

  var pending=[], history=[];
  specs.forEach(function(sp){
    var st=cmdState(m,sp.name);
    (st.state==="queued"?pending:history).push(cmdRow(sp,st,commands[sp.name]||{}));
  });

  $("queue-section").classList.toggle("hidden",pending.length===0);
  $("v-queued").textContent=pending.length?pending.length+" pending":"";
  $("queuelist").innerHTML=pending.join("");

  $("v-cmds").textContent=history.length
    ?history.length+" executed"+(cmdCap?" · keeping newest "+cmdCap:"")
    :"";
  $("cmdlist").innerHTML=history.length?history.join("")
    :"<div class='nodata'>no commands have run on this machine yet</div>";
}
// Delegated so the buttons survive the panel being re-rendered every poll.
function onCmdClick(e){
  var b=e.target.closest?e.target.closest(".cmdbtn"):null;
  if(!b||!selected)return;
  e.stopPropagation();
  var name=b.getAttribute("data-name"), act=b.getAttribute("data-act");
  b.disabled=true;
  if(act==="rerun"){
    var c=commands[name];
    if(!c){b.disabled=false;return}
    apiPost("/admin/commands",{script:displayScript(c.script),
      target_kind:"machine",target_id:selected})
      .then(function(){return loadCommands(true)}).then(refresh)
      .catch(function(err){alert("re-run failed: "+err.message);b.disabled=false});
  } else {
    fetch("/admin/commands/"+encodeURIComponent(name)+
          "?target_kind=machine&target_id="+encodeURIComponent(selected),
      {method:"DELETE",headers:{Authorization:"Bearer "+tok}})
    .then(function(r){
      if(!r.ok&&r.status!==204)throw new Error("HTTP "+r.status);
      return refresh();
    }).catch(function(err){alert("cancel failed: "+err.message);b.disabled=false});
  }
}

function renderGroups(){
  var el=$("d-groups");
  el.innerHTML=mgroups.length?mgroups.map(function(g){
    // Say WHY the machine is in the group. "via all" was ambiguous: the
    // built-in group is itself named "all", so it rendered "all via all".
    var why="";
    if(g.via==="match_all")     why="every machine in the tenant";
    else if(g.via==="selector") why="labels "+Object.keys(g.selector||{}).sort()
                                      .map(function(k){return k+"="+g.selector[k]}).join(" ");
    else                        why="added directly";
    return "<span class='gchip' title='"+esc(why)+"'>"+esc(g.name)+
      " <span class='via'>"+esc(why)+"</span></span>";
  }).join(""):"<span class='chip'>no groups</span>";
}
// "role=gpu region=hk" -> {role:"gpu",region:"hk"}; null when empty.
function parseSelector(raw){
  raw=(raw||"").trim();
  if(!raw)return null;
  var out={},ok=false;
  raw.split(/[\s,]+/).forEach(function(pair){
    var i=pair.indexOf("=");
    if(i>0){out[pair.slice(0,i)]=pair.slice(i+1);ok=true}
  });
  return ok?out:null;
}
function runCommand(kind,id,scriptEl,msgEl,selector){
  var script=scriptEl.value.trim();
  if(!script){msgEl.className="cmsg err";msgEl.textContent="type a command first";return}
  msgEl.className="cmsg"; msgEl.textContent="queuing…";
  var body={script:script,target_kind:kind,target_id:id};
  if(selector)body.selector=selector;
  apiPost("/admin/commands",body).then(function(c){
    msgEl.className="cmsg ok";
    msgEl.textContent="queued as "+c.name+
      (c.matched>1?" on "+c.matched+" machines":"")+" — agents pull it on their next reconcile";
    scriptEl.value="";
    return loadCommands(true).then(refresh);
  }).catch(function(e){ msgEl.className="cmsg err"; msgEl.textContent=e.message });
}
// Groups change rarely, so this runs once after connecting rather than on
// every 2s poll — refetching them each tick was pure waste and made the
// page feel sluggish.
var groupsLoaded=false;
function fillGroupSelect(){
  if(groupsLoaded)return Promise.resolve();
  var sel=$("g-target");
  var tenants={}; machines.forEach(function(m){tenants[m.tenant_id]=true});
  var ids=Object.keys(tenants);
  if(!ids.length)return Promise.resolve();
  groupsLoaded=true;
  return Promise.all(ids.map(function(t){return api("/admin/groups?tenant="+t).catch(function(){return []})}))
  .then(function(lists){
    var groups=[].concat.apply([],lists);
    var cur=sel.value;
    sel.innerHTML=groups.map(function(g){
      return "<option value='"+esc(g.id)+"'>"+esc(g.name)+(g.match_all?" (all machines)":"")+"</option>";
    }).join("");
    if(cur)sel.value=cur;
  }).catch(function(){groupsLoaded=false});
}

/* ---- detail ---- */
function openDetail(id){
  selected=id;
  $("overview").classList.add("hidden");
  $("detail").classList.remove("hidden");
  renderDetailStatic();
  // The fleet poll has been filling this machine's history all along, so
  // the charts open already populated and simply continue. Clearing them
  // here is what made opening a machine feel like a page reload.
  var h=histFor(id);
  renderInterfaces(h.last);
  if(h.last)redraw(h.last);   // paint from existing history, do not re-ingest
  mgroups=[]; renderGroups();
  api("/admin/machines/"+id+"/groups").then(function(g){
    if(selected===id){ mgroups=g||[]; renderGroups() }
  }).catch(function(){});
  loadCommands().then(function(){ if(selected===id)renderCommands() });
  startStream(id);
}
function closeDetail(){
  selected=null; streaming=false;
  if(ctrl){ctrl.abort();ctrl=null}
  $("detail").classList.add("hidden");
  $("overview").classList.remove("hidden");
  renderOverview(); // repaint immediately rather than waiting for the next tick
}
function findMachine(id){
  for(var i=0;i<machines.length;i++) if(machines[i].id===id) return machines[i];
  return null;
}
function renderDetailStatic(){
  var m=findMachine(selected); if(!m)return;
  var inv=m.inventory||{}, up=isUp(m);
  $("d-name").textContent=m.name||m.id;
  $("d-id").textContent=m.id;
  // Heartbeat and live-sample ages are shown separately on purpose: they
  // are different channels at different intervals, and conflating them is
  // what makes a streaming machine look stale.
  var s=live[m.id];
  $("d-state").innerHTML="<span class='dot "+(up?"up":"down")+"' style='display:inline-block'></span> "+
    (up?"online":"OFFLINE")+
    " &middot; heartbeat "+fmtAge(now()-m.last_seen)+
    " &middot; "+(s?"<span style='color:var(--accent)'>live sample "+fmtSecs(now()-s.at_unix)+" ago</span>"
                   :"<span style='color:var(--muted)'>no live sample</span>");
  var kv=[["hostname",inv.hostname],["primary ip",inv.primary_ip||"--"],
    ["interfaces",(inv.interfaces||[]).length||"--"],
    ["os",(inv.os||"")+" "+(inv.os_version||"")],
    ["kernel",inv.kernel],["arch",inv.arch],["cpu cores",inv.cpu_cores||"--"],
    ["uptime",inv.uptime_sec?fmtUp(inv.uptime_sec):"--"],
    ["packages",inv.packages],["processes",inv.processes],
    ["agent",inv.agent_version],["kubernetes",inv.kubernetes||"--"],
    ["desired rev",m.desired?m.desired.revision:"--"]];
  if(inv.updates) kv.push(["updates",inv.updates.total+" pending / "+inv.updates.security+
    " security"+(inv.updates.reboot_required?" · REBOOT REQUIRED":"")]);
  if(inv.services&&inv.services.length) kv.push(["services",
    inv.services.map(function(s){return s.name+" ("+s.category+")"}).join(", ")]);
  if(inv.gpus&&inv.gpus.length) kv.push(["gpus",inv.gpus.join(", ")]);
  $("d-inv").innerHTML=kv.map(function(p){
    return "<b>"+esc(p[0])+"</b><span>"+esc(p[1]==null||p[1]===""?"--":p[1])+"</span>"}).join("");

  // Ad-hoc commands have their own panel above; listing them here too made
  // the same output appear twice on the page.
  var specs=((m.desired&&m.desired.modules)||[]).filter(function(sp){return sp.name.indexOf("cmd-")!==0});
  var st=(m.status||[]).filter(function(x){return x.name.indexOf("cmd-")!==0});
  if(!specs.length&&!st.length){ $("d-mods").innerHTML="<div class='nodata'>no modules assigned</div>" }
  else{
    var byName={}; st.forEach(function(x){byName[x.name]=x});
    $("d-mods").innerHTML="<table><thead><tr><th>module</th><th>version</th><th>state</th></tr></thead><tbody>"+
      specs.map(function(sp){
        var s2=byName[sp.name];
        var state=s2?s2.state:"pending";
        var col=state==="applied"?"var(--accent)":state==="failed"?"var(--danger)":"var(--muted)";
        return "<tr><td class='name'>"+esc(sp.name)+"</td><td>"+esc(sp.version)+
          "</td><td style='color:"+col+"'>"+esc(state)+(s2&&s2.detail?" &mdash; "+esc(s2.detail):"")+"</td></tr>";
      }).join("")+"</tbody></table>";
  }
}
// renderInterfaces joins the static address list (inventory, refreshed on
// heartbeat) with live throughput (sample), so an interface with no
// traffic still appears with its IP rather than vanishing.
function renderInterfaces(s){
  var m=findMachine(selected); if(!m)return;
  var ifaces=(m.inventory||{}).interfaces||[];
  var rates={}; ((s&&s.interfaces)||[]).forEach(function(n){rates[n.name]=n});
  var real=ifaces.filter(function(i){return !i.virtual});
  $("v-ifaces").textContent=ifaces.length+" total · "+real.length+" physical";
  $("ifacebody").innerHTML=ifaces.map(function(i){
    var r=rates[i.name]||{rx_bps:0,tx_bps:0};
    var addrs=(i.ipv4||[]).concat(i.ipv6||[]);
    var tag=i.primary?"<span class='chip' style='color:var(--accent)'>primary</span> "
           :i.virtual?"<span class='chip'>virtual</span> ":"";
    return "<tr><td class='name'>"+tag+esc(i.name)+(i.up?"":" <span style='color:var(--danger)'>down</span>")+"</td>"+
      "<td class='name mono'>"+(addrs.length?esc(addrs.join(" ")):"<span style='color:var(--muted)'>—</span>")+"</td>"+
      "<td class='name mono'>"+esc(i.mac||"—")+"</td>"+
      "<td style='text-align:right;color:var(--muted)'>"+(i.speed_mbps?i.speed_mbps+" Mb/s":"—")+"</td>"+
      "<td style='text-align:right'>"+fmtB(r.rx_bps)+"/s</td>"+
      "<td style='text-align:right'>"+fmtB(r.tx_bps)+"/s</td></tr>";
  }).join("");
}

function startStream(id){
  if(ctrl)ctrl.abort();
  ctrl=new AbortController();
  streaming=false;
  fetch("/admin/machines/"+id+"/live",{headers:{Authorization:"Bearer "+tok},signal:ctrl.signal})
  .then(function(r){
    if(!r.ok)throw new Error("HTTP "+r.status);
    streaming=true;
    var rd=r.body.getReader(),dec=new TextDecoder(),buf="";
    function pump(){return rd.read().then(function(x){
      if(x.done){streaming=false;return}
      buf+=dec.decode(x.value,{stream:true});
      var idx;
      while((idx=buf.indexOf("\n\n"))>=0){
        var chunk=buf.slice(0,idx); buf=buf.slice(idx+2);
        if(chunk.indexOf("data: ")===0){try{render(JSON.parse(chunk.slice(6)),true)}catch(e){}}
      }
      return pump();
    })}
    return pump();
  }).catch(function(e){
    streaming=false;
    if(e.name==="AbortError")return;
    // The stream dropped; the poll keeps the charts alive meanwhile, so a
    // reconnect is invisible rather than a gap.
    if(selected===id) setTimeout(function(){if(selected===id)startStream(id)},3000);
  });
}
// Area fills need a translucent variant of the line colour. Tokens may be
// hex or a colour function depending on the theme, so fall back to a
// canvas-safe overlay rather than assuming an 8-digit hex is valid.
function fade(c){
  if(/^#[0-9a-fA-F]{6}$/.test(c))return c+"22";
  return c;
}
function draw(id,rows,max,colors){
  var cv=$(id),dpr=window.devicePixelRatio||1;
  cv.width=cv.clientWidth*dpr; cv.height=cv.clientHeight*dpr;
  var g=cv.getContext("2d"); g.scale(dpr,dpr);
  var w=cv.clientWidth,h=cv.clientHeight;
  g.clearRect(0,0,w,h);
  g.strokeStyle=palette().grid; g.lineWidth=1;
  for(var i=1;i<4;i++){g.beginPath();g.moveTo(0,h*i/4);g.lineTo(w,h*i/4);g.stroke()}
  rows.forEach(function(data,ri){
    if(data.length<2)return;
    var step=w/(WINDOW-1), off=WINDOW-data.length;
    g.beginPath();
    data.forEach(function(v,i){
      var x=(off+i)*step, y=h-(max>0?v/max:0)*(h-6)-3;
      i===0?g.moveTo(x,y):g.lineTo(x,y);
    });
    g.strokeStyle=colors[ri]; g.lineWidth=1.5; g.stroke();
    g.lineTo((off+data.length-1)*step,h); g.lineTo(off*step,h); g.closePath();
    g.fillStyle=fade(colors[ri]); g.fill();
  });
}
function render(s,fromStream){
  if(!s||!selected)return;
  ingest(selected,s,fromStream);
  redraw(s);
}
// redraw paints the detail view from the machine's existing history. Kept
// separate from ingest so opening a machine can repaint instantly without
// re-appending samples that are already recorded.
function redraw(s){
  if(!s||!selected)return;
  var series=histFor(selected);
  $("v-cpu").textContent=s.cpu_percent.toFixed(1)+"% · load "+s.load1;
  $("v-mem").textContent=fmtB(s.mem_used)+" / "+fmtB(s.mem_total);
  $("v-net").textContent="↓"+fmtB(s.net_rx_bps)+"/s ↑"+fmtB(s.net_tx_bps)+"/s";
  $("v-disk").textContent=fmtB(s.disk_used)+" / "+fmtB(s.disk_total);
  var P=palette();
  draw("c-cpu",[series.cpu],100,[P.accent]);
  draw("c-mem",[series.mem],s.mem_total,[P.blue]);
  var nmax=Math.max.apply(null,series.rx.concat(series.tx,[1024]));
  draw("c-net",[series.rx,series.tx],nmax,[P.accent,P.warn]);
  draw("c-disk",[series.disk],s.disk_total,[P.pink]);
  var pb=$("procbody"), procs=s.top_processes||[];
  $("procnone").classList.toggle("hidden",procs.length>0);
  // Totals come from the whole process table, not this list — the list is
  // a union of top-by-CPU, top-by-RSS and recognised services.
  $("v-procs").textContent=(s.proc_count||procs.length)+" running · "+
    fmtB(s.proc_total_rss||0)+" resident total";
  pb.innerHTML=procs.map(function(p){
    return "<tr><td style='color:var(--muted)'>"+p.pid+"</td><td class='name'>"+esc(p.comm)+
      "</td><td class='name' style='color:var(--accent)'>"+esc(p.service||"")+"</td>"+
      "<td style='text-align:right'>"+p.cpu_percent.toFixed(1)+"%</td>"+
      "<td style='text-align:right'>"+fmtB(p.mem_bytes)+"</td></tr>";
  }).join("");

  var pods=s.pods||[], ps=$("pod-section");
  if(pods.length){
    ps.classList.remove("hidden");
    var podCPU=0,podMem=0;
    pods.forEach(function(p){podCPU+=p.cpu_percent;podMem+=p.mem_bytes});
    $("v-pods").textContent=pods.length+" pods · "+podCPU.toFixed(1)+"% cpu · "+fmtB(podMem)+
      " · via "+(pods[0].source||"?");
    $("podbody").innerHTML=pods.map(function(p){
      var cs=(p.containers||[]).map(function(c){return c.name}).join(", ");
      // cgroup-sourced pods have a UID but no name until /var/log/pods is readable
      var nm=p.name||("<span style='color:var(--muted)'>"+esc((p.uid||"").slice(0,18))+"…</span>");
      return "<tr><td class='name' style='color:var(--muted)'>"+esc(p.namespace||"—")+"</td>"+
        "<td class='name'>"+(p.name?esc(p.name):nm)+"</td>"+
        "<td class='name' style='color:var(--muted)'>"+esc(cs)+"</td>"+
        "<td style='text-align:right'>"+p.cpu_percent.toFixed(1)+"%</td>"+
        "<td style='text-align:right'>"+fmtB(p.mem_bytes)+"</td></tr>";
    }).join("");
  } else { ps.classList.add("hidden") }

  renderInterfaces(s);
  var gs=$("gpu-section"), gb=$("gpubody");
  if(s.gpus&&s.gpus.length){
    gs.classList.remove("hidden"); gb.innerHTML="";
    s.gpus.forEach(function(g){
      var pct=g.mem_total?Math.round(100*g.mem_used/g.mem_total):0;
      var d=document.createElement("div"); d.style.cssText="padding:6px 0;border-top:1px solid var(--line)";
      d.innerHTML="<b>#"+g.index+" "+esc(g.name)+"</b> &mdash; util "+g.util_percent+"% · "+
        fmtB(g.mem_used)+"/"+fmtB(g.mem_total)+" ("+pct+"%) · "+g.temp_c+"°C · "+g.power_w+"W";
      gb.appendChild(d);
    });
  } else { gs.classList.add("hidden") }
}

/* ---- polling: runs in both views so health stays current ---- */
function refresh(){
  return Promise.all([api("/admin/machines"),api("/admin/live")]).then(function(r){
    machines=r[0]||[]; live=r[1]||{};
    $("status").textContent=""; $("status").classList.remove("err");
    // Record every machine's sample, not just the one on screen — this is
    // what gives a machine chart history before it is ever opened.
    Object.keys(live).forEach(function(id){ ingest(id,live[id]) });
    detectTransitions();
    renderHealth();
    if(selected){
      renderDetailStatic();
      renderCommands();   // picks up state changes as agents report back
      // The SSE stream drives the charts while a machine is open; if it is
      // still connecting, keep them moving from the poll.
      if(!streaming)render(histFor(selected).last);
    } else renderOverview();
  }).catch(function(e){
    $("status").textContent=e.message==="unauthorized"?"auth failed — check the token":e.message;
    $("status").classList.add("err");
    if(e.message==="unauthorized")disconnect();
  });
}
function connect(){
  var t=$("token").value.trim();
  if(!t){$("status").textContent="paste the admin token";$("status").classList.add("err");return}
  tok=t;
  try{localStorage.setItem("fleet_token",t)}catch(e){}
  $("token").classList.add("hidden"); $("connect").classList.add("hidden");
  $("forget").classList.remove("hidden");
  $("overview").classList.remove("hidden");
  refresh().then(fillGroupSelect);
  if(poller)clearInterval(poller);
  poller=setInterval(refresh,2000);
}
function disconnect(){
  if(poller){clearInterval(poller);poller=null}
  if(ctrl){ctrl.abort();ctrl=null}
  tok=""; selected=null; machines=[]; live={}; prevUp={}; groupsLoaded=false; commands={};
  try{localStorage.removeItem("fleet_token")}catch(e){}
  $("token").classList.remove("hidden"); $("connect").classList.remove("hidden");
  $("forget").classList.add("hidden");
  $("overview").classList.add("hidden"); $("detail").classList.add("hidden");
  $("health").classList.add("hidden");
  document.title="fleetcore";
}

$("g-run").onclick=function(){
  var sel=parseSelector($("g-selector").value);
  if(sel){
    // Selector targeting is scoped by tenant, not by group.
    var tenant=machines.length?machines[0].tenant_id:"";
    if(!tenant){$("g-msg").className="cmsg err";$("g-msg").textContent="no machines";return}
    runCommand("selector",tenant,$("g-script"),$("g-msg"),sel);
    return;
  }
  var id=$("g-target").value;
  if(!id){$("g-msg").className="cmsg err";$("g-msg").textContent="no group selected";return}
  runCommand("group",id,$("g-script"),$("g-msg"));
};
$("m-run").onclick=function(){
  if(selected)runCommand("machine",selected,$("m-script"),$("m-msg"));
};
// Ctrl/Cmd+Enter submits, so the textarea can still take multi-line scripts.
function submitOnCtrlEnter(el,btn){
  el.addEventListener("keydown",function(e){
    if((e.metaKey||e.ctrlKey)&&e.key==="Enter"){e.preventDefault();btn.click()}
  });
}
submitOnCtrlEnter($("g-script"),$("g-run"));
submitOnCtrlEnter($("m-script"),$("m-run"));

// system -> light -> dark -> system
$("theme").onclick=function(){
  setTheme(themePref==="system"?"light":themePref==="light"?"dark":"system");
};
$("cmdlist").addEventListener("click",onCmdClick);
$("queuelist").addEventListener("click",onCmdClick);
$("mgrid").addEventListener("click",function(e){
  var b=e.target.closest?e.target.closest(".mrm"):null;
  if(!b)return;
  e.stopPropagation(); e.preventDefault();
  var card=b.closest(".mcard");
  var id=card&&card.dataset.id;
  var m=id&&findMachine(id);
  if(m)removeMachine(m);
});
$("filter").addEventListener("input",function(){
  filterText=this.value.trim().toLowerCase();
  renderOverview();
});
$("connect").onclick=connect;
$("forget").onclick=function(){disconnect();$("status").textContent=""};
$("back").onclick=closeDetail;
$("d-remove").onclick=function(){
  var m=findMachine(selected);
  if(m)removeMachine(m);
};
$("health").onclick=function(){if(selected)closeDetail()};
$("token").addEventListener("keydown",function(e){if(e.key==="Enter")connect()});

// Persist the rolling buffer so a page refresh resumes the charts rather
// than restarting them from an empty window.
window.addEventListener("pagehide",saveHist);
document.addEventListener("visibilitychange",function(){if(document.hidden)saveHist()});
setInterval(saveHist,5000);

(function boot(){
  applyTheme();
  loadHist();
  var saved=""; try{saved=localStorage.getItem("fleet_token")||""}catch(e){}
  if(saved){$("token").value=saved;connect()}
})();
</script></body></html>`
