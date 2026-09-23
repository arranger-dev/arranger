// Arranger UI: a free-form canvas of agents, the inspector (goal / logs / diff / settings),
// merging, stats, and live status over SSE.
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const PID = document.body.dataset.project;
const main = $("main"), canvas = $("#canvas"), linksSvg = $("#links");
const SVG = "http://www.w3.org/2000/svg";
const MAX_LOG_ROWS = 1000;
const BUSY = ["running", "verifying", "planning", "waiting", "reviewing"];
const NEEDS = ["failed", "blocked"];
const GAP_X = 240, GAP_Y = 150, PAD = 48, SNAP = 8;
let dirty = false, selected = null, tab = "goal";

function say(text, err = false) {
  const m = $("#msg");
  m.textContent = m.title = text;
  m.className = err ? "err" : "";
}

// status shows a result right where the user acted, not only in the header.
function status(elm, text, bad = false) {
  elm.textContent = text;
  elm.className = "status" + (text ? (bad ? " bad" : " ok") : "");
}

async function api(method, url, body) {
  const r = await fetch(url, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body && JSON.stringify(body) });
  if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
  return r.headers.get("Content-Type")?.includes("json") ? r.json() : null;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

const time = ts => new Date(ts).toLocaleTimeString([], { hour12: false });
const fmtTok = n => n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(n);
function fmtDur(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor(s / 60) % 60}m`;
}

/* ---------- the agent model ---------- */

// agents: id -> {id, name, role, runtime, parent, color, x, y, el, defaults}
const agents = new Map((window.AGENTS || []).map(a => [a.id, { ...a }]));
const TYPES = new Map((window.TYPES || []).map(t => [t.name, t]));

const cardOf = id => agents.get(id)?.el;
const nameOf = id => agents.get(id)?.name ?? id;
const parentOf = id => { const p = agents.get(id)?.parent; return agents.has(p) ? p : undefined; };
const kidsOf = id => [...agents.values()].filter(a => a.parent === id);
// isUnder reports whether id sits somewhere below ancestor.
function isUnder(id, ancestor) {
  for (let p = parentOf(id), n = 0; p && n < agents.size; p = parentOf(p), n++) if (p === ancestor) return true;
  return false;
}

function makeCard(a) {
  const c = $("#card-tpl").content.firstElementChild.cloneNode(true);
  c.dataset.id = a.id;
  canvas.append(c);
  a.el = c;
  paintCard(a);
}

function paintCard(a) {
  const c = a.el;
  c.dataset.role = a.role;
  c.querySelector(".name").textContent = a.name;
  c.querySelector(".role").textContent = a.role;
  c.querySelector(".rt").textContent = a.runtime;
  const color = a.color || TYPES.get(a.role)?.color;
  color ? c.style.setProperty("--c", color) : c.style.removeProperty("--c");
  c.style.left = a.x + "px";
  c.style.top = a.y + "px";
}

// layout places agents as a tidy tree: leaves side by side, managers centered over their team.
// With all=false only agents that have no position yet move.
function layout(all) {
  let slot = 0;
  const place = (a, depth, seen) => {
    seen.add(a.id);
    const xs = kidsOf(a.id).filter(k => !seen.has(k.id)).map(k => place(k, depth + 1, seen));
    const x = xs.length ? (xs[0] + xs[xs.length - 1]) / 2 : slot++ * GAP_X;
    if (all || a.x == null || a.y == null) { a.x = PAD + x; a.y = PAD + depth * GAP_Y; }
    return x;
  };
  const seen = new Set();
  [...agents.values()].filter(a => !parentOf(a.id)).forEach(r => place(r, 0, seen));
}

// drawLinks draws a dotted curve from each manager down to each report; a working agent's
// link animates up toward its manager.
function drawLinks() {
  let w = 0, h = 0;
  const paths = [];
  for (const a of agents.values()) {
    w = Math.max(w, a.x + a.el.offsetWidth);
    h = Math.max(h, a.y + a.el.offsetHeight);
    const p = agents.get(parentOf(a.id));
    if (!p) continue;
    const x1 = p.x + p.el.offsetWidth / 2, y1 = p.y + p.el.offsetHeight, x2 = a.x + a.el.offsetWidth / 2, y2 = a.y;
    const bend = Math.max(40, Math.abs(y2 - y1) / 2);
    const path = document.createElementNS(SVG, "path");
    path.setAttribute("d", `M${x1},${y1} C${x1},${y1 + bend} ${x2},${y2 - bend} ${x2},${y2}`);
    path.dataset.status = a.el.dataset.status;
    if (BUSY.includes(a.el.dataset.status)) path.classList.add("flow");
    paths.push(path);
  }
  linksSvg.replaceChildren(...paths);
  // room to spread out: the canvas always reaches well past the last box and the viewport
  const cw = Math.max(w + 1200, main.clientWidth * 2), ch = Math.max(h + 900, main.clientHeight * 2);
  canvas.style.width = cw + "px";
  canvas.style.height = ch + "px";
  linksSvg.setAttribute("width", cw);
  linksSvg.setAttribute("height", ch);
}

function markDirty() { dirty = true; say("unsaved changes"); }

function addAgent(role, name, type, x, y, parent) {
  const a = {
    id: role + "-" + Date.now().toString(36) + Math.random().toString(36).slice(2, 5),
    name, role, runtime: type?.runtime ?? "claude", parent: parent ?? "", color: "",
    x: Math.max(0, Math.round(x / SNAP) * SNAP), y: Math.max(0, Math.round(y / SNAP) * SNAP),
    defaults: type ? { model: type.model, args: type.args, prompt: type.prompt } : null,
  };
  agents.set(a.id, a);
  makeCard(a);
  return a;
}

// slotUnder is a free spot just below parent, to the right of its other reports.
function slotUnder(parentId, except) {
  const p = agents.get(parentId), sibs = kidsOf(parentId).filter(k => k.id !== except);
  return { x: sibs.length ? Math.max(...sibs.map(k => k.x)) + GAP_X : p.x, y: p.y + GAP_Y };
}

/* ---------- canvas interaction ---------- */

// palette → canvas: HTML5 drag and drop creates a new agent where it lands
let dragged = null;
$("#palette").addEventListener("dragstart", e => {
  dragged = e.target.closest?.(".card[data-new]");
  if (dragged) e.dataTransfer.effectAllowed = "copy";
});
document.addEventListener("dragend", () => { dragged = null; $$(".drop").forEach(x => x.classList.remove("drop")); });
main.addEventListener("dragover", e => {
  if (!dragged) return;
  $$(".drop").forEach(x => x.classList.remove("drop"));
  (e.target.closest("#canvas .card") || main).classList.add("drop");
  e.preventDefault();
});
main.addEventListener("drop", e => {
  e.preventDefault();
  if (!dragged) return;
  const target = e.target.closest("#canvas .card"), r = canvas.getBoundingClientRect();
  let x = e.clientX - r.left - 105, y = e.clientY - r.top - 24, parent = "";
  if (target) ({ x, y } = slotUnder(parent = target.dataset.id));
  addAgent(dragged.dataset.role, dragged.querySelector(".name").textContent, TYPES.get(dragged.dataset.role), x, y, parent);
  markDirty();
  refreshCards();
});

// boxes on the canvas: drag to move; release over another box to report to it; click to open
canvas.addEventListener("pointerdown", e => {
  const c = e.target.closest(".card");
  if (!c || e.button !== 0 || e.target.classList.contains("x")) return;
  const a = agents.get(c.dataset.id), sx = e.clientX, sy = e.clientY, ox = a.x, oy = a.y;
  let moved = false, over = null;
  try { c.setPointerCapture(e.pointerId); } catch {} // keeps the drag when the pointer outruns the box
  c.onpointermove = m => {
    const dx = m.clientX - sx, dy = m.clientY - sy;
    if (!moved && Math.hypot(dx, dy) < 4) return;
    moved = true;
    c.classList.add("dragging");
    a.x = Math.max(0, Math.round((ox + dx) / SNAP) * SNAP);
    a.y = Math.max(0, Math.round((oy + dy) / SNAP) * SNAP);
    c.style.left = a.x + "px";
    c.style.top = a.y + "px";
    over?.classList.remove("drop");
    over = document.elementsFromPoint(m.clientX, m.clientY).find(x => x !== c && x.matches?.("#canvas .card"));
    if (over && (over.dataset.id === a.parent || isUnder(over.dataset.id, a.id))) over = null; // no-ops and cycles
    over?.classList.add("drop");
    drawLinks();
  };
  c.onpointerup = c.onpointercancel = () => {
    c.onpointermove = c.onpointerup = c.onpointercancel = null;
    c.classList.remove("dragging");
    if (!moved) { select(a.id); return; }
    if (over) {
      over.classList.remove("drop");
      a.parent = over.dataset.id;
      ({ x: a.x, y: a.y } = slotUnder(a.parent, a.id));
      paintCard(a);
      say(`${a.name} now reports to ${nameOf(a.parent)} · unsaved`);
      dirty = true;
    } else markDirty();
    drawLinks();
  };
});
// empty space: drag to pan, like draw.io
main.addEventListener("pointerdown", e => {
  if (e.button !== 0 || e.target.closest(".card, button")) return;
  const sx = e.clientX + main.scrollLeft, sy = e.clientY + main.scrollTop;
  try { main.setPointerCapture(e.pointerId); } catch {}
  main.classList.add("panning");
  main.onpointermove = m => { main.scrollLeft = sx - m.clientX; main.scrollTop = sy - m.clientY; };
  main.onpointerup = main.onpointercancel = () => {
    main.onpointermove = main.onpointerup = main.onpointercancel = null;
    main.classList.remove("panning");
  };
});
canvas.addEventListener("click", e => {
  if (!e.target.classList.contains("x")) return;
  const id = e.target.closest(".card").dataset.id, a = agents.get(id);
  kidsOf(id).forEach(k => k.parent = a.parent); // reports move up a level
  if (selected === id) closeInspector();
  a.el.remove();
  agents.delete(id);
  markDirty();
  drawLinks();
});
canvas.addEventListener("dblclick", e => {
  const c = e.target.closest(".card");
  const a = c && agents.get(c.dataset.id);
  const v = a && prompt("Agent name", a.name);
  if (v?.trim()) { a.name = v.trim(); paintCard(a); markDirty(); }
});
$("#tidy").onclick = () => { layout(true); agents.forEach(paintCard); drawLinks(); markDirty(); };

async function save() {
  const list = [...agents.values()].sort((a, b) => a.y - b.y || a.x - b.x).map(a => ({
    ...(a.defaults || {}), id: a.id, name: a.name, role: a.role, runtime: a.runtime,
    parent: parentOf(a.id) ?? "", color: a.color || "", x: a.x, y: a.y,
  }));
  await api("POST", `/api/projects/${PID}/arrangement`, list);
  agents.forEach(a => a.defaults = null); // applied once, on creation
  dirty = false;
  say(`saved ${list.length} agents`);
  refreshSummary();
}
$("#save").onclick = () => save().catch(e => say(e.message, true));
addEventListener("beforeunload", e => { if (dirty) e.preventDefault(); });

/* ---------- custom agent types ---------- */

function renderTypes() {
  $("#custom-types").replaceChildren(...[...TYPES.values()].map(t => {
    const c = el("div", "card");
    c.draggable = true;
    c.dataset.role = t.name;
    c.dataset.new = "";
    c.style.setProperty("--c", t.color);
    const edit = el("span", "edit", "✎");
    edit.title = "Edit this type";
    edit.onclick = () => openType(t);
    c.append(el("span", "name", t.name), edit, el("span", "meta", t.runtime + (t.model ? " · " + t.model : "")));
    return c;
  }));
}

const tdlg = $("#type-dialog"), tform = $("#type-form");
let editingType = null;
function openType(t) {
  editingType = t;
  $("h3", tform).textContent = t ? "Edit agent type" : "New agent type";
  for (const k of ["name", "color", "runtime", "model", "args", "prompt"]) tform.elements[k].value = t?.[k] ?? { color: "#3d6fe0", runtime: "claude" }[k] ?? "";
  $("#t-delete").hidden = !t;
  status($("#t-err"), "");
  tdlg.showModal();
}
$("#new-type").onclick = () => openType(null);
tform.onsubmit = async e => {
  const action = e.submitter?.value;
  if (action === "cancel") return;
  e.preventDefault();
  try {
    if (action === "delete") {
      await api("DELETE", `/api/types/${editingType.id}`);
      TYPES.delete(editingType.name);
    } else {
      const body = Object.fromEntries(["name", "color", "runtime", "model", "args", "prompt"].map(k => [k, tform.elements[k].value]));
      const t = editingType ? await api("PUT", `/api/types/${editingType.id}`, body) : await api("POST", "/api/types", body);
      if (editingType) TYPES.delete(editingType.name);
      TYPES.set(t.name, t);
    }
    renderTypes();
    agents.forEach(paintCard);
    tdlg.close();
  } catch (err) { status($("#t-err"), err.message, true); }
};

/* ---------- header: projects, theme, run all, stats ---------- */

$("#project").onchange = e => {
  if (dirty && !confirm("Discard unsaved changes?")) { e.target.value = PID; return; }
  location = "/arrange?p=" + encodeURIComponent(e.target.value);
};

const dlg = $("#project-dialog"), pform = $("#project-form");
let editing = null; // null = creating
function openProject(p) {
  editing = p;
  $("h3", pform).textContent = p ? "Project settings" : "New project";
  pform.elements.name.value = p?.name ?? "";
  pform.elements.repo.value = p?.repo ?? "";
  pform.elements.base.value = p?.base ?? "";
  $("#p-err").textContent = "";
  dlg.showModal();
}
$("#new-project").onclick = () => openProject(null);
$("#edit-project").onclick = () => openProject(window.PROJECT);
pform.onsubmit = async e => {
  if (e.submitter?.value === "cancel") return;
  e.preventDefault();
  const body = { name: pform.elements.name.value, repo: pform.elements.repo.value, base: pform.elements.base.value };
  try {
    const p = editing ? await api("PUT", `/api/projects/${editing.id}`, body) : await api("POST", "/api/projects", body);
    location = "/arrange?p=" + encodeURIComponent(p.id);
  } catch (err) { $("#p-err").textContent = err.message; }
};

$("#theme").onclick = () => {
  const t = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = t;
  try { localStorage.setItem("theme", t); } catch {}
};

$("#run-all").onclick = async () => {
  try {
    if (dirty) await save();
    const r = await api("POST", `/api/projects/${PID}/run`);
    say(`started ${r.started}` + (r.errors.length ? " · " + r.errors.join(" · ") : ""), r.errors.length > 0);
  } catch (e) { say(e.message, true); }
};

function table(head, rows) {
  const t = el("table"), tr = el("tr");
  head.forEach(h => tr.append(el("th", "", h)));
  t.append(tr);
  rows.forEach(r => { const row = el("tr"); r.forEach(c => row.append(el("td", "", c))); t.append(row); });
  const wrap = el("div", "tbl");
  wrap.append(t);
  return wrap;
}

$("#stats").onclick = async () => {
  const body = $("#stats-body");
  body.replaceChildren(el("p", "hint", "Loading…"));
  $("#stats-dialog").showModal();
  let s;
  try { s = await api("GET", `/api/projects/${PID}/stats`); } catch (e) { body.replaceChildren(el("p", "note bad", e.message)); return; }
  const pct = (a, b) => b ? Math.round(100 * a / b) + "%" : "–";
  const tiles = el("div", "tiles");
  [[`${s.done}/${s.goals}`, "goals done"], [pct(s.firstTry, s.done), "done on the first try"], [s.failed, "failed or blocked"],
   [s.runs, "agent runs"], [fmtDur(s.seconds * 1000), "agent time"], [fmtTok(s.tokens), "tokens"],
   ["$" + s.cost.toFixed(2), "cost"], [s.done ? "$" + (s.cost / s.done).toFixed(2) : "–", "per finished goal"], [[el("b", "ok", "+" + s.adds), el("b", "bad", "−" + s.dels)], "lines changed"]]
    .forEach(([v, l]) => { const d = el("div"); d.append(...(Array.isArray(v) ? v : [el("b", "", v)]), el("span", "", l)); tiles.append(d); });
  body.replaceChildren(tiles,
    el("h2", "", "By runtime"),
    table(["runtime", "agents", "runs", "done", "failed", "time", "tokens", "cost", "$ / done"],
      s.runtimes.map(r => [r.runtime, r.agents, r.runs, r.done, r.failed, fmtDur(r.seconds * 1000), fmtTok(r.tokens), "$" + r.cost.toFixed(2), r.done ? "$" + (r.cost / r.done).toFixed(2) : "–"])),
    el("h2", "", "By agent"),
    table(["agent", "runtime", "status", "attempts", "runs", "time", "tokens", "cost", "lines"],
      s.agents.map(a => [a.name, a.runtime, a.status, a.attempts, a.runs, fmtDur(a.seconds * 1000), fmtTok(a.tokens), "$" + a.cost.toFixed(2), `+${a.adds} −${a.dels}`])));
};

/* ---------- inspector ---------- */

const insp = $("#inspector"), gform = $("#goal-form"), aform = $("#agent-form");

try { const w = localStorage.getItem("inspector-width"); if (w) insp.style.setProperty("--iw", w); } catch {}
$("#resize").onpointerdown = e => {
  const handle = e.currentTarget;
  try { handle.setPointerCapture(e.pointerId); } catch {}
  handle.classList.add("on");
  document.body.classList.add("resizing");
  handle.onpointermove = m => {
    const w = Math.min(innerWidth * 0.85, Math.max(320, innerWidth - m.clientX));
    insp.style.setProperty("--iw", w + "px");
  };
  handle.onpointerup = () => {
    handle.onpointermove = handle.onpointerup = null;
    handle.classList.remove("on");
    document.body.classList.remove("resizing");
    try { localStorage.setItem("inspector-width", insp.style.getPropertyValue("--iw")); } catch {}
  };
};

let colorChoice = ""; // the agent's own color, or "" for its role color
const roleColor = id => {
  const v = getComputedStyle(cardOf(id)).getPropertyValue("--c").trim();
  return /^#[0-9a-f]{6}$/i.test(v) ? v : "#3d6fe0";
};

function fillParents(id) {
  const sel = $("#a-parent");
  sel.replaceChildren(new Option("nobody (top level)", ""));
  for (const a of agents.values()) if (a.id !== id && !isUnder(a.id, id)) sel.append(new Option(a.name, a.id));
  sel.value = parentOf(id) ?? "";
}

async function select(id) {
  let data;
  try { data = await api("GET", `/api/agents/${encodeURIComponent(id)}`); }
  catch (e) { say(dirty ? "Save the arrangement first" : e.message, true); return; }
  selected = id;
  $$(".card.sel").forEach(c => c.classList.remove("sel"));
  cardOf(id)?.classList.add("sel");
  insp.hidden = false;
  const { agent: a, goal: g } = data;
  $("#i-name").textContent = a.name;
  for (const k of ["title", "body", "criteria", "checks"]) gform.elements[k].value = g[k];
  for (const k of ["name", "runtime", "model", "args", "prompt"]) aform.elements[k].value = a[k];
  aform.elements.tokenSoft.value = a.tokenSoft || "";
  aform.elements.tokenHard.value = a.tokenHard || "";
  colorChoice = a.color || "";
  if (agents.has(id)) agents.get(id).color = colorChoice;
  $("#a-color").value = colorChoice || roleColor(id);
  fillParents(id);
  status($("#a-msg"), "");
  showGoalState(g);
  renderStats();
  showTab(tab);
}

$("#a-color").oninput = e => {
  colorChoice = e.target.value;
  cardOf(selected)?.style.setProperty("--c", colorChoice); // preview; Save settings keeps it
};
$("#a-color-reset").onclick = () => {
  colorChoice = "";
  const a = agents.get(selected);
  paintCard({ ...a, color: "" });
  $("#a-color").value = roleColor(selected);
};
$("#a-parent").onchange = e => {
  const a = agents.get(selected);
  a.parent = e.target.value;
  if (a.parent) ({ x: a.x, y: a.y } = slotUnder(a.parent, a.id));
  paintCard(a);
  drawLinks();
  markDirty();
  status($("#a-msg"), "Moved on the canvas. Save the arrangement to keep it.");
};

function showGoalState(g) {
  const mgr = parentOf(selected);
  $("#g-hint").textContent = kidsOf(selected).length
    ? "Manager: Run plans subgoals for its team, runs them in parallel, reviews and merges their work, then runs these checks on the result."
    : mgr ? `${nameOf(mgr)} sets this goal when it plans; editing it here is fine, but the next plan replaces it.` : "";
  $("#i-status").classList.toggle("busy", BUSY.includes(g.status));
  $("#i-status").dataset.status = g.status;
  $("#g-state").textContent = g.title ? `${g.status}` + (g.attempts ? ` · attempt ${g.attempts}/3` : "") + (g.total ? ` · checks ${g.passed}/${g.total}` : "") : "";
  $("#g-feedback").hidden = !g.feedback;
  $("#g-feedback .feedback").textContent = g.feedback;
}

// limitClass says whether a token count is past the agent's soft or hard limit.
const limitClass = st => st.tokenHard && st.tokens >= st.tokenHard ? "over" : st.tokenSoft && st.tokens >= st.tokenSoft ? "warn" : "";

function renderStats() {
  const st = selected && summary.agents[selected], box = $("#i-stats");
  if (!st) { box.replaceChildren(); return; }
  const parts = [el("span", "", st.status)];
  if (BUSY.includes(st.status) && st.since) parts.push(el("span", "", "working " + fmtDur(Date.now() - st.since)));
  const run = el("span", limitClass(st));
  run.append("this run ", el("b", "", fmtTok(st.tokens) + " tok"), ` · $${st.cost.toFixed(2)}`);
  if (st.tokenSoft || st.tokenHard) run.append(` (limits ${st.tokenSoft ? fmtTok(st.tokenSoft) : "–"} / ${st.tokenHard ? fmtTok(st.tokenHard) : "–"})`);
  parts.push(run, el("span", "", `all time ${fmtTok(st.allTokens)} tok · $${st.allCost.toFixed(2)}`));
  box.replaceChildren(...parts);
}

function closeInspector() {
  selected = null;
  insp.hidden = true;
  $$(".card.sel").forEach(c => c.classList.remove("sel"));
}
$("#i-close").onclick = closeInspector;

function showTab(name) {
  tab = name;
  $$(".tabs button").forEach(b => b.classList.toggle("on", b.dataset.tab === name));
  $$("#inspector .pane").forEach(p => p.hidden = p.dataset.pane !== name);
  if (name === "logs") loadLogs();
  if (name === "diff") { status($("#diff-msg"), ""); loadDiff(); }
}
$$(".tabs button").forEach(b => b.onclick = () => showTab(b.dataset.tab));

const goalBody = () => ({ title: gform.elements.title.value, body: gform.elements.body.value, criteria: gform.elements.criteria.value, checks: gform.elements.checks.value });
gform.onsubmit = async e => {
  e.preventDefault();
  try { await api("PUT", `/api/agents/${selected}/goal`, goalBody()); say("goal saved"); refreshSummary(); }
  catch (err) { say(err.message, true); }
};

aform.onsubmit = async e => {
  e.preventDefault();
  const f = aform.elements;
  const body = { name: f.name.value, runtime: f.runtime.value, model: f.model.value, args: f.args.value, prompt: f.prompt.value,
                 tokenSoft: Number(f.tokenSoft.value) || 0, tokenHard: Number(f.tokenHard.value) || 0, color: colorChoice };
  try {
    await api("PUT", `/api/agents/${selected}`, body);
    const a = agents.get(selected);
    Object.assign(a, { name: body.name, runtime: body.runtime, color: body.color });
    paintCard(a);
    $("#i-name").textContent = a.name;
    status($("#a-msg"), dirty ? "Settings saved. The arrangement still has unsaved changes." : "Saved.");
    refreshSummary();
  } catch (err) { status($("#a-msg"), err.message, true); }
};

$("#i-run").onclick = async () => {
  try {
    if (dirty) await save();
    if (gform.elements.title.value.trim()) await api("PUT", `/api/agents/${selected}/goal`, goalBody());
    await api("POST", `/api/agents/${selected}/run`);
    say("started");
    showTab("logs");
  } catch (e) { say(e.message, true); }
};
$("#i-clone").onclick = async () => {
  try {
    const { agent: src } = await api("GET", `/api/agents/${encodeURIComponent(selected)}`);
    const s = agents.get(selected), parent = parentOf(selected) ?? "";
    const { x, y } = parent ? slotUnder(parent) : { x: s.x + GAP_X, y: s.y };
    const a = addAgent(s.role, src.name + " copy", null, x, y, parent);
    Object.assign(a, { runtime: src.runtime, color: src.color,
      defaults: { model: src.model, args: src.args, prompt: src.prompt, tokenSoft: src.tokenSoft, tokenHard: src.tokenHard } });
    paintCard(a);
    await save();
    refreshCards();
    select(a.id);
  } catch (e) { say(e.message, true); }
};
$("#i-stop").onclick = () => api("POST", `/api/agents/${selected}/stop`).catch(e => say(e.message, true));

/* ---------- logs ---------- */

const log = $("#log"), logPane = $('[data-pane="logs"]');

function evRow(e) {
  const row = el("div", "ev");
  row.dataset.kind = e.kind;
  row.append(el("span", "t", time(e.ts)), el("span", "k", e.kind), el("span", "x", e.text || "·"));
  if (e.raw) {
    row.dataset.raw = "";
    row.raw = e.raw;
    if ($("#raw").checked) row.append(el("pre", "", e.raw));
  }
  return row;
}
log.onclick = e => {
  const row = e.target.closest(".ev[data-raw]");
  if (!row) return;
  const pre = row.querySelector("pre");
  pre ? pre.remove() : row.append(el("pre", "", row.raw));
};
$("#raw").onchange = loadLogs;

async function loadLogs() {
  if (!selected) return;
  const es = await api("GET", `/api/agents/${selected}/events`).catch(e => (say(e.message, true), []));
  log.replaceChildren(...es.map(evRow));
  if (!es.length) log.append(el("div", "empty", "No activity yet. Set a goal and press Run."));
  logPane.scrollTop = logPane.scrollHeight;
}

// Live rows are batched per animation frame and capped, so a chatty agent can't stall the page.
let pending = [], frame = 0;
function appendLive(e) {
  pending.push(e);
  frame ||= requestAnimationFrame(() => {
    const atBottom = logPane.scrollTop + logPane.clientHeight >= logPane.scrollHeight - 30;
    log.querySelector(".empty")?.remove();
    log.append(...pending.map(evRow));
    while (log.childElementCount > MAX_LOG_ROWS) log.firstElementChild.remove();
    if (atBottom) logPane.scrollTop = logPane.scrollHeight;
    pending = []; frame = 0;
  });
}

/* ---------- diff: select hunks to remove or promote; revert checkpoints ---------- */

const selectedHunks = () => $$("#diff .hunk .hh input:checked").map(c => c.dataset.id);
const diffMsg = (t, bad) => status($("#diff-msg"), t, bad);

function updateSelection() {
  $$("#diff .file").forEach(f => {
    const boxes = $$(".hunk .hh input", f), on = boxes.filter(b => b.checked).length;
    const all = $("summary input", f);
    all.checked = on > 0 && on === boxes.length;
    all.indeterminate = on > 0 && on < boxes.length;
  });
  const n = selectedHunks().length;
  $("#n-sel").textContent = n;
  $("#h-remove").disabled = $("#h-promote").disabled = n === 0;
}

async function loadDiff() {
  if (!selected) return false;
  const out = $("#diff");
  let d, cps;
  try {
    [d, cps] = await Promise.all([api("GET", `/api/agents/${selected}/diff`), api("GET", `/api/agents/${selected}/checkpoints`)]);
  } catch (e) { diffMsg(e.message, true); out.replaceChildren(); return false; }
  const files = d.files;
  let adds = 0, dels = 0;
  const nodes = files.map(f => {
    adds += f.adds; dels += f.dels;
    const det = el("details", "file");
    det.open = true;
    const sum = el("summary"), all = el("input");
    all.type = "checkbox";
    all.title = "Select the whole file";
    all.onchange = () => { $$(".hunk .hh input", det).forEach(b => b.checked = all.checked); updateSelection(); };
    sum.append(all, el("span", "", f.path), el("span", "ok", "+" + f.adds), el("span", "bad", "−" + f.dels));
    det.append(sum);
    for (const h of f.hunks ?? []) {
      const hk = el("div", "hunk"), hh = el("div", "hh"), box = el("input");
      box.type = "checkbox";
      box.dataset.id = h.id;
      box.onchange = updateSelection;
      hh.append(box, el("span", "", h.header));
      hk.append(hh);
      for (const l of h.lines ?? []) hk.append(el("div", "l" + (l[0] === "+" ? " a" : l[0] === "-" ? " d" : ""), l));
      det.append(hk);
    }
    return det;
  });
  $("#diff-stat").textContent = files.length ? `${files.length} file${files.length > 1 ? "s" : ""} · +${adds} −${dels}` : "";
  out.replaceChildren(...(nodes.length ? nodes : [el("div", "empty", "No changes yet.")]));
  $("#diff-actions").hidden = !files.length;
  $("#diff-note").replaceChildren(...(d.mergedInto ? [el("p", "note",
    `This work is already merged into ${d.mergedInto}. Removing a change here won't remove it from ${d.mergedInto}; open ${d.mergedInto} and remove it there, or re-run ${d.mergedInto}.`)] : []));
  const mgr = parentOf(selected);
  $("#h-promote").hidden = !mgr;
  if (mgr) $("#h-promote").textContent = "Promote to " + nameOf(mgr);
  updateSelection();

  $("#checkpoints").replaceChildren(...(cps.length ? cps.map(c => {
    const row = el("div", "cp"), btn = el("button", "", "Revert");
    btn.title = "Undo this checkpoint with a new commit";
    btn.onclick = () => diffAction(btn, "Reverting…", () => api("POST", `/api/agents/${selected}/revert`, { sha: c.sha }), `Reverted ${c.sha}. The undo is saved as a new checkpoint.`);
    row.append(el("span", "t", c.sha), el("span", "", c.subject), el("span", "t", time(c.time * 1000)), btn);
    return row;
  }) : [el("div", "empty", "No checkpoints yet.")]));
  return true;
}

// diffAction runs a change, shows progress then the outcome inside the Diff tab, and reloads the diff.
async function diffAction(btn, working, call, done) {
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = working;
  diffMsg(working);
  try { await call(); diffMsg(done); say(done); }
  catch (e) { diffMsg(e.message, true); }
  btn.textContent = label;
  btn.disabled = false;
  await loadDiff();
}
$("#h-remove").onclick = e => {
  const ids = selectedHunks();
  diffAction(e.currentTarget, "Removing…", () => api("POST", `/api/agents/${selected}/hunks`, { action: "remove", ids }),
    `Removed ${ids.length} change${ids.length > 1 ? "s" : ""}. Saved as a checkpoint; the agent won't add ${ids.length > 1 ? "them" : "it"} back.`);
};
$("#h-promote").onclick = e => {
  const ids = selectedHunks(), to = nameOf(parentOf(selected));
  diffAction(e.currentTarget, "Promoting…", () => api("POST", `/api/agents/${selected}/hunks`, { action: "promote", ids }),
    `Copied ${ids.length} change${ids.length > 1 ? "s" : ""} into ${to}'s work.`);
};
$("#diff-refresh").onclick = async () => {
  diffMsg("Refreshing…");
  if (await loadDiff()) diffMsg("Up to date as of " + time(Date.now()) + ".");
};

/* ---------- merge into a branch of the user's repo ---------- */

const mdlg = $("#merge-dialog"), mform = $("#merge-form");
let previewTimer = 0;

async function loadPreview() {
  const box = $("#m-preview"), target = mform.elements.target.value.trim();
  $("#m-go").disabled = true;
  box.replaceChildren(el("span", "hint", "Checking…"));
  let p;
  try { p = await api("GET", `/api/agents/${selected}/merge?target=${encodeURIComponent(target)}`); }
  catch (e) { box.replaceChildren(el("p", "note bad", e.message)); return; }
  const lines = [];
  if (!mform.elements.target.value) mform.elements.target.value = p.target;
  const st = summary.agents[selected];
  if (st && st.status !== "done") lines.push(el("p", "note", `${nameOf(selected)} isn't done (${st.status}), so this work hasn't passed its checks.`));
  lines.push(el("span", "", p.commits && p.files
    ? `${p.commits} commit${p.commits > 1 ? "s" : ""} · ${p.files} file${p.files !== 1 ? "s" : ""} · +${p.adds} −${p.dels}`
    : `Nothing to merge: ${p.target} already has all of this work.`));
  if (!p.exists) lines.push(el("span", "", `Creates branch ${p.target} from ${p.start}.`));
  if (p.checkedOut) lines.push(el("p", p.dirty ? "note bad" : "note", p.dirty
    ? `${p.target} is checked out in ${p.checkedOut} with uncommitted changes. Commit or stash them first.`
    : `${p.target} is checked out in ${p.checkedOut}; the files there will update.`));
  if (p.conflicts.length) {
    const ul = el("ul");
    p.conflicts.forEach(f => ul.append(el("li", "", f)));
    lines.push(el("p", "note bad", `Conflicts with ${p.target} in:`), ul);
  }
  $("#m-ff").disabled = !p.canFF;
  if (!p.canFF && mform.elements.strategy.value === "ff") mform.elements.strategy.value = "merge";
  $("#m-go").disabled = !p.commits || !p.files || p.dirty || p.conflicts.length > 0;
  box.replaceChildren(...lines);
}

$("#i-merge").onclick = async () => {
  $("#m-agent").textContent = nameOf(selected);
  mform.elements.target.value = "";
  mform.elements.message.value = gform.elements.title.value;
  status($("#m-msg"), "");
  mdlg.showModal();
  await loadPreview();
};
mform.elements.target.oninput = () => { clearTimeout(previewTimer); previewTimer = setTimeout(loadPreview, 350); };
mform.onsubmit = async e => {
  if (e.submitter?.value === "cancel") return;
  e.preventDefault();
  $("#m-go").disabled = true;
  status($("#m-msg"), "Merging…");
  try {
    const r = await api("POST", `/api/agents/${selected}/merge`, {
      target: mform.elements.target.value.trim(), strategy: mform.elements.strategy.value, message: mform.elements.message.value });
    status($("#m-msg"), `Merged into ${r.target} as ${r.sha}.`);
    say(`merged ${nameOf(selected)} into ${r.target}`);
  } catch (err) { status($("#m-msg"), err.message, true); }
  loadPreview();
};

/* ---------- status: cards, links, top bar ---------- */

let summary = { agents: {}, cost: 0 };

function refreshCards() {
  for (const a of agents.values()) {
    const card = a.el, st = summary.agents[a.id];
    card.dataset.status = st?.status ?? "idle";
    const busy = BUSY.includes(card.dataset.status);
    card.classList.toggle("busy", busy);
    card.querySelector(".now").textContent = st?.now || (st?.title ? "goal: " + st.title : "no goal yet");
    const b = card.querySelector(".badges");
    b.replaceChildren();
    if (!st) continue;
    if (busy && st.since) {
      const t = el("span", "elapsed", fmtDur(Date.now() - st.since));
      t.dataset.since = st.since;
      b.append(t);
    }
    if (st.total) b.append(el("span", st.passed === st.total ? "ok" : "bad", `✓ ${st.passed}/${st.total}`));
    if (st.adds || st.dels) b.append(el("span", "", `+${st.adds} −${st.dels}`));
    if (st.attempts > 1) b.append(el("span", "", `try ${st.attempts}/3`));
    if (st.tokens) {
      const lc = limitClass(st);
      const tok = el("span", lc, (lc ? "⚠ " : "") + fmtTok(st.tokens) + " tok");
      tok.title = `$${st.cost.toFixed(2)} this run` + (lc === "over" ? " · over the hard limit" : lc === "warn" ? " · over the soft limit" : "");
      b.append(tok);
    }
  }
  drawLinks();
}

async function refreshSummary() {
  try { summary = await api("GET", `/api/projects/${PID}/summary`); } catch { return; }
  refreshCards();
  renderStats();
  const all = Object.entries(summary.agents);
  const goals = all.filter(([, s]) => s.title);
  const done = goals.filter(([, s]) => s.status === "done").length;
  const needs = all.filter(([, s]) => NEEDS.includes(s.status));
  const running = all.filter(([, s]) => BUSY.includes(s.status)).length;
  $("#n-done").textContent = done;
  $("#n-goals").textContent = goals.length;
  $("#n-running").textContent = running;
  $("#bar .live").hidden = running === 0;
  $("#bar .prog i").style.width = goals.length ? (100 * done / goals.length) + "%" : 0;
  $("#cost").textContent = summary.cost.toFixed(2);
  $("#n-needs").textContent = needs.length;
  $("#needs").dataset.n = needs.length;
  $("#needs ul").replaceChildren(...needs.map(([id, s]) => {
    const li = el("li", "", `✗ ${nameOf(id)}: ${s.status} · ${s.title}`);
    li.onclick = () => { $("#needs").open = false; select(id); };
    return li;
  }));
  if (selected && summary.agents[selected]) {
    const g = (await api("GET", `/api/agents/${selected}`).catch(() => null))?.goal;
    if (g) showGoalState(g);
  }
}

let summaryTimer = 0;
// a throttle, not a debounce: busy agents send usage updates nonstop, which would keep postponing a debounced refresh
const scheduleSummary = () => { summaryTimer ||= setTimeout(() => { summaryTimer = 0; refreshSummary(); }, 150); };

// tick the "working for" timers without refetching anything
setInterval(() => {
  for (const t of $$("#canvas .elapsed")) t.textContent = fmtDur(Date.now() - Number(t.dataset.since));
  if (selected && BUSY.includes(summary.agents[selected]?.status)) renderStats();
}, 1000);

/* ---------- live updates ---------- */

const es = new EventSource("/api/events?project=" + encodeURIComponent(PID));
es.onmessage = m => {
  const msg = JSON.parse(m.data);
  if (msg.type === "hello") {
    $("#stale").hidden = msg.build === String(window.BUILD); // this page came from an older run of the server
  } else if (msg.type === "event") {
    const e = msg.event;
    if (e.kind !== "stderr") {
      const now = cardOf(e.agent)?.querySelector(".now");
      if (now) now.textContent = `${e.kind}: ${e.text}`;
    }
    if (e.agent === selected && tab === "logs") appendLive(e);
  } else if (msg.type === "status" || msg.type === "usage") {
    scheduleSummary();
    if (msg.type === "status" && msg.agent === selected && tab === "diff" && !selectedHunks().length) loadDiff(); // don't wipe a selection in progress
  }
};
$("#reload").onclick = () => location.reload();

/* ---------- start ---------- */

renderTypes();
if ([...agents.values()].some(a => a.x == null || a.y == null)) layout(false);
agents.forEach(makeCard);
refreshCards();
refreshSummary();
document.fonts?.ready.then(drawLinks); // box heights settle once fonts load
