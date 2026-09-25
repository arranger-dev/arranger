// Arranger UI: a free-form canvas of agents, the inspector (goal / logs / diff / settings),
// merging, stats, and live status over SSE.
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const PID = document.body.dataset.project;
const main = $("main"), canvas = $("#canvas"), linksSvg = $("#links");
const SVG = "http://www.w3.org/2000/svg";
const MAX_LOG_ROWS = 1000;
const BUSY = ["starting", "running", "verifying", "planning", "waiting", "reviewing", "fixing"];
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

// linkFlow says whether the link between a manager and a report is live, and which way it moves:
// "down" while the manager hands out subgoals (planning, or the report is just starting),
// "up" while the report works for its manager, or the manager reviews what it sent back.
function linkFlow(mgrStatus, kidStatus) {
  if (kidStatus === "starting" || mgrStatus === "planning" || mgrStatus === "fixing") return { dir: "down", status: kidStatus === "starting" ? kidStatus : mgrStatus };
  if (BUSY.includes(kidStatus)) return { dir: "up", status: kidStatus };
  if (mgrStatus === "reviewing" && kidStatus === "done") return { dir: "up", status: mgrStatus };
  return null;
}

// drawLinks draws a dotted curve from each manager down to each report, animated while they work together.
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
    const flow = linkFlow(p.el.dataset.status, a.el.dataset.status);
    if (flow) {
      path.dataset.status = flow.status;
      path.classList.add("flow", flow.dir);
    }
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

// slug makes a role usable in an agent id (ids name folders and git branches): "Frontend Guy" → "frontend-guy".
const slug = s => s.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 32) || "agent";

function addAgent(role, name, type, x, y, parent) {
  const a = {
    id: slug(role) + "-" + Date.now().toString(36) + Math.random().toString(36).slice(2, 5),
    name, role, runtime: type?.runtime ?? "claude", parent: parent ?? "", color: "",
    x: Math.max(0, Math.round(x / SNAP) * SNAP), y: Math.max(0, Math.round(y / SNAP) * SNAP),
    defaults: type ? { model: type.model, args: type.args, prompt: type.prompt }
      : window.ROLES?.[role] ? { prompt: window.ROLES[role] } : null, // a default type starts with its role's instructions
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

// linkFrom draws a connection from one of a box's dots, like draw.io. From the bottom dot, the agent
// it's released over reports to this one; from a side dot, whichever box is higher on the canvas
// manages (level: the one dropped on reports).
function linkFrom(e, c) {
  const src = agents.get(c.dataset.id), draft = document.createElementNS(SVG, "path"), cls = e.target.classList;
  const side = cls.contains("left") ? -1 : cls.contains("right") ? 1 : 0, down = cls.contains("down");
  draft.classList.add("draft");
  canvas.classList.add("linking");
  c.classList.add("src");
  e.target.classList.add("on");
  let over = null;
  try { e.target.setPointerCapture(e.pointerId); } catch {}
  const x1 = side ? src.x + (side > 0 ? c.offsetWidth : 0) : src.x + c.offsetWidth / 2;
  const y1 = side ? src.y + c.offsetHeight / 2 : src.y + c.offsetHeight, dir = 1;
  // the manager and the report a drop would make, and whether that changes anything without a loop
  const managesIt = id => side ? agents.get(id).y >= src.y : down;
  const pair = id => managesIt(id) ? { mgr: src.id, kid: id } : { mgr: id, kid: src.id };
  const allowed = id => { const { mgr, kid } = pair(id); return agents.get(kid).parent !== mgr && !isUnder(mgr, kid); };
  e.target.onpointermove = m => {
    const r = canvas.getBoundingClientRect(), x2 = m.clientX - r.left, y2 = m.clientY - r.top;
    const bend = Math.max(40, Math.abs((side ? x2 - x1 : y2 - y1)) / 2);
    draft.setAttribute("d", side
      ? `M${x1},${y1} C${x1 + side * bend},${y1} ${x2 - side * bend},${y2} ${x2},${y2}`
      : `M${x1},${y1} C${x1},${y1 + dir * bend} ${x2},${y2 - dir * bend} ${x2},${y2}`);
    if (!draft.isConnected) linksSvg.append(draft); // a live refresh may have redrawn the links
    over?.classList.remove("drop");
    over = document.elementsFromPoint(m.clientX, m.clientY).find(x => x !== c && x.matches?.("#canvas .card"));
    if (over && !allowed(over.dataset.id)) over = null; // no-ops and loops
    over?.classList.add("drop");
  };
  e.target.onpointerup = e.target.onpointercancel = () => {
    e.target.onpointermove = e.target.onpointerup = e.target.onpointercancel = null;
    draft.remove();
    canvas.classList.remove("linking");
    c.classList.remove("src");
    e.target.classList.remove("on");
    if (!over) return;
    over.classList.remove("drop");
    const { mgr, kid: kidId } = pair(over.dataset.id), kid = agents.get(kidId);
    kid.parent = mgr;
    paintCard(kid);
    say(`${kid.name} now reports to ${nameOf(mgr)} · unsaved`);
    dirty = true;
    drawLinks();
  };
}

// boxes on the canvas: drag to move; release over another box to report to it; click to open
canvas.addEventListener("pointerdown", e => {
  const c = e.target.closest(".card");
  if (!c || e.button !== 0 || e.target.classList.contains("x")) return;
  if (e.target.classList.contains("port")) { e.preventDefault(); linkFrom(e, c); return; }
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
// ask shows the styled confirm dialog and resolves true when the user presses the action button,
// which is red unless danger is false.
function ask(title, text, action, keep = "Cancel", danger = true) {
  const d = $("#confirm-dialog");
  $("#c-input").hidden = true;
  $("#c-ok").classList.toggle("danger", danger);
  $("#c-cancel").textContent = keep;
  $("h3", d).textContent = title;
  $("#c-text").textContent = text;
  $("#c-text").hidden = !text;
  $("#c-ok").textContent = action;
  d.returnValue = "";
  d.showModal();
  $("#c-cancel").focus(); // Enter shouldn't delete by accident
  return new Promise(r => d.addEventListener("close", () => r(d.returnValue === "ok"), { once: true }));
}

canvas.addEventListener("click", async e => {
  if (!e.target.classList.contains("x")) return;
  const id = e.target.closest(".card").dataset.id, a = agents.get(id);
  if (BUSY.includes(a.el.dataset.status)) { say(`${a.name} is running; stop it before removing it`, true); return; }
  const n = kidsOf(id).length, st = summary.agents[id];
  const notes = [];
  if (n) notes.push(`Its ${n === 1 ? "report moves" : `${n} reports move`} up to ${a.parent ? nameOf(a.parent) : "the top level"}.`);
  if (st?.adds || st?.dels) notes.push(`Its work (+${st.adds} −${st.dels}) and its branch arranger/${id} are deleted when you save. Merge it first if you want to keep it.`);
  if (!await ask(`Remove ${a.name}?`, notes.join(" "), "Remove")) return;
  if (!agents.has(id)) return; // removed some other way while the dialog was open
  kidsOf(id).forEach(k => k.parent = a.parent); // reports move up a level
  if (selected === id) closeInspector();
  a.el.remove();
  agents.delete(id);
  markDirty();
  drawLinks();
});
// askText is ask with a text field; it resolves to the text, or null when cancelled.
async function askText(title, value, action) {
  const done = ask(title, "", action, "Cancel", false), input = $("#c-input");
  input.hidden = false;
  input.value = value;
  input.focus();
  input.select();
  return (await done) ? input.value.trim() : null;
}

canvas.addEventListener("dblclick", async e => {
  const c = e.target.closest(".card");
  const a = c && agents.get(c.dataset.id);
  if (!a) return;
  const v = await askText("Rename agent", a.name, "Rename");
  if (v && v !== a.name) { a.name = v; paintCard(a); markDirty(); }
});
$("#tidy").onclick = () => {
  layout(true);
  agents.forEach(paintCard);
  drawLinks();
  main.scrollTo({ left: 0, top: 0, behavior: "smooth" }); // the tidy tree starts at the top left; don't leave the view on empty canvas
  markDirty();
};

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
      if (!await ask(`Delete the ${editingType.name} type?`, "You can't drag new agents from it anymore. Agents already on the canvas keep their settings.", "Delete")) return;
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

// switching projects leaves this page; unsaved canvas changes would be lost, so ask first
$$("#project ul a").forEach(link => link.onclick = async e => {
  $("#project").open = false;
  if (link.classList.contains("on")) { e.preventDefault(); return; }
  if (!dirty) return;
  e.preventDefault();
  if (await ask("Discard unsaved changes?", "The canvas has changes you haven't saved. Switching projects throws them away.", "Discard and switch")) {
    dirty = false; // beforeunload would ask again
    location = link.href;
  }
});
// open menus close on a click elsewhere or Escape
document.addEventListener("click", e => $$("details.menu[open], #needs[open]").forEach(d => { if (!d.contains(e.target)) d.open = false; }));
document.addEventListener("keydown", e => { if (e.key === "Escape") $$("details.menu[open], #needs[open]").forEach(d => { d.open = false; d.querySelector("summary").focus(); }); });

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
  const tops = [...agents.values()].filter(a => !parentOf(a.id) && summary.agents[a.id]?.title);
  if (!tops.length) { say("No top-level agent has a goal yet. Select one and set its goal first.", true); return; }
  const names = tops.map(a => a.name).join(", ");
  if (!await ask(`Run all ${tops.length} top-level ${tops.length === 1 ? "agent" : "agents"}?`,
    `Starts ${names} from ${tops.length === 1 ? "its goal" : "their goals"}. Managers plan for their teams, so every agent below them may run too.` +
    (dirty ? " Unsaved changes on the canvas are saved first." : ""), "Run all", "Cancel", false)) return;
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
   [s.done ? fmtTok(Math.round(s.tokens / s.done)) : "–", "tokens per finished goal"], [[el("b", "ok", "+" + s.adds), el("b", "bad", "−" + s.dels)], "lines changed"]]
    .forEach(([v, l]) => { const d = el("div"); d.append(...(Array.isArray(v) ? v : [el("b", "", v)]), el("span", "", l)); tiles.append(d); });
  body.replaceChildren(tiles,
    el("h2", "", "By runtime"),
    table(["runtime", "agents", "runs", "done", "failed", "time", "tokens", "tokens / done"],
      s.runtimes.map(r => [r.runtime, r.agents, r.runs, r.done, r.failed, fmtDur(r.seconds * 1000), fmtTok(r.tokens), r.done ? fmtTok(Math.round(r.tokens / r.done)) : "–"])),
    el("h2", "", "By agent"),
    table(["agent", "runtime", "status", "attempts", "runs", "time", "tokens", "lines"],
      s.agents.map(a => [a.name, a.runtime, a.status, a.attempts, a.runs, fmtDur(a.seconds * 1000), fmtTok(a.tokens), `+${a.adds} −${a.dels}`])));
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

$("#i-dir").onclick = async e => {
  const path = e.currentTarget.dataset.path;
  if (!path) return;
  try { await navigator.clipboard.writeText(path); say("Copied " + path); }
  catch { say("Couldn't copy the path", true); }
};

// selAgent is the selected agent's settings as last saved.
let selAgent = null;

// showApprove shows the plan-approval setting, in Settings and next to the goal, for managers only.
function showApprove(on) {
  const manager = selected && kidsOf(selected).length > 0;
  aform.elements.approvePlan.checked = $("#g-approve").checked = on;
  $("#a-approve-row").style.display = $("#g-approve-row").style.display = manager ? "flex" : "none";
}

// The Goal tab's checkbox saves the setting right away; the rest of the settings stay as saved.
$("#g-approve").onchange = async e => {
  const on = e.target.checked, id = selected;
  try {
    await api("PUT", `/api/agents/${encodeURIComponent(id)}`, { ...selAgent, approvePlan: on });
    if (id !== selected) return;
    selAgent.approvePlan = on;
    agents.get(id).approvePlan = on;
    aform.elements.approvePlan.checked = on;
    status($("#g-msg"), on ? `${nameOf(id)} will show you its plan before its team runs.` : `${nameOf(id)}'s team starts as soon as it has a plan.`);
  } catch (err) {
    e.target.checked = !on;
    status($("#g-msg"), err.message, true);
  }
};

async function select(id) {
  let data;
  try { data = await api("GET", `/api/agents/${encodeURIComponent(id)}`); }
  catch (e) { say(dirty ? "Save the arrangement first" : e.message, true); return; }
  selected = id;
  $$(".card.sel").forEach(c => c.classList.remove("sel"));
  cardOf(id)?.classList.add("sel");
  insp.hidden = false;
  const { agent: a, goal: g } = data;
  selAgent = a;
  $("#i-name").textContent = a.name;
  const d = $("#i-dir");
  d.textContent = data.dir ? `(${data.dir})` : "";
  d.dataset.path = data.dir;
  d.title = data.dir ? `Worktree on branch ${data.branch}. Click to copy the path.` : "";
  for (const k of ["title", "body", "criteria", "checks"]) gform.elements[k].value = g[k];
  for (const k of ["name", "runtime", "model", "args", "prompt"]) aform.elements[k].value = a[k];
  aform.elements.tokenSoft.value = a.tokenSoft || "";
  aform.elements.tokenHard.value = a.tokenHard || "";
  showApprove(a.approvePlan);
  colorChoice = a.color || "";
  if (agents.has(id)) agents.get(id).color = colorChoice;
  $("#a-color").value = colorChoice || roleColor(id);
  fillParents(id);
  status($("#a-msg"), "");
  status($("#i-msg"), "");
  status($("#g-msg"), "");
  if (id !== revisingFor) { $("#r-change").value = ""; status($("#r-msg"), ""); } // a draft belongs to its agent
  showGoalState(g);
  renderPlan();
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
  const mgr = parentOf(selected), team = kidsOf(selected);
  // once an agent has run (and isn't running), changes go through Request changes, not goal edits
  $("#revise").hidden = !g.title || g.status === "idle" || g.status === "awaiting" || BUSY.includes(g.status);
  $("#r-hint").textContent = team.length
    ? `${nameOf(selected)} passes each part to the ${team.length === 1 ? "agent" : "agents"} it concerns and re-runs only them. Everyone keeps their work; ${nameOf(selected)} reviews and merges the changes and runs its checks again.`
    : `${nameOf(selected)} keeps its work and changes only this, then its checks run again.`;
  $("#i-run").title = team.length ? "Plan again from the goal and run the whole team" : "Run from the goal";
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

// WORKING says what a busy agent is doing, for the banner at the top of its Goal tab.
const WORKING = { starting: "is starting", running: "is working", verifying: "is running its checks", planning: "is planning for its team",
                  waiting: "is waiting for its team", reviewing: "is reviewing its team's work",
                  fixing: "is working out who fixes its failing checks" };

function renderWorking(st) {
  if (tab === "logs") renderLogNow();
  const box = $("#g-working"), busy = st && BUSY.includes(st.status);
  box.hidden = !busy;
  if (!busy) return;
  box.dataset.status = st.status;
  $("#g-working-dot").dataset.status = st.status;
  $("#g-working-text").textContent = `${nameOf(selected)} ${WORKING[st.status] ?? st.status}` + (st.since ? ` · ${fmtDur(Date.now() - st.since)}` : "");
  $("#g-working-note").textContent = st.status === "planning" && agents.get(selected)?.approvePlan
    ? "It's reading the repo and writing the plan. The plan shows up here for your approval; nothing runs until you approve it." : "";
}

function renderStats() {
  const st = selected && summary.agents[selected], box = $("#i-stats");
  renderWorking(st);
  if (!st) { box.replaceChildren(); return; }
  const parts = [el("span", "", st.status)];
  if (BUSY.includes(st.status) && st.since) parts.push(el("span", "", "working " + fmtDur(Date.now() - st.since)));
  const run = el("span", limitClass(st));
  run.append("this run ", el("b", "", fmtTok(st.tokens) + " tok"));
  if (st.tokenSoft || st.tokenHard) run.append(` (limits ${st.tokenSoft ? fmtTok(st.tokenSoft) : "–"} / ${st.tokenHard ? fmtTok(st.tokenHard) : "–"})`);
  parts.push(run, el("span", "", `all time ${fmtTok(st.allTokens)} tok`));
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
  if (name === "diff") { status($("#diff-msg"), ""); shownDiff = ""; loadDiff(); }
}
$$(".tabs button").forEach(b => b.onclick = () => showTab(b.dataset.tab));

gform.addEventListener("input", () => status($("#g-msg"), "")); // "Saved" no longer holds once you edit
const goalBody = () => ({ title: gform.elements.title.value, body: gform.elements.body.value, criteria: gform.elements.criteria.value, checks: gform.elements.checks.value });
gform.onsubmit = async e => {
  e.preventDefault();
  const btn = e.submitter;
  if (btn) btn.disabled = true;
  status($("#g-msg"), "Saving…");
  try {
    await api("PUT", `/api/agents/${selected}/goal`, goalBody());
    status($("#g-msg"), "Saved at " + time(Date.now()) + (gform.elements.checks.value.trim() ? "." : ". Add a check before running: a shell command that exits 0 when the goal is met."), !gform.elements.checks.value.trim());
    refreshSummary();
  } catch (err) { status($("#g-msg"), err.message, true); }
  if (btn) btn.disabled = false;
};

aform.onsubmit = async e => {
  e.preventDefault();
  const f = aform.elements;
  const body = { name: f.name.value, runtime: f.runtime.value, model: f.model.value, args: f.args.value, prompt: f.prompt.value,
                 tokenSoft: Number(f.tokenSoft.value) || 0, tokenHard: Number(f.tokenHard.value) || 0, color: colorChoice,
                 approvePlan: f.approvePlan.checked };
  try {
    await api("PUT", `/api/agents/${selected}`, body);
    const a = agents.get(selected);
    Object.assign(a, { name: body.name, runtime: body.runtime, color: body.color, approvePlan: body.approvePlan });
    Object.assign(selAgent, body);
    $("#g-approve").checked = body.approvePlan;
    paintCard(a);
    $("#i-name").textContent = a.name;
    status($("#a-msg"), dirty ? "Settings saved. The arrangement still has unsaved changes." : "Saved.");
    refreshSummary();
  } catch (err) { status($("#a-msg"), err.message, true); }
};

// runMsg reports on Run next to the button, where the user is looking; the header line is easy to miss.
const runMsg = (t, bad) => { status($("#i-msg"), t, bad); if (t) say(t, bad); };

$("#i-run").onclick = async e => {
  const btn = e.currentTarget, id = selected, f = gform.elements;
  const isManager = kidsOf(id).length > 0;
  // catch what the server would refuse, and point at the field to fix
  const missing = !f.title.value.trim() ? "title" : !f.checks.value.trim() ? "checks" : "";
  if (missing) {
    showTab("goal");
    f[missing].focus();
    runMsg(missing === "title" ? "Give the goal a title first." : "Add at least one check (a shell command that exits 0 when the goal is met), then Run.", true);
    return;
  }
  btn.disabled = true;
  runMsg("Starting…");
  try {
    if (dirty) await save();
    await api("PUT", `/api/agents/${id}/goal`, goalBody()); // Run uses what's in the form, saved or not
    await api("POST", `/api/agents/${id}/run`);
    setStatus(id, "starting");
    runMsg(!isManager ? `${nameOf(id)} is working.` : agents.get(id)?.approvePlan
      ? `${nameOf(id)} is planning. You'll review the plan here before anyone on the team starts.`
      : `${nameOf(id)} is planning for its team.`);
    refreshSummary();
    showTab("logs");
  } catch (err) { runMsg(err.message, true); }
  btn.disabled = false;
};
// Request changes: re-run an agent that already worked with what the user wants different.
let revisingFor = null;
$("#r-change").oninput = () => { revisingFor = selected; };
$("#r-change").onkeydown = e => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); $("#r-go").click(); } };
$("#r-go").onclick = async e => {
  const btn = e.currentTarget, id = selected, change = $("#r-change").value.trim();
  if (!change) { status($("#r-msg"), "Describe what should change first.", true); $("#r-change").focus(); return; }
  btn.disabled = true;
  status($("#r-msg"), "Starting…");
  try {
    if (dirty) await save();
    await api("POST", `/api/agents/${id}/revise`, { change });
    $("#r-change").value = "";
    revisingFor = null;
    status($("#r-msg"), "");
    setStatus(id, "starting");
    runMsg(kidsOf(id).length ? `${nameOf(id)} is passing your changes to its team.` : `${nameOf(id)} is making your changes.`);
    refreshSummary();
    showTab("logs");
  } catch (err) { status($("#r-msg"), err.message, true); }
  btn.disabled = false;
};

$("#i-clone").onclick = async () => {
  if (!await ask(`Add a copy of ${nameOf(selected)}?`, "It gets the same settings and manager, but no goal." + (dirty ? " Unsaved changes on the canvas are saved too." : ""),
    "Add copy", "Cancel", false)) return;
  try {
    const { agent: src } = await api("GET", `/api/agents/${encodeURIComponent(selected)}`);
    const s = agents.get(selected), parent = parentOf(selected) ?? "";
    const { x, y } = parent ? slotUnder(parent) : { x: s.x + GAP_X, y: s.y };
    const a = addAgent(s.role, src.name + " copy", null, x, y, parent);
    Object.assign(a, { runtime: src.runtime, color: src.color,
      defaults: { model: src.model, args: src.args, prompt: src.prompt, tokenSoft: src.tokenSoft, tokenHard: src.tokenHard, approvePlan: src.approvePlan } });
    paintCard(a);
    await save();
    refreshCards();
    select(a.id);
  } catch (e) { say(e.message, true); }
};
$("#i-stop").onclick = () => api("POST", `/api/agents/${selected}/stop`).then(() => runMsg("Stopping…")).catch(e => runMsg(e.message, true));

/* ---------- plan approval ---------- */

// plans: manager id -> its draft plan waiting for the user's decision
const plans = new Map();
const baseTitle = document.title;
const planItems = d => d.plan.subgoals || d.plan.changes || [];

// loadPlan fetches the manager's waiting plan. Only the newest request per manager counts: an older
// one can come back after it, e.g. "no plan" from just before a new plan was made.
const planLoads = new Map();
async function loadPlan(id) {
  const n = (planLoads.get(id) ?? 0) + 1;
  planLoads.set(id, n);
  let d = null;
  try { d = await api("GET", `/api/agents/${encodeURIComponent(id)}/plan`); } catch {}
  if (planLoads.get(id) !== n) return;
  d ? plans.set(id, d) : plans.delete(id);
  planChanged(id);
}

function planChanged(id) {
  document.title = (plans.size ? "Plan ready · " : "") + baseTitle;
  refreshCards();
  if (id === selected) renderPlan();
}

// syncPlans fetches drafts for managers that wait on the user and drops the ones that don't anymore.
function syncPlans() {
  for (const [id, s] of Object.entries(summary.agents)) if (s.status === "awaiting" && !plans.has(id)) loadPlan(id);
  for (const id of [...plans.keys()]) if (summary.agents[id]?.status !== "awaiting") { plans.delete(id); planChanged(id); }
}

// planEdit is the plan as the user is editing it: report id -> {on, f}, for one draft.
let planEdit = null;

function renderPlan() {
  const d = selected && plans.get(selected);
  $("#plan-review").hidden = !d;
  if (!d) { planEdit = null; return; }
  if (planEdit?.draft !== d.id) {
    const items = new Map();
    for (const k of kidsOf(selected)) {
      const x = planItems(d).find(i => i.agent === k.id);
      items.set(k.id, { on: !!x, f: x ? { ...structuredClone(x), checks: x.checks || [] }
        : d.kind === "revision" ? { agent: k.id, change: "", checks: [] } : { agent: k.id, title: "", body: "", criteria: "", checks: [] } });
    }
    planEdit = { draft: d.id, kind: d.kind, mgr: selected, items, open: planItems(d)[0]?.agent };
    $("#pr-feedback").value = "";
    status($("#pr-msg"), "");
  }
  drawPlan();
}

const WEAK_CHECK = /^(true|:|exit 0|pwd|ls(\s.*)?|echo(\s.*)?)$/;
function checkWarnings(kind, checks) {
  const ws = checks.filter(c => WEAK_CHECK.test(c)).map(c => `\`${c}\` can't fail, so it proves nothing.`);
  if (kind === "plan" && !checks.length) ws.unshift("No checks: nothing can prove this is done.");
  return ws;
}
const checkLines = v => v.split("\n").map(l => l.trim()).filter(Boolean);

function drawPlan() {
  const { kind, mgr, items } = planEdit, on = [...items.values()].filter(i => i.on).length;
  const st = summary.agents[mgr];
  $("#pr-title").textContent = kind === "revision" ? `${nameOf(mgr)}'s plan for your change` : `${nameOf(mgr)}'s plan`;
  $("#pr-sub").textContent = `${on} of ${items.size} reports get work` + (st?.tokens ? ` · planning used ${fmtTok(st.tokens)} tokens` : "") +
    ". Nothing runs until you approve.";
  $("#pr-approve").disabled = on === 0;
  $("#pr-approve").title = on ? "" : "Give at least one report work, or cancel.";
  $("#pr-items").replaceChildren(...[...items].map(([id, it]) => it.on ? planCard(id, it) : leftOut(id, it)));
}

// planCard is one report's part of the plan, editable in place.
function planCard(id, it) {
  const det = el("details", "pr-item"), sum = el("summary"), f = it.f, team = kidsOf(id).length;
  det.open = planEdit.open === id;
  det.ontoggle = () => { if (det.open) planEdit.open = id; };
  const what = el("span", "what");
  const showWhat = () => what.textContent = (team ? `manager, ${team} reports · ` : "") + (planEdit.kind === "revision" ? f.change : f.title);
  showWhat();
  const skip = el("button", "", "Skip");
  skip.type = "button";
  skip.title = "Leave this report out of this run; its current goal stays";
  skip.onclick = e => { e.preventDefault(); it.on = false; drawPlan(); };
  sum.append(el("span", "who", nameOf(id)), what, skip);

  const fields = el("div", "fields"), warn = el("div");
  const showWarn = () => warn.replaceChildren(...checkWarnings(planEdit.kind, f.checks).map(w => el("p", "warn", "⚠ " + w)));
  const field = (label, key, tag = "textarea", cls = "") => {
    const input = el(tag, cls);
    input.value = key === "checks" ? f.checks.join("\n") : f[key] ?? "";
    input.oninput = () => {
      if (key === "checks") { f.checks = checkLines(input.value); showWarn(); } else { f[key] = input.value; showWhat(); }
    };
    fields.append(el("label", "", label), input);
  };
  if (planEdit.kind === "revision") {
    field("Change", "change");
    field("New checks (optional), one per line", "checks", "textarea", "mono");
  } else {
    field("Goal", "title", "input");
    field("Details", "body");
    field("Acceptance criteria", "criteria");
    field("Checks: one shell command per line, all must exit 0", "checks", "textarea", "mono");
  }
  fields.append(warn);
  showWarn();
  det.append(sum, fields);
  return det;
}

// leftOut is a report the plan gives no work, with a way to bring it in.
function leftOut(id, it) {
  const row = el("div", "pr-item out"), noGoal = planEdit.kind === "revision" && !summary.agents[id]?.title;
  const add = el("button", "", "Add");
  add.type = "button";
  add.disabled = noGoal;
  add.onclick = () => { it.on = true; planEdit.open = id; drawPlan(); $("#pr-items details[open] input, #pr-items details[open] textarea")?.focus(); };
  row.append(el("span", "who", nameOf(id)), el("span", "what", noGoal ? "no goal yet, so no work to change" : planEdit.kind === "revision" ? "no change" : "not in this plan"), add);
  return row;
}

async function decidePlan(btn, body, working, done) {
  const mgr = planEdit.mgr, draft = planEdit.draft;
  btn.disabled = true;
  status($("#pr-msg"), working);
  try {
    await api("POST", `/api/agents/${encodeURIComponent(mgr)}/plan`, { draft, ...body });
    if (plans.get(mgr)?.id === draft) { plans.delete(mgr); planChanged(mgr); } // a new plan may already be here
    planLoads.set(mgr, (planLoads.get(mgr) ?? 0) + 1); // and a request still in flight must not bring the old one back
    runMsg(done);
    refreshSummary();
  } catch (err) {
    status($("#pr-msg"), err.message, true);
    loadPlan(mgr); // it may have been decided elsewhere, or replaced; edits to the same draft are kept
  }
  btn.disabled = false;
}

$("#pr-approve").onclick = e => {
  const on = [...planEdit.items.values()].filter(i => i.on).map(i => i.f);
  const body = planEdit.kind === "revision" ? { action: "approve", changes: on } : { action: "approve", subgoals: on };
  decidePlan(e.currentTarget, body, "Approving…", `${nameOf(planEdit.mgr)} is running its team.`);
};
$("#pr-replan").onclick = e => {
  decidePlan(e.currentTarget, { action: "replan", feedback: $("#pr-feedback").value.trim() }, "Asking for a new plan…", `${nameOf(planEdit.mgr)} is planning again.`);
};
$("#pr-cancel").onclick = async e => {
  const btn = e.currentTarget;
  if (!await ask(`Cancel ${nameOf(planEdit.mgr)}'s plan?`, "The run stops and no report's goal changes.", "Cancel plan", "Keep it")) return;
  decidePlan(btn, { action: "cancel" }, "Cancelling…", `${nameOf(planEdit.mgr)}'s plan was cancelled.`);
};

/* ---------- logs: one section per run, outcome and reason first ---------- */

const log = $("#log"), logPane = $('[data-pane="logs"]');

// line icons, drawn like the sidebar's (no emoji)
const ICONS = {
  msg: "M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.4A8 8 0 1 1 21 12Z",
  tool: "M4 17l6-5-6-5M12 19h8",
  file: "M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9zM14 3v6h6M9 15l2 2 4-4",
  ok: "M20 6 9 17l-5-5",
  bad: "M18 6 6 18M6 6l12 12",
  error: "M12 9v4M12 17h.01M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z",
  plan: "M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01",
  verdict: "M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6ZM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6ZM18 9a9 9 0 0 1-9 9",
  you: "M20 21a8 8 0 0 0-16 0M12 13a5 5 0 1 0 0-10 5 5 0 0 0 0 10Z",
  flag: "M4 22V4M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1",
};
function icon(name, cls = "") {
  const s = document.createElementNS(SVG, "svg"), p = document.createElementNS(SVG, "path");
  s.setAttribute("viewBox", "0 0 24 24");
  s.setAttribute("class", "li " + cls);
  s.setAttribute("aria-hidden", "true");
  p.setAttribute("d", ICONS[name]);
  s.append(p);
  return s;
}

// logState holds the selected agent's log, and how the user is looking at it.
const logState = { agent: null, events: [], runs: new Map(), open: new Map(), filter: "all", q: "" };

const isTerminalDone = e => e.kind === "done" && /^(all \d+ checks? passed|team work merged)/.test(e.text);
const isJSON = t => /^\s*\{[\s\S]*\}\s*$/.test(t || "");
const attemptMsg = e => e.kind === "msg" && /^attempt (\d+)\/(\d+) with /.exec(e.text || "");
const plural = (n, one, many = one + "s") => `${n} ${n === 1 ? one : many}`;
const names = xs => xs.length <= 3 ? xs.join(", ") : `${xs.slice(0, 3).join(", ")} and ${xs.length - 3} more`;
const clock = ts => { const d = new Date(ts); return d.toDateString() === new Date().toDateString() ? time(ts).slice(0, 5) : d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + time(ts).slice(0, 5); };
const since = (ts, t0) => { const s = Math.max(0, Math.round((ts - t0) / 1000)); return `+${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`; };

// check parses a check event: "✓ cmd" or "✗ cmd (exit status 1)".
function check(e) {
  const ok = e.text.startsWith("✓");
  let cmd = e.text.slice(2);
  if (!ok) { const i = cmd.lastIndexOf(" ("); if (i > 0) cmd = cmd.slice(0, i); }
  return { ok, cmd, out: e.raw || "" };
}
// keyLine picks the line of a failure's output that says what went wrong.
function keyLine(out) {
  const ls = out.split("\n").map(l => l.trim()).filter(Boolean);
  return ls.find(l => /error|fail|panic|undefined|exception|expected|not found|cannot|denied|no such/i.test(l) && !/^ok\b/.test(l)) || ls.at(-1) || "";
}
const fileOf = e => (e.text || "").replace(/^\S+\s+/, "");
const fileVerb = e => ({ write: "Wrote", create_file: "Created", delete: "Deleted" })[(e.text || "").split(" ")[0].toLowerCase()] || "Edited";

// sessionsOf splits the events into runs: one per press of Run (or a manager starting this agent).
// Events from before runs were recorded are split at requests, results and long pauses.
function sessionsOf(events) {
  const out = [];
  let cur = null;
  const attemptOfRun = new Map();
  for (const e of events) {
    const m = attemptMsg(e);
    if (m) attemptOfRun.set(e.run, +m[1]);
    const legacy = !e.session;
    const fresh = !cur || (legacy
      ? !cur.legacy || e.kind === "request" || cur.ended || e.ts - cur.last > 10 * 60e3
      : cur.key !== e.session);
    if (fresh) out.push(cur = { key: legacy ? "L" + e.id : e.session, legacy, events: [], ended: false, first: e.ts });
    e.att = e.attempt || attemptOfRun.get(e.run) || 0;
    cur.events.push(e);
    cur.last = e.ts;
    if (isTerminalDone(e) || e.kind === "error" && /^(stopped by user|you cancelled the plan|needs you|checks still failing|merged work)/.test(e.text)) cur.ended = true;
  }
  return out;
}

// outcome says how a run ended, in one word and one sentence, with the reason for a failure.
function outcome(g, latest) {
  const st = latest ? summary.agents[logState.agent]?.status : null;
  if (st === "awaiting") return { state: "awaiting", word: "Waiting for you", line: "The plan is ready. Approve it in the Goal tab." };
  const attempts = Math.max(0, ...g.events.map(e => e.att));
  const manager = kidsOf(logState.agent).length > 0;
  if (latest && BUSY.includes(st)) return { state: "running", word: "Working", line: attempts && !manager ? `Working on attempt ${attempts} of 3.` : `${nameOf(logState.agent)} ${WORKING[st] ?? "is working"}.` };
  for (let i = g.events.length - 1; i >= 0; i--) {
    const e = g.events[i];
    if (isTerminalDone(e)) {
      const n = /all (\d+)/.exec(e.text)?.[1];
      const checks = +n === 1 ? "its check" : `all ${n} checks`;
      return { by: e, state: "done", word: "Done", line: e.text.startsWith("team")
        ? `Done. The team's work is merged and ${checks} passed.`
        : `Done${attempts > 1 ? ` on attempt ${attempts}` : ""}. ${checks[0].toUpperCase() + checks.slice(1)} passed.` };
    }
    if (e.kind !== "error") continue;
    if (/^stopped by user/.test(e.text)) return { by: e, state: "stopped", word: "Stopped", line: "You stopped it." };
    if (/^you cancelled the plan/.test(e.text)) return { by: e, state: "stopped", word: "Cancelled", line: "You cancelled the plan." };
    if (/^the plan wasn't approved/.test(e.text)) return { by: e, state: "stopped", word: "Stopped", line: "The plan wasn't approved in time." };
    if (/^needs you: /.test(e.text)) return { by: e, state: "blocked", word: "Needs you", line: e.text.replace(/^needs you: /, "").split("\n")[0].replace(/^./, c => c.toUpperCase()) + "." };
    return { by: e, state: "failed", word: "Failed", line: failureLine(g, attempts, e) };
  }
  if (g.events.some(e => e.kind === "merge")) return { state: "other", word: "Merged", line: "" };
  if (g.events.some(e => /^synced with/.test(e.text))) return { state: "other", word: "Synced", line: "" };
  return { state: "stopped", word: "Ended", line: "It ended without a result." };
}
// failureLine puts the reason first: the check that still fails and its telling line.
function failureLine(g, attempts, err) {
  const last = lastChecks(g).filter(c => !c.ok);
  const head = `Failed${attempts > 1 ? ` after ${attempts} attempts` : ""}`;
  if (last.length) return [`${head}: `, el("code", "", last[0].cmd), last.length > 1 ? ` and ${plural(last.length - 1, "other check")} still fail.` : " still fails."];
  return `${head}: ${err.text.split("\n")[0]}`;
}
// lastChecks are the checks of the run's last round (worker attempt or manager verify).
function lastChecks(g) {
  const cs = g.events.filter(e => e.kind === "check");
  if (!cs.length) return [];
  const lastAtt = cs.at(-1).att, round = [];
  for (let i = cs.length - 1; i >= 0 && cs[i].att === lastAtt; i--) {
    round.unshift(check(cs[i]));
    if (i > 0 && cs[i].id - cs[i - 1].id > 1) break; // an earlier round of the same attempt number
  }
  return round;
}
// activity says what the run did, in one line.
function activity(g) {
  const ev = g.events, parts = [];
  const plans = ev.filter(e => e.kind === "plan" && !/^plan ready/.test(e.text));
  const verdicts = ev.filter(e => e.kind === "verdict");
  if (plans.length) parts.push(`Gave work to ${names([...new Set(plans.map(e => e.text.split(" → ")[0]))])}`);
  const acc = verdicts.filter(e => e.text.startsWith("✓")).map(e => e.text.replace(/^✓ accepted and merged |^✓ /, ""));
  const rej = verdicts.filter(e => e.text.startsWith("✗")).map(e => e.text.replace(/^✗ rejected /, "").split(":")[0]);
  if (acc.length) parts.push(`merged ${names(acc)}`);
  if (rej.length) parts.push(`sent back ${names(rej)}`);
  const fixes = ev.filter(e => e.kind === "warn" && /fails my checks/.test(e.text)).length;
  if (fixes) parts.push(`${plural(fixes, "fix round")}`);
  const files = [...new Set(ev.filter(e => e.kind === "file").map(fileOf))];
  const cmds = ev.filter(e => e.kind === "tool" && /^bash /i.test(e.text)).length;
  const reads = ev.filter(e => e.kind === "tool" && !/^bash /i.test(e.text)).length;
  if (files.length) parts.push(`changed ${names(files)}`);
  if (cmds) parts.push(`ran ${plural(cmds, "command")}`);
  if (reads) parts.push(`looked at the code ${plural(reads, "time")}`);
  const s = parts.join(", ");
  return s ? s[0].toUpperCase() + s.slice(1) + "." : "";
}

// category is what a filter chip calls an event.
function category(e) {
  switch (e.kind) {
    case "tool": return "commands";
    case "file": return "files";
    case "check": return "checks";
    case "error": case "warn": case "stderr": return "errors";
    default: return "messages";
  }
}
// hidden events carry no information for a reader unless they ask for everything.
const hidden = e => e.kind === "stderr" || e.kind === "usage" || e.kind === "done" && e.text === "finished" ||
  e.kind === "msg" && (isJSON(e.text) || /^checkpoint [0-9a-f]+$/.test(e.text)) || !!attemptMsg(e);

function matches(e) {
  if (logState.filter !== "all" && category(e) !== logState.filter) return false;
  return !logState.q || (e.text || "").toLowerCase().includes(logState.q);
}

// timeline turns a run's events into readable rows: commands and checks in a row fold together,
// attempts get a divider that says why they happened, noise is left out.
function timeline(g, all, decided) {
  const rows = [], t0 = g.first, filtered = logState.filter !== "all" || logState.q;
  let att = 0, prevChecks = [], i = 0;
  // the event that decided the outcome is already the section's first line
  const ev = g.events.filter(e => (all || !hidden(e) && e !== decided) && matches(e));
  while (i < ev.length) {
    const e = ev[i];
    if (!filtered && e.att > 1 && e.att !== att && g.events.some(x => x.att === e.att - 1)) {
      const failed = prevChecks.filter(c => !c.ok).map(c => c.cmd);
      const d = el("li", "ldiv");
      d.append(el("b", "", `Attempt ${e.att} of 3`), failed.length ? el("span", "", `because ${failed.length === 1 ? "a check" : plural(failed.length, "check")} failed: ${names(failed)}`) : "");
      rows.push(d);
    }
    att = e.att || att;
    // a run of the same kind folds into one row
    let j = i;
    while (j + 1 < ev.length && ["tool", "check"].includes(e.kind) && ev[j + 1].kind === e.kind && ev[j + 1].att === e.att) j++;
    const group = ev.slice(i, j + 1);
    if (e.kind === "check") prevChecks = group.map(check);
    rows.push(row(group, t0));
    i = j + 1;
  }
  return rows;
}

// toolText is a tool call as a reader wants it: the command itself for the shell, else "Read file".
const toolText = e => /^bash /i.test(e.text) ? e.text.replace(/^bash /i, "") : e.text;

// inline renders the little markdown agents write: `code`, **bold**, and headings as plain lines.
function inline(text) {
  const out = [];
  for (const part of text.replace(/^#{1,6} /gm, "").split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/)) {
    if (/^`[^`]+`$/.test(part)) out.push(el("code", "", part.slice(1, -1)));
    else if (/^\*\*[^*]+\*\*$/.test(part)) out.push(el("b", "", part.slice(2, -2)));
    else if (part) out.push(part);
  }
  return out;
}

function row(group, t0) {
  const e = group[0], li = el("li", "lrow"), x = el("div", "lx");
  let k = e.kind, ic = "msg", cls = "";
  const t = el("span", "lt", since(e.ts, t0));
  t.title = new Date(e.ts).toLocaleString();
  if (e.kind === "tool") {
    ic = "tool";
    if (group.length === 1) { x.append(el("code", "", toolText(e))); x.className = "lx one"; x.title = toolText(e); x.onclick = () => x.classList.toggle("one"); }
    else {
      const d = el("details"), ul = el("ul");
      group.forEach(g => ul.append(el("li", "", toolText(g))));
      const cmds = group.filter(g => /^bash /i.test(g.text)).length;
      d.append(el("summary", "", cmds === group.length ? `Ran ${group.length} commands` : `Used tools ${group.length} times`), ul);
      x.append(d);
    }
  } else if (e.kind === "check") {
    const cs = group.map(check), ok = cs.filter(c => c.ok).length;
    ic = ok === cs.length ? "ok" : "bad"; cls = ic;
    const d = el("details"), ul = el("ul");
    cs.forEach(c => ul.append(el("li", "", `${c.ok ? "passed" : "failed"}  ${c.cmd}`)));
    d.append(el("summary", "", `Checks: ${ok} of ${cs.length} passed`), ul);
    x.append(d);
  } else if (e.kind === "file") {
    ic = "file"; x.append(`${fileVerb(e)} `, el("code", "", fileOf(e)));
  } else if (e.kind === "plan") {
    ic = "plan"; cls = "mgr"; x.textContent = e.text.replace(/ \(checks: .*\)$/, "");
    const checks = / \(checks: (.*)\)$/.exec(e.text)?.[1];
    if (checks) x.title = "checks: " + checks;
  } else if (e.kind === "verdict") {
    ic = e.text.startsWith("✗") ? "bad" : "verdict"; cls = e.text.startsWith("✗") ? "bad" : "mgr";
    x.textContent = e.text.replace(/^[✓✗] /, "").replace(/^./, c => c.toUpperCase());
  } else if (e.kind === "request") {
    ic = "you"; cls = "you"; x.textContent = e.text.replace(/^you /, "You ");
  } else if (e.kind === "error" || e.kind === "warn" || e.kind === "stderr") {
    ic = "error"; cls = e.kind === "warn" ? "warn" : "bad"; x.textContent = e.text;
  } else if (e.kind === "done") {
    if (isTerminalDone(e)) { ic = "flag"; cls = "ok"; k = "other"; x.textContent = e.text.replace(/^./, c => c.toUpperCase()); }
    else { k = "answer"; x.append(...inline(e.text)); x.className = "lx clamp"; x.onclick = () => x.classList.toggle("clamp"); }
  } else if (e.kind === "start") {
    ic = "flag"; k = "other"; x.append("Started", e.text ? ": " : "", e.text || "");
  } else if (e.kind === "merge") {
    ic = "verdict"; cls = "ok"; k = "other"; x.textContent = e.text.replace(/^./, c => c.toUpperCase());
  } else {
    x.append(...inline(e.text || ""));
    if ((e.text || "").length > 280 || (e.text || "").split("\n").length > 4) { x.className = "lx clamp"; x.onclick = () => x.classList.toggle("clamp"); }
  }
  if ($("#raw").checked && group.some(g => g.raw)) {
    const d = el("details"); d.append(el("summary", "", "raw output"), el("pre", "", group.map(g => g.raw).filter(Boolean).join("\n\n")));
    x.append(d);
  }
  li.dataset.k = k;
  li.append(t, icon(ic, cls), x);
  return li;
}

// checklist shows the last round of checks, with a failure's telling line and output right there.
function checklist(g) {
  const cs = lastChecks(g);
  if (!cs.length) return null;
  const ul = el("ul", "lchk");
  for (const c of cs) {
    const li = el("li");
    li.append(icon(c.ok ? "ok" : "bad", c.ok ? "ok" : "bad"), el("code", "", c.cmd));
    if (!c.ok && c.out) {
      li.append(el("div", "lkey", keyLine(c.out)));
      const d = el("details"); d.append(el("summary", "", "Full output"), el("pre", "", c.out));
      li.append(d);
    }
    ul.append(li);
  }
  return ul;
}

function section(g, latest, all, first) {
  const d = el("details", "lrun"), o = outcome(g, latest), sum = el("summary");
  d.dataset.state = o.state;
  d.open = logState.open.has(g.key) ? logState.open.get(g.key) : first; // the newest run that did something starts open
  d.ontoggle = () => logState.open.set(g.key, d.open);
  const runs = new Set(g.events.map(e => e.run).filter(Boolean));
  let tokens = [...runs].reduce((n, r) => n + (logState.runs.get(r)?.tokens || 0), 0);
  if (latest && BUSY.includes(summary.agents[logState.agent]?.status)) tokens = Math.max(tokens, summary.agents[logState.agent]?.tokens || 0);
  const attempts = Math.max(0, ...g.events.map(e => e.att));
  const meta = [fmtDur((o.state === "running" || o.state === "awaiting" ? Date.now() : g.last) - g.first)];
  if (attempts > 1) meta.push(plural(attempts, "attempt"));
  if (tokens) meta.push(fmtTok(tokens) + " tokens");
  const label = o.state === "other" ? (g.events.some(e => e.kind === "merge") ? "Merge at " : "Sync at ") : "Run at ";
  sum.append(el("span", "lstate", o.word), el("span", "lwhen", label + clock(g.first)), el("span", "lmeta", meta.join(" · ")));
  const body = el("div", "lbody");
  const filtered = logState.filter !== "all" || logState.q;
  if (!filtered) {
    const ask = g.events.find(e => e.kind === "request");
    if (ask) {
      const p = el("p", "lask"), newPlan = /^you asked for a new plan/.test(ask.text);
      p.append(el("b", "", newPlan ? "You asked for a new plan: " : "You asked: "),
        ask.text.replace(/^you asked for (changes: |a new plan( and said: )?)/, "") || "(no note)");
      body.append(p);
    }
    const out = el("p", "lout");
    Array.isArray(o.line) ? out.append(...o.line) : out.append(o.line);
    if (o.line) body.append(out);
    const act = activity(g);
    if (act) body.append(el("p", "lact", act));
    const cl = checklist(g);
    if (cl) body.append(cl);
  }
  const rows = timeline(g, all, o.by);
  if (rows.length) { const ol = el("ol", "ltime"); ol.append(...rows); body.append(ol); }
  d.append(sum, body);
  return { d, count: rows.length };
}

function stoppedRuns(gs) {
  const d = el("details", "lrun"), sum = el("summary");
  d.dataset.state = "stopped";
  sum.append(el("span", "lstate", "Stopped"), el("span", "lwhen", `${gs.length} runs stopped before they did anything`),
    el("span", "lmeta", `${clock(gs.at(-1).first)} to ${clock(gs[0].first)}`));
  const body = el("div", "lbody"), ul = el("ul", "lchk");
  gs.forEach(g => { const li = el("li"); li.append(icon("error"), el("span", "", "Run at " + clock(g.first))); ul.append(li); });
  body.append(ul);
  d.append(sum, body);
  return d;
}

function renderFilters(events, all) {
  const counts = { all: 0, messages: 0, commands: 0, files: 0, checks: 0, errors: 0 };
  for (const e of events) if (all || !hidden(e)) { counts.all++; counts[category(e)]++; }
  const labels = { all: "All", messages: "Messages", commands: "Commands", files: "Files", checks: "Checks", errors: "Errors" };
  $("#log-filters").replaceChildren(...Object.keys(labels).filter(k => k === "all" || counts[k]).map(k => {
    const b = el("button", logState.filter === k ? "on" : "");
    b.type = "button";
    b.append(labels[k], el("b", "", counts[k]));
    b.onclick = () => { logState.filter = k; renderLogs(); };
    return b;
  }));
}

function renderLogs() {
  if (logState.agent !== selected) return;
  const all = $("#raw").checked, top = logPane.scrollTop;
  renderFilters(logState.events, all);
  const groups = sessionsOf(logState.events).reverse();
  const filtered = logState.filter !== "all" || logState.q;
  const st = summary.agents[logState.agent];
  if (st && (BUSY.includes(st.status) || st.status === "awaiting") && st.since && !(groups[0] && groups[0].first >= st.since - 2000 && !groups[0].legacy))
    groups.unshift({ key: "now", events: [], first: st.since, last: Date.now() }); // it started, and hasn't logged anything yet
  const secs = [];
  for (let i = 0; i < groups.length; i++) {
    // runs stopped before they did anything fold into one line
    const idle = g => g.events.length <= 3 && g.events.every(e => e.kind === "request" || e.kind === "start" || e.kind === "error" && /^stopped by user/.test(e.text));
    let j = i;
    while (!filtered && j + 1 < groups.length && idle(groups[i]) && idle(groups[j + 1]) && !(i === 0 && BUSY.includes(summary.agents[logState.agent]?.status))) j++;
    if (j > i) { secs.push({ d: stoppedRuns(groups.slice(i, j + 1)), count: 1 }); i = j; continue; }
    secs.push(section(groups[i], i === 0, all, !secs.some(s => s.full)));
    secs.at(-1).full = true;
  }
  if (filtered) secs.splice(0, secs.length, ...secs.filter(s => s.count));
  if (!secs.length) log.replaceChildren(el("div", "empty", logState.events.length ? "Nothing matches." : "No activity yet. Set a goal and press Run."));
  else log.replaceChildren(...secs.map(s => s.d));
  logPane.scrollTop = top;
  renderLogNow();
}

// renderLogNow pins what the agent is doing right now above its log.
function renderLogNow() {
  const st = selected && summary.agents[selected], box = $("#log-now"), busy = st && (BUSY.includes(st.status) || st.status === "awaiting");
  box.hidden = !busy;
  if (!busy) return;
  box.style.setProperty("--s", `var(--${st.status === "awaiting" ? "awaiting" : ["planning", "waiting", "reviewing", "fixing"].includes(st.status) ? "manager" : st.status === "verifying" ? "verifying" : "running"})`);
  $("#log-now-text").textContent = (st.status === "awaiting" ? "Waiting for you to approve the plan" : `${nameOf(selected)} ${WORKING[st.status] ?? st.status}`) +
    (st.since ? ` · ${fmtDur(Date.now() - st.since)}` : "") + (st.now ? ` · now: ${st.now}` : "");
}

async function loadLogs() {
  if (!selected) return;
  const id = selected;
  const [es, rs] = await Promise.all([
    api("GET", `/api/agents/${id}/events`).catch(e => (say(e.message, true), [])),
    api("GET", `/api/agents/${id}/runs`).catch(() => []),
  ]);
  if (id !== selected) return;
  if (logState.agent !== id) Object.assign(logState, { open: new Map(), filter: "all", q: "" }), $("#log-search").value = "";
  Object.assign(logState, { agent: id, events: es, runs: new Map(rs.map(r => [r.id, r])) });
  renderLogs();
}

$("#raw").onchange = renderLogs;
$("#log-search").oninput = e => { logState.q = e.target.value.trim().toLowerCase(); renderLogs(); };

// live events join the log; the view re-renders at most a few times a second
let logTimer = 0;
function appendLive(e) {
  if (logState.agent !== e.agent) return;
  logState.events.push(e);
  if (logState.events.length > MAX_LOG_ROWS) logState.events.splice(0, logState.events.length - MAX_LOG_ROWS);
  logTimer ||= setTimeout(() => { logTimer = 0; renderLogs(); }, 250);
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

let shownDiff = ""; // what the Diff tab shows now, so live refreshes skip re-rendering when nothing changed

async function loadDiff() {
  if (!selected) return false;
  const out = $("#diff"), id = selected;
  let d, cps;
  try {
    [d, cps] = await Promise.all([api("GET", `/api/agents/${id}/diff`), api("GET", `/api/agents/${id}/checkpoints`)]);
  } catch (e) { diffMsg(e.message, true); out.replaceChildren(); shownDiff = ""; return false; }
  if (id !== selected) return false; // another agent was picked while this loaded
  const live = BUSY.includes(summary.agents[id]?.status);
  const key = id + JSON.stringify([live, d, cps]);
  if (key === shownDiff) return true;
  shownDiff = key;
  const pane = $('[data-pane="diff"]'), top = pane.scrollTop;
  const closed = new Set($$("#diff .file:not([open])").map(f => f.dataset.path)); // keep files the user folded
  const files = d.files;
  let adds = 0, dels = 0;
  const nodes = files.map(f => {
    adds += f.adds; dels += f.dels;
    const det = el("details", "file");
    det.dataset.path = f.path;
    det.open = !closed.has(f.path);
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
  $("#diff-stat").classList.toggle("live", live);
  out.replaceChildren(...(nodes.length ? nodes : [el("div", "empty", live ? "No changes yet. They'll show here as the agent makes them." : "No changes yet.")]));
  pane.scrollTop = top;
  $("#diff-actions").hidden = !files.length;
  const notes = [];
  if (d.behind) notes.push(el("p", "note", `${d.from} has ${d.behind} new commit${d.behind > 1 ? "s" : ""} this agent doesn't have yet. They're merged in when it runs, or press Sync to merge them now.`));
  if (d.mergedInto) notes.push(el("p", "note",
    `This work is already merged into ${d.mergedInto}. Removing a change here won't remove it from ${d.mergedInto}; open ${d.mergedInto} and remove it there, or re-run ${d.mergedInto}.`));
  $("#diff-note").replaceChildren(...notes);
  const sync = $("#diff-sync");
  sync.textContent = d.behind ? `Sync with ${d.from} (${d.behind})` : `Up to date with ${d.from}`;
  sync.disabled = !d.behind;
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
  try { const r = await call(), msg = typeof done === "function" ? done(r) : done; diffMsg(msg); say(msg); }
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
  const n = k => `${k} change${k > 1 ? "s" : ""}`;
  diffAction(e.currentTarget, "Promoting…", () => api("POST", `/api/agents/${selected}/hunks`, { action: "promote", ids }),
    r => !r.applied ? `${to} already has ${r.present > 1 ? "these changes" : "this change"}; nothing to copy.`
      : `Copied ${n(r.applied)} into ${to}'s work` + (r.present ? `; ${r.present} ${r.present > 1 ? "were" : "was"} already there.` : ".") +
        ` They stay here too; open ${to} to see them.`);
};
$("#diff-sync").onclick = e => diffAction(e.currentTarget, "Syncing…", () => api("POST", `/api/agents/${selected}/sync`),
  r => r.commits ? `Merged ${r.commits} new commit${r.commits > 1 ? "s" : ""} from ${r.from}. The diff still shows only this agent's changes.` : `Already up to date with ${r.from}.`);
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
  const something = (p.commits && p.files) || p.uncommitted > 0;
  lines.push(el("span", "", p.commits && p.files
    ? `${p.commits} commit${p.commits > 1 ? "s" : ""} · ${p.files} file${p.files !== 1 ? "s" : ""} · +${p.adds} −${p.dels}`
    : something ? "No checkpoints to merge yet." : `Nothing to merge: ${p.target} already has all of this work.`));
  if (p.uncommitted) lines.push(el("p", "note", `Plus ${p.uncommitted} file${p.uncommitted > 1 ? "s" : ""} changed since the last checkpoint, not counted above. Merging saves ${p.uncommitted > 1 ? "them" : "it"} as a checkpoint first.`));
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
  $("#m-go").disabled = !something || p.dirty || p.conflicts.length > 0;
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
    const draft = plans.get(parentOf(a.id)), item = draft && planItems(draft).find(x => x.agent === a.id);
    card.classList.toggle("proposed", !!item);
    card.classList.toggle("left-out", !!draft && !item);
    if (item) card.querySelector(".now").textContent = draft.kind === "revision" ? "proposed change: " + item.change : "proposed: " + item.title;
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
      tok.title = "tokens this run" + (lc === "over" ? " · over the hard limit" : lc === "warn" ? " · over the soft limit" : "");
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
  const needs = all.filter(([, s]) => NEEDS.includes(s.status) || s.status === "awaiting");
  syncPlans();
  const running = all.filter(([, s]) => BUSY.includes(s.status)).length;
  $("#n-running").textContent = running;
  $("#running").hidden = running === 0;
  $("#tokens").textContent = fmtTok(all.reduce((n, [, s]) => n + (s.allTokens || 0), 0));
  $("#n-needs").textContent = needs.length;
  $("#needs").dataset.n = needs.length;
  $("#needs").dataset.kind = needs.every(([, s]) => s.status === "awaiting") ? "plan" : "fail"; // amber for plans, red for failures
  if (!needs.length) $("#needs").open = false;
  $("#needs ul").replaceChildren(...needs.map(([id, s]) => {
    const li = el("li", "", s.status === "awaiting" ? `⏸ ${nameOf(id)}: plan waiting for your approval` : `✗ ${nameOf(id)}: ${s.status} · ${s.title}`);
    li.onclick = () => { $("#needs").open = false; if (s.status === "awaiting") tab = "goal"; select(id); };
    return li;
  }));
  if (selected && summary.agents[selected]) {
    const g = (await api("GET", `/api/agents/${selected}`).catch(() => null))?.goal;
    if (g) showGoalState(g);
  }
}

// setStatus shows a status change on the canvas right away, before the summary refetch.
function setStatus(id, s) {
  const st = summary.agents[id] ??= {};
  if (st.status === s) return;
  st.status = s;
  if (BUSY.includes(s) && !st.since) st.since = Date.now();
  refreshCards();
}

let summaryTimer = 0;
// a throttle, not a debounce: busy agents send usage updates nonstop, which would keep postponing a debounced refresh
const scheduleSummary = () => { summaryTimer ||= setTimeout(() => { summaryTimer = 0; refreshSummary(); }, 150); };

// tick the "working for" timers without refetching anything
// liveDiff keeps the Diff tab current while its agent works: on the agent's own activity, and every
// few seconds for agents that don't report file edits. A hunk selection in progress is never wiped.
let liveDiffTimer = 0;
function scheduleLiveDiff() {
  liveDiffTimer ||= setTimeout(() => {
    liveDiffTimer = 0;
    if (tab === "diff" && selected && !selectedHunks().length) loadDiff();
  }, 1000);
}
setInterval(() => { if (tab === "diff" && BUSY.includes(summary.agents[selected]?.status)) scheduleLiveDiff(); }, 3000);

setInterval(() => {
  for (const t of $$("#canvas .elapsed")) t.textContent = fmtDur(Date.now() - Number(t.dataset.since));
  if (selected && BUSY.includes(summary.agents[selected]?.status)) renderStats();
}, 1000);

/* ---------- live updates ---------- */

const es = new EventSource("/api/events?project=" + encodeURIComponent(PID));
es.onmessage = m => {
  const msg = JSON.parse(m.data);
  if (msg.type === "hello" || msg.type === "resync") {
    // a (re)connect or missed updates: whatever changed meanwhile never arrived, so reload the state
    if (msg.type === "hello") $("#stale").hidden = msg.build === String(window.BUILD); // this page came from an older run of the server
    refreshSummary();
    if (selected && tab === "logs") loadLogs();
  } else if (msg.type === "event") {
    const e = msg.event;
    if (e.kind !== "stderr") {
      const now = cardOf(e.agent)?.querySelector(".now");
      if (now) now.textContent = `${e.kind === "start" ? "started" : e.kind}: ${e.text}`;
    }
    if (e.agent === selected && tab === "logs") appendLive(e);
    if (e.agent === selected && e.kind !== "stderr") scheduleLiveDiff(); // it edited files, ran a tool, checkpointed...
  } else if (msg.type === "plan") {
    loadPlan(msg.agent);
  } else if (msg.type === "status" || msg.type === "usage") {
    if (msg.type === "status") setStatus(msg.agent, msg.status);
    if (msg.type === "status" && msg.agent === selected && tab === "logs") loadLogs(); // its outcome and totals changed
    scheduleSummary();
    if (msg.type === "status" && msg.agent === selected && tab === "diff" && !selectedHunks().length) loadDiff(); // don't wipe a selection in progress
  }
};
$("#reload").onclick = () => location.reload();
// a backstop for anything a live update didn't cover
setInterval(() => { if (!document.hidden) refreshSummary(); }, 15000);

/* ---------- start ---------- */

renderTypes();
if ([...agents.values()].some(a => a.x == null || a.y == null)) layout(false);
agents.forEach(makeCard);
refreshCards();
refreshSummary();
document.fonts?.ready.then(drawLinks); // box heights settle once fonts load
