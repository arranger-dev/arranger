// Arranger UI: tree editing, inspector (goal / logs / diff / settings), merging, live status over SSE.
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const PID = document.body.dataset.project;
const roots = $("#roots"), main = $("main");
const MAX_LOG_ROWS = 1000;
const BUSY = ["running", "verifying", "planning", "waiting", "reviewing"];
const NEEDS = ["failed", "blocked"];
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

const cardOf = id => $(`li[data-id="${CSS.escape(id)}"] > .card`, roots);
const nameOf = id => cardOf(id)?.querySelector(".name").textContent ?? id;
const parentOf = id => cardOf(id)?.parentElement.parentElement.closest("li")?.dataset.id;
const time = ts => new Date(ts).toLocaleTimeString([], { hour12: false });
const fmtTok = n => n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(n);
function fmtDur(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor(s / 60) % 60}m`;
}

/* ---------- custom agent types ---------- */

const TYPES = new Map((window.TYPES || []).map(t => [t.name, t]));

function paintRoles() {
  for (const card of $$("#roots .card")) {
    const t = TYPES.get(card.dataset.role);
    t ? card.style.setProperty("--c", t.color) : card.style.removeProperty("--c");
  }
}

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
    paintRoles();
    tdlg.close();
  } catch (err) { status($("#t-err"), err.message, true); }
};

/* ---------- tree editing ---------- */

function markDirty() { dirty = true; say("unsaved changes"); }

function newNode(role, name, type) {
  const li = $("#node-tpl").content.firstElementChild.cloneNode(true);
  li.dataset.id = role + "-" + Date.now().toString(36) + Math.random().toString(36).slice(2, 5);
  const card = li.querySelector(".card");
  card.dataset.role = role;
  card.querySelector(".name").textContent = name;
  card.querySelector(".role").textContent = role;
  card.querySelector(".rt").textContent = type?.runtime ?? "claude";
  if (type) li.dataset.defaults = JSON.stringify({ model: type.model, args: type.args, prompt: type.prompt });
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
  const li = dragged.hasAttribute("data-new")
    ? newNode(dragged.dataset.role, dragged.querySelector(".name").textContent, TYPES.get(dragged.dataset.role))
    : dragged;
  if (li.contains(ul)) return; // can't nest an agent under its own descendant
  ul.append(li);
  markDirty();
  paintRoles();
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
    return { ...JSON.parse(li.dataset.defaults || "{}"), id: li.dataset.id, name: card.querySelector(".name").textContent,
             role: card.dataset.role, runtime: card.querySelector(".rt").textContent, parent: li.parentElement.closest("li")?.dataset.id ?? "" };
  });
  await api("POST", `/api/projects/${PID}/arrangement`, agents);
  $$("li[data-defaults]", roots).forEach(li => delete li.dataset.defaults); // applied once, on creation
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

try { const w = localStorage.getItem("inspector-width"); if (w) insp.style.setProperty("--iw", w); } catch {}
$("#resize").onpointerdown = e => {
  const handle = e.currentTarget;
  handle.setPointerCapture(e.pointerId);
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
  status($("#a-msg"), "");
  showGoalState(g);
  renderStats();
  showTab(tab);
}

function showGoalState(g) {
  const li = cardOf(selected)?.parentElement, mgr = parentOf(selected);
  $("#g-hint").textContent = li?.querySelector(":scope > ul > li")
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
  const a = { name: f.name.value, runtime: f.runtime.value, model: f.model.value, args: f.args.value, prompt: f.prompt.value,
              tokenSoft: Number(f.tokenSoft.value) || 0, tokenHard: Number(f.tokenHard.value) || 0 };
  try {
    await api("PUT", `/api/agents/${selected}`, a);
    const card = cardOf(selected);
    card.querySelector(".name").textContent = $("#i-name").textContent = a.name;
    card.querySelector(".rt").textContent = a.runtime;
    status($("#a-msg"), "Saved.");
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
let previewTimer = 0, preview = null;

async function loadPreview() {
  const box = $("#m-preview"), target = mform.elements.target.value.trim();
  preview = null;
  $("#m-go").disabled = true;
  box.replaceChildren(el("span", "hint", "Checking…"));
  try {
    preview = await api("GET", `/api/agents/${selected}/merge?target=${encodeURIComponent(target)}`);
  } catch (e) { box.replaceChildren(el("p", "note bad", e.message)); return; }
  const p = preview, lines = [];
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

/* ---------- status: cards + top bar ---------- */

let summary = { agents: {}, cost: 0 };

function refreshCards() {
  for (const li of $$("li", roots)) {
    const card = li.querySelector(".card"), st = summary.agents[li.dataset.id];
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
const scheduleSummary = () => { clearTimeout(summaryTimer); summaryTimer = setTimeout(refreshSummary, 150); };

// tick the "working for" timers without refetching anything
setInterval(() => {
  for (const t of $$("#roots .elapsed")) t.textContent = fmtDur(Date.now() - Number(t.dataset.since));
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

renderTypes();
paintRoles();
refreshSummary();
