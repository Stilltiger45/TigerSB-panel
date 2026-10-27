"use strict";
const ROLES = { main: "Основной сервер", router: "Точка маршрутизации" };
const CERT = {
  ok: ["ok", "действует"],
  expiring: ["warn", "скоро истекает"],
  untrusted: ["bad", "не доверенный"],
  missing: ["bad", "ещё не выпущен"],
  no_domain: ["off", "домен не задан"],
};
const CADDY = { active: "работает", activating: "запускается", inactive: "остановлен", failed: "ошибка запуска", unknown: "неизвестно" };
const $ = (id) => document.getElementById(id);
const TABS = ["overview", "users", "protocols", "site", "control", "about"];

async function api(path, body) {
  const opts = { credentials: "same-origin" };
  if (body !== undefined) {
    opts.method = "POST";
    opts.headers = { "Content-Type": "application/json" };
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) {
    location.reload();
    throw new Error("требуется вход");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || "Ошибка " + res.status);
  return data;
}

function showTab() {
  const name = (location.hash || "#overview").slice(1);
  const known = TABS.includes(name) ? name : "overview";
  for (const a of document.querySelectorAll(".tabs a")) {
    if (a.dataset.tab === known) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  for (const id of TABS) $("tab-" + id).hidden = id !== known;
  if (known === "control") loadControl();
  if (known === "users") loadUsers();
  if (known === "protocols") loadProtocols();
}

function setPill(el, cert) {
  const [cls, text] = CERT[cert.state] || ["off", cert.state];
  el.className = "pill " + cls;
  el.textContent = text;
}

function certText(cert) {
  switch (cert.state) {
    case "ok":
    case "expiring":
      return cert.days_left > 0
        ? `${cert.issuer}, ещё ${cert.days_left} дн. Caddy продлит его сам.`
        : `${cert.issuer}, истекает меньше чем через сутки. Caddy продлит его сам, если порт 443 открыт.`;
    case "missing":
      return "Caddy получает сертификат. Если это длится дольше пары минут, проверьте, что домен указывает на сервер и порт 443 открыт.";
    case "untrusted":
      return "Сервер отдаёт сертификат, которому браузеры не доверяют. Обычно это проходит, когда Caddy получит настоящий.";
    case "no_domain":
      return "Заглушка работает по http://IP/. Чтобы получить сертификат, укажите домен.";
    default:
      return "";
  }
}

let siteState = null;

function renderSite(data) {
  siteState = data;
  const s = data.settings;
  const form = $("site-form");
  form.domain.value = s.domain;
  form.email.value = s.email;
  form.sitename.value = s.name;
  const box = $("templates");
  box.querySelectorAll(".tpl").forEach((n) => n.remove());
  for (const t of data.templates) {
    const label = document.createElement("label");
    label.className = "tpl";
    const input = document.createElement("input");
    input.type = "radio";
    input.name = "template";
    input.value = t.id;
    input.checked = t.id === s.template;
    input.dataset.defaultName = t.default_name;
    const span = document.createElement("span");
    span.textContent = t.title;
    label.append(input, span);
    box.append(label);
  }
  renderCert(data);
  updatePreview();
}

function renderCert(data) {
  const c = data.cert;
  setPill($("cert"), c);
  setPill($("ov-cert"), c);
  $("ov-cert-text").textContent = certText(c);
  $("cert-issuer").textContent = c.issuer || "—";
  $("cert-until").textContent = c.not_after ? new Date(c.not_after).toLocaleDateString("ru-RU") : "—";
  $("cert-error").textContent = c.state === "ok" || c.state === "no_domain" ? "" : certText(c);
  $("caddy").textContent = CADDY[data.caddy] || data.caddy;
  $("domain").textContent = data.settings.domain || "не задан";
}

function updatePreview() {
  const form = $("site-form");
  const t = form.querySelector("input[name=template]:checked");
  if (!t) return;
  const q = new URLSearchParams({ t: t.value, name: form.sitename.value.trim() || t.dataset.defaultName });
  $("preview").src = "stub/preview?" + q;
}

function message(text, bad) {
  const box = $("site-msg");
  box.textContent = text;
  box.className = bad ? "notice bad" : "notice";
  box.hidden = !text;
}

let previewTimer;
$("site-form").addEventListener("input", (e) => {
  if (e.target.name === "template") {
    const prev = siteState && siteState.templates.find((t) => t.default_name === $("site-form").sitename.value);
    if (prev || !$("site-form").sitename.value.trim()) $("site-form").sitename.value = e.target.dataset.defaultName;
  }
  if (e.target.name === "template" || e.target.name === "sitename") {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(updatePreview, 250);
  }
});

$("site-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target;
  const t = form.querySelector("input[name=template]:checked");
  $("save").disabled = true;
  message("");
  try {
    const data = await api("api/site", {
      domain: form.domain.value,
      email: form.email.value,
      template: t ? t.value : "",
      name: form.sitename.value,
    });
    renderSite(data);
    renderAccess(await api("api/me"));
    message(data.warning || "Сохранено. Caddy применил настройки.", false);
  } catch (err) {
    message(err.message, true);
  } finally {
    $("save").disabled = false;
  }
});

$("recheck").addEventListener("click", async () => {
  try {
    renderCert(await api("api/site"));
  } catch (err) {
    message(err.message, true);
  }
});

function renderAccess(me) {
  const pill = $("access");
  const viaDomain = me.url && location.href.startsWith(me.url);
  $("access-url").textContent = me.url;
  $("access-url").hidden = !me.url;
  $("close-panel").hidden = !me.open;
  if (!me.url) {
    pill.className = "pill off";
    pill.textContent = "только через SSH-туннель";
    $("access-text").textContent = "Без домена панель не публикуется, чтобы пароль не шёл по сети открытым текстом. Укажите домен на вкладке «Сайт и сертификат».";
  } else if (me.open) {
    pill.className = "pill warn";
    pill.textContent = "открыта на домене";
    $("access-text").textContent = "Когда закончите настройку, закройте панель: по этому адресу снова будет заглушка. Открыть её заново можно только на сервере командой tigersb open.";
  } else {
    pill.className = "pill ok";
    pill.textContent = "спрятана за заглушкой";
    $("access-text").textContent = viaDomain
      ? "Панель закрыта и скоро перестанет открываться по этому адресу. Открыть снова: tigersb open на сервере."
      : "По адресу панели сейчас открывается заглушка. Открыть панель на домене: tigersb open на сервере.";
  }
}

function accessMessage(text, bad) {
  const box = $("access-msg");
  box.textContent = text;
  box.className = bad ? "notice bad" : "notice";
  box.hidden = !text;
}

$("close-panel").addEventListener("click", async () => {
  const ok = confirm("Закрыть панель? По её адресу будет открываться заглушка, а открыть панель снова можно будет только по SSH командой tigersb open.");
  if (!ok) return;
  $("close-panel").disabled = true;
  accessMessage("");
  try {
    await api("api/panel/close", {});
    renderAccess(await api("api/me"));
    accessMessage("Панель закрыта. Caddy применит это через пару секунд.", false);
  } catch (err) {
    accessMessage(err.message, true);
  } finally {
    $("close-panel").disabled = false;
  }
});

$("logout").addEventListener("click", async () => {
  await api("api/logout", {}).catch(() => {});
  location.reload();
});

// ---- Control tab ----
const HEALTH = { ok: ["ok", "в порядке"], warn: ["warn", "внимание"], fail: ["bad", "ошибка"], off: ["off", "не проверялось"], soon: ["soon", "скоро"] };
const SVG = "http://www.w3.org/2000/svg";
let hours = 24;
let controlTimer = null;
let jobTimer = null;
let controlVersion = "";

function bytes(n) {
  if (n == null) return "—";
  const units = ["Б", "КБ", "МБ", "ГБ", "ТБ"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return (i === 0 ? n.toFixed(0) : n.toFixed(1)) + " " + units[i];
}
const rate = (n) => (n == null ? "—" : bytes(n) + "/с");
const pct = (used, total) => (total ? (100 * used) / total : 0);
const when = (t) => new Date(t).toLocaleString("ru-RU", { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });

function uptime(sec) {
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  return d > 0 ? `${d} дн. ${h} ч` : `${h} ч ${Math.floor((sec % 3600) / 60)} мин`;
}

function setBar(id, value) {
  const bar = $(id);
  bar.style.width = Math.max(0, Math.min(100, value)) + "%";
  bar.classList.toggle("hot", value >= 90);
}

function renderMeters(m) {
  $("m-empty").hidden = !!m;
  $("meters").hidden = !m;
  if (!m) {
    $("m-age").textContent = "";
    return;
  }
  const ago = Math.max(0, Math.round((Date.now() / 1000 - m.time) / 60));
  $("m-age").textContent = `замер ${ago === 0 ? "только что" : ago + " мин назад"} · сервер работает ${uptime(m.uptime)}`;
  $("m-cpu").textContent = m.cpu == null ? "—" : Math.round(m.cpu) + "%";
  setBar("m-cpu-bar", m.cpu || 0);
  const load = (m.load || []).map((x) => x.toFixed(2)).join(" ");
  $("m-cpu-sub").textContent = `${m.cores} ядр. · нагрузка ${load}` + (m.steal >= 1 ? ` · steal ${Math.round(m.steal)}%` : "");
  const mem = pct(m.mem_used, m.mem_total);
  $("m-mem").textContent = Math.round(mem) + "%";
  setBar("m-mem-bar", mem);
  $("m-mem-sub").textContent = `${bytes(m.mem_used)} из ${bytes(m.mem_total)}` + (m.swap_total ? ` · подкачка ${bytes(m.swap_used)}` : "");
  const disk = pct(m.disk_used, m.disk_total);
  $("m-disk").textContent = Math.round(disk) + "%";
  setBar("m-disk-bar", disk);
  $("m-disk-sub").textContent = `свободно ${bytes(m.disk_total - m.disk_used)} из ${bytes(m.disk_total)}`;
  $("m-net").textContent = m.rx_rate == null ? "—" : "↓ " + rate(m.rx_rate);
  $("m-net-sub").textContent = m.tx_rate == null ? "скорость появится со вторым замером" : "↑ " + rate(m.tx_rate) + " исходящий";
}

function svgEl(name, attrs) {
  const el = document.createElementNS(SVG, name);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  return el;
}

// drawChart draws up to two series; gaps (null) break the line.
function drawChart(svg, points, keyA, keyB, top) {
  svg.replaceChildren();
  for (const y of [0, 0.5, 1]) svg.append(svgEl("line", { x1: 0, x2: 600, y1: 4 + y * 152, y2: 4 + y * 152, class: "grid-line" }));
  if (points.length < 2) return;
  const t0 = points[0].t;
  const span = Math.max(1, points[points.length - 1].t - t0);
  const x = (p) => ((p.t - t0) / span) * 600;
  const y = (v) => 156 - (Math.min(v, top) / top) * 152;
  for (const [key, cls] of [[keyB, "line-b"], [keyA, "line-a"]]) {
    let run = [];
    const flush = () => {
      if (run.length > 1) {
        const d = run.map((p, i) => (i ? "L" : "M") + x(p).toFixed(1) + " " + y(p[key]).toFixed(1)).join(" ");
        if (cls === "line-a") svg.append(svgEl("path", { d: `${d} L${x(run[run.length - 1]).toFixed(1)} 156 L${x(run[0]).toFixed(1)} 156 Z`, class: "area-a" }));
        svg.append(svgEl("path", { d, class: cls }));
      }
      run = [];
    };
    for (const p of points) {
      if (p[key] == null) flush();
      else run.push(p);
    }
    flush();
  }
}

function renderCharts(history) {
  drawChart($("chart-load"), history, "cpu", "mem", 100);
  let peak = 0;
  for (const p of history) peak = Math.max(peak, p.rx || 0, p.tx || 0);
  drawChart($("chart-net"), history, "rx", "tx", peak * 1.1 || 1);
  $("net-max").textContent = rate(peak);
  const from = history.length ? when(history[0].t * 1000) : "";
  const to = history.length ? when(history[history.length - 1].t * 1000) : "";
  for (const n of ["load", "net"]) {
    $(`ax-${n}-from`).textContent = from;
    $(`ax-${n}-to`).textContent = to;
  }
}

function renderHealth(r) {
  const list = $("checks");
  list.replaceChildren();
  if (!r) {
    $("h-pill").className = "pill off";
    $("h-pill").textContent = "нет данных";
    $("h-time").textContent = "Нажмите «Проверить сейчас».";
    return;
  }
  const bad = r.results.filter((x) => x.state === "fail").length;
  const warn = r.results.filter((x) => x.state === "warn").length;
  const pill = $("h-pill");
  if (bad) [pill.className, pill.textContent] = ["pill bad", `ошибок: ${bad}`];
  else if (warn) [pill.className, pill.textContent] = ["pill warn", `предупреждений: ${warn}`];
  else [pill.className, pill.textContent] = ["pill ok", "всё работает"];
  $("h-time").textContent = "проверено " + when(r.checked);
  // Problems first; the order within each state stays as the server sent it.
  const order = { fail: 0, warn: 1, ok: 2, off: 3, soon: 4 };
  $("h-sum").textContent = `Все проверки (${r.results.length})`;
  const sorted = [...r.results].sort((a, b) => (order[a.state] ?? 3) - (order[b.state] ?? 3));
  for (const res of sorted) {
    const li = document.createElement("li");
    li.className = res.state === "soon" ? "soon" : "";
    const title = document.createElement("b");
    title.textContent = res.title;
    const [cls, text] = HEALTH[res.state] || ["off", res.state];
    const p = document.createElement("span");
    p.className = "pill " + cls;
    p.textContent = text;
    const detail = document.createElement("span");
    detail.className = "detail";
    detail.textContent = res.detail;
    li.append(title, p, detail);
    list.append(li);
  }
}

// ---- VPN cores: one card per container with a restart button ----
const CORE_DROP = { xray: "Подключения через gRPC и XHTTP прервутся на секунду.", singbox: "Подключения через TUIC прервутся, клиенты переподключатся за несколько секунд." };

function coreMessage(text, bad) {
  const box = $("core-msg");
  box.textContent = text;
  box.className = bad ? "notice bad" : "notice";
  box.hidden = !text;
}

let coresData = [];

function renderCores(cores, report) {
  coresData = cores || [];
  const box = $("cores");
  box.replaceChildren();
  for (const c of coresData) {
    const res = report && report.results.find((x) => x.id === c.id);
    const card = document.createElement("div");
    card.className = "glass core-card";
    const head = document.createElement("div");
    head.className = "head-row";
    const h = document.createElement("h2");
    h.textContent = c.title + " ";
    const tag = document.createElement("span");
    tag.className = "tag core";
    tag.textContent = c.protocols;
    h.append(tag);
    const pill = document.createElement("span");
    if (c.state !== "active") [pill.className, pill.textContent] = ["pill bad", "не работает: " + c.state];
    else if (res) {
      const [cls, text] = HEALTH[res.state] || ["off", res.state];
      [pill.className, pill.textContent] = ["pill " + cls, text];
    } else [pill.className, pill.textContent] = ["pill ok", "работает"];
    head.append(h, pill);
    const detail = document.createElement("p");
    detail.className = "muted";
    detail.textContent = res ? res.detail : "Контейнер " + (c.state === "active" ? "запущен." : "остановлен.");
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = c.state === "active" && (!res || res.state !== "fail") ? "ghost" : "";
    btn.textContent = "Перезапустить";
    btn.addEventListener("click", async () => {
      if (!confirm(`Перезапустить ${c.title}?\n\n${CORE_DROP[c.id] || ""}`)) return;
      btn.disabled = true;
      btn.textContent = "Перезапускаю…";
      coreMessage("");
      try {
        const d = await api("api/control/core", { core: c.id });
        renderHealth(d.health);
        renderCores(d.cores, d.health);
        coreMessage(d.warning || `${c.title} перезапущен.`, !!d.warning);
      } catch (err) {
        coreMessage(err.message, true);
        btn.disabled = false;
        btn.textContent = "Перезапустить";
      }
    });
    card.append(head, detail, btn);
    box.append(card);
  }
}

function uMessage(text, bad) {
  const box = $("u-msg");
  box.textContent = text;
  box.className = bad ? "notice bad" : "notice";
  box.hidden = !text;
}

function renderUpdate(u) {
  const st = u.status || {};
  const c = u.check;
  const busy = st.phase === "queued" || st.phase === "running";
  $("u-current").textContent = controlVersion || "—";
  if (c && c.error) $("u-latest").textContent = "не удалось проверить: " + c.error;
  else if (c && c.latest) $("u-latest").textContent = c.available ? `${c.latest} от ${when(c.date)}` : `${c.latest}, у вас последняя`;
  else $("u-latest").textContent = "не проверялось";
  const changes = $("u-changes");
  changes.replaceChildren();
  for (const ch of (c && c.available && c.changes) || []) {
    const li = document.createElement("li");
    li.textContent = ch;
    changes.append(li);
  }
  changes.hidden = !changes.children.length;
  $("u-start").hidden = !(c && c.available) || busy;
  $("u-check").disabled = busy;
  $("p-create").disabled = busy;
  for (const b of document.querySelectorAll("#points button")) b.disabled = busy;
  if (busy) uMessage(st.message || "Идёт обновление…", false);
  else if (st.phase === "done") uMessage(st.message, false);
  else if (st.phase === "failed" || st.phase === "interrupted") uMessage(st.message, true);
  else uMessage("", false);
  $("u-log").textContent = u.log || "";
  $("u-log-box").hidden = !u.log;
  if (busy) watchJob();
}

function renderPoints(points) {
  const list = $("points");
  list.replaceChildren();
  if (!points.length) {
    const li = document.createElement("li");
    li.textContent = "Пока ни одной.";
    li.className = "muted";
    list.append(li);
    return;
  }
  for (const p of points) {
    const li = document.createElement("li");
    const info = document.createElement("span");
    const name = document.createElement("b");
    name.textContent = when(p.created);
    const sub = document.createElement("small");
    sub.textContent = `${p.version} · ${p.reason} · ${bytes(p.size)}`;
    info.append(name, sub);
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "ghost";
    btn.textContent = "Вернуть";
    btn.addEventListener("click", () => rollback(p));
    li.append(info, btn);
    list.append(li);
  }
}

async function loadControl() {
  clearTimeout(controlTimer);
  try {
    const d = await api("api/control?hours=" + hours);
    renderMeters(d.latest);
    renderCharts(d.history);
    renderHealth(d.health);
    renderCores(d.cores, d.health);
    renderPoints(d.points);
    renderUpdate(d.update);
  } catch (err) {
    uMessage(err.message, true);
  }
  if (!$("tab-control").hidden) controlTimer = setTimeout(loadControl, 60000);
}

// watchJob polls the update. The panel restarts during it, so failed
// requests are expected for a while.
function watchJob() {
  if (jobTimer) return;
  const tick = async () => {
    let u;
    try {
      u = await api("api/control/job");
    } catch {
      uMessage("Панель перезапускается, жду…", false);
      jobTimer = setTimeout(tick, 3000);
      return;
    }
    jobTimer = null;
    const st = u.status || {};
    if (st.phase === "queued" || st.phase === "running") {
      renderUpdate(u); // schedules the next poll
      return;
    }
    if (st.phase === "done") {
      uMessage(st.message + " Обновляю страницу…", false);
      setTimeout(() => location.reload(), 2500);
      return;
    }
    loadControl();
  };
  jobTimer = setTimeout(tick, 3000);
}

async function rollback(p) {
  const ok = confirm(`Вернуть точку от ${when(p.created)} (версия ${p.version})?\n\nПрограмма панели, настройки, база и сертификаты станут такими, какими были тогда. Всё, что изменилось после, пропадёт. Панель перезапустится.`);
  if (!ok) return;
  try {
    renderUpdate(await api("api/control/restore", { id: p.id }));
  } catch (err) {
    uMessage(err.message, true);
  }
}

$("range").addEventListener("click", (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  hours = Number(b.dataset.hours);
  for (const x of $("range").querySelectorAll("button")) x.setAttribute("aria-pressed", String(x === b));
  loadControl();
});

$("h-run").addEventListener("click", async () => {
  $("h-run").disabled = true;
  try {
    const r = await api("api/control/health", {});
    renderHealth(r);
    renderCores(coresData, r);
  } catch (err) {
    uMessage(err.message, true);
  } finally {
    $("h-run").disabled = false;
  }
});

$("u-check").addEventListener("click", async () => {
  $("u-check").disabled = true;
  try {
    renderUpdate(await api("api/control/check", {}));
  } catch (err) {
    uMessage(err.message, true);
  } finally {
    $("u-check").disabled = false;
  }
});

$("u-start").addEventListener("click", async () => {
  if (!confirm("Обновить панель?\n\nСначала создастся точка восстановления. Потом панель скачает и соберёт новую версию, это займёт пару минут, и перезапустится. Если что-то пойдёт не так, точка вернётся сама.")) return;
  try {
    renderUpdate(await api("api/control/update", {}));
  } catch (err) {
    uMessage(err.message, true);
  }
});

$("p-create").addEventListener("click", async () => {
  $("p-create").disabled = true;
  const box = $("p-msg");
  try {
    const p = await api("api/control/restore", {});
    box.className = "notice";
    box.textContent = `Точка от ${when(p.created)} создана.`;
    loadControl();
  } catch (err) {
    box.className = "notice bad";
    box.textContent = err.message;
  } finally {
    box.hidden = false;
    $("p-create").disabled = false;
  }
});

async function load() {
  showTab();
  const me = await api("api/me");
  $("who").textContent = me.username;
  $("role").textContent = ROLES[me.role] || me.role;
  $("version").textContent = me.version;
  $("about-version").textContent = me.version;
  controlVersion = me.version;
  if (!$("tab-control").hidden) loadControl();
  renderAccess(me);
  renderSite(await api("api/site"));
}

window.addEventListener("hashchange", showTab);
// vpn.js loads after this file; start once every script is in.
window.addEventListener("DOMContentLoaded", () => load().catch((err) => message(err.message, true)));
