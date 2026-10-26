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
  const known = ["overview", "site", "about"].includes(name) ? name : "overview";
  for (const a of document.querySelectorAll(".tabs a")) {
    if (a.dataset.tab === known) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  for (const id of ["overview", "site", "about"]) $("tab-" + id).hidden = id !== known;
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
      return `${cert.issuer}, ещё ${cert.days_left} дн. Caddy продлит его сам.`;
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

$("logout").addEventListener("click", async () => {
  await api("api/logout", {}).catch(() => {});
  location.reload();
});

async function load() {
  showTab();
  const me = await api("api/me");
  $("who").textContent = me.username;
  $("role").textContent = ROLES[me.role] || me.role;
  $("version").textContent = me.version;
  $("about-version").textContent = me.version;
  renderSite(await api("api/site"));
}

window.addEventListener("hashchange", showTab);
load().catch((err) => message(err.message, true));
