// Arranger UI: tree editing, inspector (goal / logs / diff / settings), live status over SSE.
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const PID = document.body.dataset.project;
const roots = $("#roots"), main = $("main");
const MAX_LOG_ROWS = 1000;
let dirty = false, selected = null, tab = "goal";

function say(text, err = false) {
  const m = $("#msg");
  m.textContent = m.title = text;
  m.className = err ? "err" : "";
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

const cardOf = id => $(`li[data-id="${CSS.escape(id)}"] > .card`, roots);

/* ---------- tree editing ---------- */

function markDirty() { dirty = true; say("unsaved changes"); }

function newNode(role, name) {
  const li = $("#node-tpl").content.firstElementChild.cloneNode(true);
  li.dataset.id = role + "-" + Date.now().toString(36) + Math.random().toString(36).slice(2, 5);
  const card = li.querySelector(".card");
  card.dataset.role = role;
  card.querySelector(".name").textContent = name;
  card.querySelector(".role").textContent = role;
  card.querySelector(".rt").textContent = "claude";
  return li;
}

let dragged = null; // an existing <li>, or a palette card
document.addEventListener("dragstart", e => {
  const card = e.target.closest?.(".card");
  if (!card) return;
  dragged = card.hasAttribute("data-new") ? card : card.parentElement;
  e.dataTransfer.effectAllowed = "move";
});
document.addEventListener("dragend", () => { dragged = null; $$(".drop").forEach(x => x.classList.remove("drop")); });
document.addEventListener("dragover", e => {
  if (!dragged) return;
  const card = e.target.closest("#roots .card");
  $$(".drop").forEach(x => x.classList.remove("drop"));
  if (card) card.classList.add("drop"); else if (e.target.closest("main")) main.classList.add("drop"); else return;
  e.preventDefault();
});
document.addEventListener("drop", e => {
  e.preventDefault();
  if (!dragged) return;
  const card = e.target.closest("#roots .card");
  const ul = card ? card.parentElement.querySelector(":scope > ul") : roots;
  const li = dragged.hasAttribute("data-new") ? newNode(dragged.dataset.role, dragged.querySelector(".name").textContent) : dragged;
  if (li.contains(ul)) return; // can't nest an agent under its own descendant
  ul.append(li);
  markDirty();
  refreshCards();
});

roots.addEventListener("click", e => {
  const card = e.target.closest(".card");
  if (!card) return;
  const li = card.parentElement;
  if (e.target.classList.contains("x")) {
    li.parentElement.append(...li.querySelector(":scope > ul").children); // children move up a level
    if (selected === li.dataset.id) closeInspector();
    li.remove();
    markDirty();
    return;
  }
  select(li.dataset.id);
});
roots.addEventListener("dblclick", e => {
  const name = e.target.closest(".card")?.querySelector(".name");
  const v = name && prompt("Agent name", name.textContent);
  if (v?.trim()) { name.textContent = v.trim(); markDirty(); }
});

async function save() {
  const agents = $$("li", roots).map(li => {
    const card = li.querySelector(".card");
    return { id: li.dataset.id, name: card.querySelector(".name").textContent, role: card.dataset.role,
             runtime: card.querySelector(".rt").textContent, parent: li.parentElement.closest("li")?.dataset.id ?? "" };
  });
  await api("POST", `/api/projects/${PID}/arrangement`, agents);
  dirty = false;
  say(`saved ${agents.length} agents`);
  refreshSummary();
}
$("#save").onclick = () => save().catch(e => say(e.message, true));
addEventListener("beforeunload", e => { if (dirty) e.preventDefault(); });

/* ---------- header: projects, theme, run all ---------- */

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

/* ---------- inspector ---------- */

const insp = $("#inspector"), gform = $("#goal-form"), aform = $("#agent-form");

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
  showGoalState(g);
  showTab(tab);
}

function showGoalState(g) {
  $("#i-status").dataset.status = g.status;
  $("#g-state").textContent = g.title ? `${g.status}` + (g.attempts ? ` · attempt ${g.attempts}/3` : "") + (g.total ? ` · checks ${g.passed}/${g.total}` : "") : "";
  $("#g-feedback").hidden = !g.feedback;
  $("#g-feedback .feedback").textContent = g.feedback;
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
  if (name === "diff") loadDiff();
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
  const a = { name: aform.elements.name.value, runtime: aform.elements.runtime.value, model: aform.elements.model.value, args: aform.elements.args.value, prompt: aform.elements.prompt.value };
  try {
    await api("PUT", `/api/agents/${selected}`, a);
    const card = cardOf(selected);
    card.querySelector(".name").textContent = $("#i-name").textContent = a.name;
    card.querySelector(".rt").textContent = a.runtime;
    say("settings saved");
  } catch (err) { say(err.message, true); }
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
$("#i-stop").onclick = () => api("POST", `/api/agents/${selected}/stop`).catch(e => say(e.message, true));

/* ---------- logs ---------- */

const log = $("#log"), logPane = $('[data-pane="logs"]');
const time = ts => new Date(ts).toLocaleTimeString([], { hour12: false });

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

/* ---------- diff ---------- */

async function loadDiff() {
  if (!selected) return;
  const out = $("#diff");
  let files;
  try { files = await api("GET", `/api/agents/${selected}/diff`); }
  catch (e) { out.replaceChildren(el("div", "empty bad", e.message)); return; }
  let adds = 0, dels = 0;
  const nodes = files.map(f => {
    adds += f.adds; dels += f.dels;
    const d = el("details", "file");
    d.open = true;
    const sum = el("summary");
    sum.append(el("span", "", f.path), el("span", "ok", "+" + f.adds), el("span", "bad", "−" + f.dels));
    d.append(sum);
    for (const h of f.hunks) {
      const hk = el("div", "hunk");
      hk.append(el("div", "hh", h.header));
      for (const l of h.lines ?? []) hk.append(el("div", "l" + (l[0] === "+" ? " a" : l[0] === "-" ? " d" : ""), l));
      d.append(hk);
    }
    return d;
  });
  $("#diff-stat").textContent = files.length ? `${files.length} file${files.length > 1 ? "s" : ""} · +${adds} −${dels}` : "";
  out.replaceChildren(...(nodes.length ? nodes : [el("div", "empty", "No changes yet.")]));
}
$("#diff-refresh").onclick = loadDiff;

/* ---------- status: cards + top bar ---------- */

let summary = { agents: {}, cost: 0 };

function refreshCards() {
  for (const li of $$("li", roots)) {
    const card = li.querySelector(".card"), st = summary.agents[li.dataset.id];
    card.dataset.status = st?.status ?? "idle";
    card.querySelector(".now").textContent = st?.now || (st?.title ? "goal: " + st.title : "no goal yet");
    const b = card.querySelector(".badges");
    b.replaceChildren();
    if (st?.total) b.append(el("span", st.passed === st.total ? "ok" : "bad", `✓ ${st.passed}/${st.total}`));
    if (st?.adds || st?.dels) b.append(el("span", "", `+${st.adds} −${st.dels}`));
    if (st?.attempts > 1) b.append(el("span", "", `try ${st.attempts}/3`));
  }
}

async function refreshSummary() {
  try { summary = await api("GET", `/api/projects/${PID}/summary`); } catch { return; }
  refreshCards();
  const all = Object.entries(summary.agents);
  const goals = all.filter(([, s]) => s.title);
  const done = goals.filter(([, s]) => s.status === "done").length;
  const needs = all.filter(([, s]) => s.status === "failed");
  $("#n-done").textContent = done;
  $("#n-goals").textContent = goals.length;
  $("#n-running").textContent = all.filter(([, s]) => s.status === "running" || s.status === "verifying").length;
  $("#bar .prog i").style.width = goals.length ? (100 * done / goals.length) + "%" : 0;
  $("#cost").textContent = summary.cost.toFixed(2);
  $("#n-needs").textContent = needs.length;
  $("#needs").dataset.n = needs.length;
  $("#needs ul").replaceChildren(...needs.map(([id, s]) => {
    const li = el("li", "", `✗ ${cardOf(id)?.querySelector(".name").textContent ?? id}: ${s.title}`);
    li.onclick = () => { $("#needs").open = false; select(id); };
    return li;
  }));
  if (selected && summary.agents[selected]) {
    const g = (await api("GET", `/api/agents/${selected}`).catch(() => null))?.goal;
    if (g) showGoalState(g);
  }
}

let summaryTimer = 0;
const scheduleSummary = () => { clearTimeout(summaryTimer); summaryTimer = setTimeout(refreshSummary, 150); };

/* ---------- live updates ---------- */

const es = new EventSource("/api/events?project=" + encodeURIComponent(PID));
es.onmessage = m => {
  const msg = JSON.parse(m.data);
  if (msg.type === "event") {
    const e = msg.event;
    if (e.kind !== "stderr") {
      const now = cardOf(e.agent)?.querySelector(".now");
      if (now) now.textContent = `${e.kind}: ${e.text}`;
    }
    if (e.agent === selected && tab === "logs") appendLive(e);
  } else if (msg.type === "status") {
    scheduleSummary();
    if (msg.agent === selected && tab === "diff") loadDiff();
  }
};

refreshSummary();
