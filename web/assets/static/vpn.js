"use strict";
// Users and Protocols tabs. Uses $, api, bytes and when from app.js.

const UNIT = { active: ["ok", "работает"], activating: ["warn", "запускается"], inactive: ["bad", "остановлен"], failed: ["bad", "ошибка"], unknown: ["off", "нет данных"] };

function unitPill(el, label, state) {
  const [cls, text] = UNIT[state] || ["bad", state];
  el.className = "pill " + cls;
  el.textContent = `${label}: ${text}`;
}

function notice(id, text, bad) {
  const box = $(id);
  box.textContent = text || "";
  box.className = bad ? "notice bad" : "notice";
  box.hidden = !text;
}

function el(tag, attrs, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (v === true) n.setAttribute(k, "");
    else if (v !== false && v != null) n.setAttribute(k, v);
  }
  n.append(...children);
  return n;
}

const ago = (u) => {
  if (u.online.length) return "в сети сейчас";
  const sec = u.seen_at;
  if (!sec) return "ещё не подключался";
  const m = Math.round((Date.now() / 1000 - sec) / 60);
  if (m < 2) return "был в сети только что";
  if (m < 60) return `был в сети ${m} мин назад`;
  return "был в сети " + when(sec * 1000);
};

async function copy(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    // Over plain HTTP through an SSH tunnel the clipboard API may be off.
    const area = el("textarea", {}, text);
    document.body.append(area);
    area.select();
    document.execCommand("copy");
    area.remove();
  }
  const old = btn.textContent;
  btn.textContent = "Скопировано";
  setTimeout(() => (btn.textContent = old), 1500);
}

// ---- Users ----
let usersData = null;
const openUsers = new Set();

async function loadUsers() {
  try {
    renderUsers(await api("api/users"));
  } catch (err) {
    notice("users-msg", err.message, true);
  }
}

function userState(u) {
  if (!u.enabled) return ["off", "выключен"];
  const n = u.online.length;
  if (u.over) return ["bad", `превышение: ${n} из ${u.devices}`];
  if (n) return ["ok", `в сети: ${n} из ${u.devices}`];
  return ["off", `устройств: ${u.device_list.length} из ${u.devices}`];
}

function renderUsers(d) {
  usersData = d;
  unitPill($("users-xray"), "Xray", d.xray);
  unitPill($("users-sub"), "Подписки", d.sub);
  if (d.warning) notice("users-msg", d.warning, true);
  $("users-count").textContent = d.users.length ? `· ${d.users.length}` : "";
  $("users-filter").hidden = d.users.length < 8;
  const q = $("users-filter").value.trim().toLowerCase();
  const list = $("users");
  list.replaceChildren();
  if (!d.users.length) {
    list.append(el("li", { class: "muted empty", text: "Пока никого. Добавьте первого пользователя выше." }));
    return;
  }
  for (const u of d.users) {
    if (q && !u.name.toLowerCase().includes(q)) continue;
    list.append(userRow(u));
  }
}

function userRow(u) {
  const [cls, text] = userState(u);
  const li = el("li", { class: u.enabled ? "user" : "user disabled" });
  const head = el("div", { class: "user-head" },
    el("button", { type: "button", class: "user-name", "aria-expanded": String(openUsers.has(u.id)), onclick: () => toggleUser(u.id) },
      el("b", { text: u.name }),
      el("small", { text: `↓ ${bytes(u.down)} · ↑ ${bytes(u.up)} · ${u.enabled ? ago(u) : "ссылки не работают"}` })),
    el("span", { class: "pill " + cls, text }));
  li.append(head);
  if (openUsers.has(u.id)) li.append(userBody(u));
  return li;
}

function toggleUser(id) {
  if (openUsers.has(id)) openUsers.delete(id);
  else openUsers.add(id);
  renderUsers(usersData);
}

async function changeUser(body, btn) {
  if (btn) btn.disabled = true;
  notice("users-msg", "");
  try {
    renderUsers(await api("api/users/update", body));
  } catch (err) {
    notice("users-msg", err.message, true);
  } finally {
    if (btn) btn.disabled = false;
  }
}

const SUBS = [
  { kind: "tigersb", title: "Ключ TigerSB", hint: "Для приложения TigerSB: профиль подписан ключом панели, подменить его нельзя." },
  { kind: "https", title: "Подписка https", hint: "Для v2rayNG, Hiddify, Happ, Streisand и других. TigerSB её тоже понимает." },
];

function agoText(sec) {
  const m = Math.round((Date.now() / 1000 - sec) / 60);
  if (m < 2) return "только что";
  if (m < 60) return `${m} мин назад`;
  return when(sec * 1000);
}

function subBlock(u) {
  const box = el("div", { class: "links" }, el("h3", { text: "Подписки" }));
  if (!u.subs) {
    box.append(el("p", { class: "muted", text: "Подпискам нужен домен с сертификатом: задайте его на вкладке «Сайт и сертификат»." }));
    return box;
  }
  for (const s of SUBS) {
    const url = u.subs[s.kind];
    const qr = el("img", { class: "qr", alt: "QR-код: " + s.title, hidden: true });
    const qrBtn = el("button", {
      type: "button", class: "ghost small", text: "QR",
      onclick: () => {
        if (!qr.src) qr.src = `api/users/qr?id=${u.id}&kind=${s.kind}&k=${encodeURIComponent(u.subs.https)}`;
        qr.hidden = !qr.hidden;
      },
    });
    const copyBtn = el("button", { type: "button", class: "ghost small", text: "Копировать", onclick: (e) => copy(url, e.target) });
    box.append(el("div", { class: "link-row" },
      el("div", { class: "link-head" }, el("b", { text: s.title }), el("span", { class: "row" }, copyBtn, qrBtn)),
      el("code", { class: "mono link-text", text: url }),
      el("small", { class: "faint", text: s.hint }),
      qr));
  }
  return box;
}

function deviceBlock(u) {
  const box = el("div", { class: "devices" },
    el("h3", {}, "Устройства ", el("span", { class: "faint", text: `${u.device_list.length} из ${u.devices}` })));
  if (!u.device_list.length) {
    box.append(el("p", { class: "faint", text: "Устройство появится здесь, когда приложение впервые скачает подписку." }));
    return box;
  }
  const ul = el("ul", { class: "device-list" });
  for (const d of u.device_list) {
    const app = d.app.split(" ")[0];
    const title = d.model || app.split("/")[0] || "Устройство";
    const os = [d.os, d.os_version].filter(Boolean).join(" ");
    const del = el("button", {
      type: "button", class: "ghost small danger", text: "Удалить",
      onclick: async (e) => {
        if (!confirm(`Удалить «${title}» из устройств «${u.name}»? Место освободится. Если устройство снова обновит подписку, оно займёт место заново, пока есть свободные. Чтобы отключить его насовсем, выдайте «Новые ссылки».`)) return;
        e.target.disabled = true;
        notice("users-msg", "");
        try {
          renderUsers(await api("api/users/device/delete", { user_id: u.id, id: d.id }));
        } catch (err) {
          notice("users-msg", err.message, true);
          e.target.disabled = false;
        }
      },
    });
    const meta = [os, app].filter(Boolean).join(" · ");
    ul.append(el("li", { class: "device" },
      el("div", { class: "device-main" },
        el("div", { class: "device-title" },
          el("b", { text: title }),
          d.online ? el("span", { class: "pill ok", text: "в сети" }) : "",
          d.no_hwid ? el("span", { class: "pill off", text: "без HWID", title: "Приложение не сообщает HWID: панель считает его одним устройством на приложение." }) : ""),
        meta ? el("small", { class: "muted", text: meta }) : "",
        el("small", { class: "faint", text: `${d.ip ? d.ip + " · " : ""}подписка обновлена ${agoText(d.last_seen)} · с ${when(d.first_seen * 1000)}` })),
      del));
  }
  box.append(ul);
  return box;
}

function userBody(u) {
  const body = el("div", { class: "user-body" });
  body.append(subBlock(u), deviceBlock(u));

  const online = el("div", { class: "online" }, el("span", { class: "muted", text: "Подключены к VPN сейчас" }));
  if (u.online.length) {
    const ul = el("ul", { class: "ips" });
    for (const a of u.online) ul.append(el("li", {}, el("code", { text: a.ip }), el("small", { class: "faint", text: a.last_seen ? " · " + when(a.last_seen * 1000) : "" })));
    online.append(ul);
  } else {
    online.append(el("p", { class: "faint", text: u.enabled ? "никого" : "пользователь выключен" }));
  }
  body.append(online);

  const devices = el("input", { type: "number", min: 1, max: 20, value: u.devices, class: "num", "aria-label": "Устройств" });
  const save = el("button", { type: "button", class: "ghost small", text: "Сохранить", onclick: (e) => changeUser({ id: u.id, devices: Number(devices.value) }, e.target) });
  const name = el("input", { value: u.name, maxlength: 40, "aria-label": "Имя" });
  const rename = el("button", { type: "button", class: "ghost small", text: "Переименовать", onclick: (e) => changeUser({ id: u.id, name: name.value }, e.target) });
  body.append(el("div", { class: "user-controls" },
    el("label", {}, "Устройств", el("span", { class: "row" }, devices, save)),
    el("label", {}, "Имя", el("span", { class: "row" }, name, rename))));

  body.append(el("div", { class: "row" },
    el("button", {
      type: "button", class: "ghost", text: u.enabled ? "Выключить" : "Включить",
      onclick: (e) => changeUser({ id: u.id, enabled: !u.enabled }, e.target),
    }),
    el("button", {
      type: "button", class: "ghost", text: "Новые ссылки",
      onclick: (e) => {
        if (confirm(`Выдать «${u.name}» новые ссылки? Старые подписки перестанут работать, все устройства сразу отключатся и список устройств очистится.`)) changeUser({ id: u.id, reissue: true }, e.target);
      },
    }),
    el("button", {
      type: "button", class: "ghost", text: "Обнулить трафик",
      onclick: (e) => changeUser({ id: u.id, reset_traffic: true }, e.target),
    }),
    el("button", {
      type: "button", class: "ghost danger", text: "Удалить",
      onclick: async (e) => {
        if (!confirm(`Удалить «${u.name}»? Его устройства сразу отключатся.`)) return;
        e.target.disabled = true;
        try {
          openUsers.delete(u.id);
          renderUsers(await api("api/users/delete", { id: u.id }));
        } catch (err) {
          notice("users-msg", err.message, true);
          e.target.disabled = false;
        }
      },
    })));
  body.append(el("p", { class: "faint", text: `Создан ${when(u.created)} · UUID ${u.uuid}` }));
  return body;
}

$("user-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target;
  $("user-add").disabled = true;
  notice("users-msg", "");
  try {
    const d = await api("api/users/add", { name: form.name.value, devices: Number(form.devices.value) });
    const added = d.users[d.users.length - 1];
    if (added) openUsers.add(added.id);
    renderUsers(d);
    if (!d.warning) notice("users-msg", `«${form.name.value.trim()}» добавлен. Отправьте ему ключ или подписку, или покажите QR-код.`);
    form.name.value = "";
  } catch (err) {
    notice("users-msg", err.message, true);
  } finally {
    $("user-add").disabled = false;
  }
});

$("users-refresh").addEventListener("click", loadUsers);
$("users-filter").addEventListener("input", () => usersData && renderUsers(usersData));

// ---- Protocols ----
// Field specs build both forms. kind: text, num, bool, select, range (a
// number or "from-to"). client marks settings that only clients use: they
// travel in the links.
const REALITY_FIELDS = [
  { key: "sni", label: "SNI (сайт для маскировки)", kind: "text", wide: true, sni: true, hint: "Сайт, под который маскируется сервер. Подберите его кнопками ниже." },
  { key: "target", label: "Куда отправлять чужих (target)", kind: "text", hint: "Кто подключится без ключа, попадёт на этот сайт. Пусто — SNI:443." },
  { key: "fingerprint", label: "Отпечаток браузера (fingerprint)", kind: "select", options: "fingerprints", client: true },
  { key: "spider_x", label: "spiderX", kind: "text", client: true },
];

const GRPC_FIELDS = [
  { key: "service_name", label: "Имя сервиса (serviceName)", kind: "text", hint: "Путь, по которому клиент обращается к серверу. Латиница, цифры, точка, дефис." },
  { key: "authority", label: "Authority", kind: "text", client: true, hint: "Пусто — как обычно." },
  { key: "multi_mode", label: "Режим multi вместо gun", kind: "bool", client: true },
  { key: "idle_timeout", label: "Пинг при простое, с (idle_timeout)", kind: "num", hint: "0 — выключено. Держит соединение живым у мобильных операторов." },
  { key: "health_check_timeout", label: "Ждать ответ на пинг, с", kind: "num" },
  { key: "permit_without_stream", label: "Пинговать без активных потоков", kind: "bool", client: true },
  { key: "initial_windows_size", label: "Начальное окно, байт", kind: "num", client: true, hint: "0 — по умолчанию." },
  { key: "user_agent", label: "User-Agent", kind: "text", client: true, hint: "Пусто — как у Chrome." },
];

const XHTTP_FIELDS = [
  { key: "path", label: "Путь (path)", kind: "text" },
  { key: "host", label: "Host", kind: "text", hint: "Пусто — любой." },
  { key: "mode", label: "Режим", kind: "select", options: "modes", hint: "auto сам выбирает packet-up или stream-up/stream-one." },
  { key: "uplink_http_method", label: "Метод отправки данных", kind: "select", options: "methods", client: true },
  { key: "x_padding_bytes", label: "Случайное дополнение, байт (xPaddingBytes)", kind: "range" },
  { key: "sc_max_each_post_bytes", label: "Наибольший POST, байт (scMaxEachPostBytes)", kind: "range" },
  { key: "sc_min_posts_interval_ms", label: "Пауза между POST, мс (scMinPostsIntervalMs)", kind: "range", client: true },
  { key: "sc_max_buffered_posts", label: "POST в очереди сервера (scMaxBufferedPosts)", kind: "num" },
  { key: "sc_stream_up_server_secs", label: "Пинг stream-up от сервера, с", kind: "range" },
  { key: "no_sse_header", label: "Без заголовка SSE (noSSEHeader)", kind: "bool" },
];

const XMUX_FIELDS = [
  { key: "max_concurrency", label: "Потоков на соединение (maxConcurrency)", kind: "range", hint: "Или это, или maxConnections." },
  { key: "max_connections", label: "Соединений (maxConnections)", kind: "range" },
  { key: "c_max_reuse_times", label: "Повторов соединения (cMaxReuseTimes)", kind: "range" },
  { key: "h_max_request_times", label: "Запросов на соединение (hMaxRequestTimes)", kind: "range" },
  { key: "h_max_reusable_secs", label: "Жизнь соединения, с (hMaxReusableSecs)", kind: "range" },
  { key: "h_keep_alive_period", label: "Пинг HTTP/2, с (hKeepAlivePeriod)", kind: "num" },
];

let protoData = null;

function field(obj, spec, onChange) {
  const id = "f-" + Math.random().toString(36).slice(2);
  let input;
  if (spec.kind === "bool") {
    input = el("input", { type: "checkbox", id });
    input.checked = !!obj[spec.key];
    input.addEventListener("change", () => onChange((obj[spec.key] = input.checked)));
    return el("label", { class: "check", for: id }, input, spec.label, spec.client ? el("span", { class: "tag", text: "клиент" }) : "");
  }
  if (spec.kind === "select") {
    input = el("select", { id });
    for (const o of protoData[spec.options]) input.append(el("option", { value: o, text: o }));
    input.value = obj[spec.key];
  } else {
    input = el("input", { id, autocomplete: "off", spellcheck: "false" });
    if (spec.kind === "num") {
      input.type = "number";
      input.min = 0;
    }
    if (spec.kind === "range") input.placeholder = "например, 100-1000";
    input.value = obj[spec.key] ?? "";
  }
  input.addEventListener("input", () => {
    obj[spec.key] = spec.kind === "num" ? Number(input.value || 0) : input.value;
    onChange();
  });
  const label = el("label", { for: id, class: spec.wide ? "wide" : "" },
    el("span", {}, spec.label, spec.client ? el("span", { class: "tag", text: "клиент" }) : ""), input);
  if (spec.hint) label.append(el("span", { class: "hint", text: spec.hint }));
  return label;
}

function dirty() {
  notice("pr-msg", "Есть несохранённые изменения.");
}

function sniTools(proto, r) {
  const out = el("p", { class: "probe", hidden: true });
  const check = el("button", {
    type: "button", class: "ghost small", text: "Проверить",
    onclick: async (e) => {
      e.target.disabled = true;
      out.hidden = false;
      out.className = "probe";
      out.textContent = "Проверяю с сервера…";
      try {
        showProbe(out, await api("api/protocols/check", { sni: r.sni }));
      } catch (err) {
        out.className = "probe bad";
        out.textContent = err.message;
      } finally {
        e.target.disabled = false;
      }
    },
  });
  const near = el("button", {
    type: "button", class: "ghost small", text: "Найти рядом",
    onclick: () => {
      scanFor = proto;
      $("scan-form").scrollIntoView({ behavior: "smooth", block: "center" });
      runScan();
    },
  });
  return el("div", { class: "sni-tools" }, el("div", { class: "row" }, check, near), out);
}

function marks(p) {
  return [["TLS 1.3", p.tls13], ["HTTP/2", p.h2], ["X25519", p.x25519], ["сертификат", p.cert_ok]];
}

function showProbe(out, p) {
  out.replaceChildren();
  if (p.error) {
    out.className = "probe bad";
    out.textContent = p.error;
    return;
  }
  out.className = p.good ? "probe ok" : "probe bad";
  out.append(el("b", { text: p.good ? "Подходит. " : "Не подходит. " }));
  for (const [name, ok] of marks(p)) out.append(el("span", { class: ok ? "mark yes" : "mark no", text: (ok ? "✓ " : "✗ ") + name }));
  out.append(el("span", { class: "faint", text: ` · ${p.latency_ms} мс` + (p.issuer ? ` · ${p.issuer}` : "") }));
}

function keysBlock(proto, r) {
  const pub = el("code", { class: "mono", text: r.public_key });
  const priv = el("input", { value: r.private_key, autocomplete: "off", spellcheck: "false" });
  priv.addEventListener("input", () => {
    r.private_key = priv.value.trim();
    pub.textContent = "появится после сохранения";
    dirty();
  });
  const sids = el("input", { value: (r.short_ids || []).join(", "), autocomplete: "off", spellcheck: "false" });
  sids.addEventListener("input", () => {
    r.short_ids = sids.value.split(/[\s,]+/).filter(Boolean);
    dirty();
  });
  const regen = el("button", {
    type: "button", class: "ghost small", text: "Новые ключи",
    onclick: async (e) => {
      if (!confirm("Сделать новые ключи? После сохранения все старые ссылки этого протокола перестанут работать.")) return;
      e.target.disabled = true;
      try {
        const k = await api("api/protocols/keys", {});
        Object.assign(r, { private_key: k.private_key, public_key: k.public_key, short_ids: k.short_ids });
        priv.value = k.private_key;
        pub.textContent = k.public_key;
        sids.value = k.short_ids.join(", ");
        dirty();
      } catch (err) {
        notice("pr-msg", err.message, true);
      } finally {
        e.target.disabled = false;
      }
    },
  });
  return el("details", { class: "keys" },
    el("summary", { class: "muted", text: "Ключи REALITY" }),
    el("div", { class: "fields" },
      el("label", { class: "wide" }, "Открытый ключ (уходит в ссылки)", pub),
      el("label", { class: "wide" }, "Закрытый ключ", priv),
      el("label", { class: "wide" }, el("span", {}, "Short ID через запятую", el("span", { class: "hint", text: " в ссылки уходит первый" })), sids)),
    el("div", { class: "row" }, regen));
}

function protoCard(proto, title, fields, extra) {
  const p = protoData.settings[proto];
  const r = p.reality;
  const enabled = el("input", { type: "checkbox" });
  enabled.checked = p.enabled;
  enabled.addEventListener("change", () => {
    p.enabled = enabled.checked;
    body.hidden = !p.enabled;
    dirty();
  });
  const port = el("input", { type: "number", min: 1, max: 65535, value: p.port, class: "num" });
  port.addEventListener("input", () => {
    p.port = Number(port.value);
    portHint.textContent = portText(p.port);
    dirty();
  });
  const portHint = el("span", { class: "hint", text: portText(p.port) });

  const disguise = el("div", { class: "fields" });
  for (const spec of REALITY_FIELDS) {
    const f = field(r, spec, dirty);
    disguise.append(f);
    if (spec.sni) f.append(sniTools(proto, r));
  }
  const transport = el("div", { class: "fields" });
  for (const spec of fields) transport.append(field(p, spec, dirty));

  const body = el("div", { class: "proto-body" },
    el("label", {}, "Порт", port, portHint),
    el("h3", { text: "Маскировка (REALITY)" }), disguise,
    keysBlock(proto, r),
    el("h3", { text: "Настройки " + title }), transport);
  if (extra) body.append(extra(p));
  body.hidden = !p.enabled;
  return el("div", { class: "glass proto", id: "proto-" + proto },
    el("div", { class: "head-row" }, el("h2", { text: title }), el("label", { class: "check" }, enabled, "включён")),
    body);
}

function portText(port) {
  if (port === 443) return "Общий с сайтом: сервер различает подключения по SNI.";
  const busy = protoData.reserved[port];
  if (busy) return `Занят: ${busy}.`;
  if (port < 1024) return "Можно 443 или от 1024.";
  return "Откройте этот порт в файрволе хостинга, если он там есть.";
}

function xmuxBlock(p) {
  const box = el("div", { class: "fields" });
  for (const spec of XMUX_FIELDS) box.append(field(p.xmux, spec, dirty));
  return el("details", { class: "keys" },
    el("summary", { class: "muted", text: "Мультиплексирование (xmux, клиент)" }),
    el("p", { class: "hint", text: "Как клиент делит и переиспользует соединения. Пустое поле — значение Xray по умолчанию." }),
    box);
}

function renderProtocols(d) {
  protoData = d;
  unitPill($("pr-xray"), "Xray", d.xray);
  unitPill($("pr-edge"), "Порт 443", d.edge);
  $("pr-address").value = d.settings.address;
  $("pr-address").placeholder = d.auto_address || "IP сервера";
  $("pr-address-hint").textContent = `Пусто — ${d.auto_address || "IP сервера"} (определён автоматически). Укажите другой IP или домен, если сервер за NAT.`;
  $("scan-form").target.placeholder = (d.auto_address || "IP сервера") + " и соседи";
  $("pr-cards").replaceChildren(
    protoCard("grpc", "gRPC", GRPC_FIELDS),
    protoCard("xhttp", "XHTTP", XHTTP_FIELDS, xmuxBlock));
}

async function loadProtocols() {
  try {
    renderProtocols(await api("api/protocols"));
    notice("pr-msg", "");
  } catch (err) {
    notice("pr-msg", err.message, true);
  }
}

$("pr-address").addEventListener("input", (e) => {
  protoData.settings.address = e.target.value.trim();
  dirty();
});

$("pr-reset").addEventListener("click", loadProtocols);

$("pr-save").addEventListener("click", async () => {
  $("pr-save").disabled = true;
  notice("pr-msg", "Проверяю настройки в Xray и применяю…");
  try {
    const d = await api("api/protocols", protoData.settings);
    renderProtocols(d);
    notice("pr-msg", d.warning || "Сохранено, Xray перезапущен с новыми настройками.", !!d.warning);
  } catch (err) {
    notice("pr-msg", err.message, true);
  } finally {
    $("pr-save").disabled = false;
  }
});

// ---- Scanner ----
let scanFor = "";

async function runScan() {
  const form = $("scan-form");
  $("scan-run").disabled = true;
  notice("scan-msg", "Сканирую, это до 45 секунд…");
  $("scan-table").hidden = true;
  try {
    const d = await api("api/protocols/scan", { target: form.target.value.trim() });
    renderScan(d);
  } catch (err) {
    notice("scan-msg", err.message, true);
  } finally {
    $("scan-run").disabled = false;
  }
}

function useSNI(proto, domain) {
  const r = protoData.settings[proto].reality;
  r.sni = domain;
  r.target = "";
  renderProtocols(protoData);
  $("proto-" + proto).scrollIntoView({ behavior: "smooth", block: "start" });
  dirty();
}

function renderScan(d) {
  const t = $("scan-table");
  t.replaceChildren(el("tr", {}, el("th", { text: "Сайт" }), el("th", { text: "Адрес" }), el("th", { text: "Проверки" }), el("th", { text: "Задержка" }), el("th", { text: "" })));
  const good = d.results.filter((p) => p.good).length;
  notice("scan-msg", d.results.length
    ? `${d.target}: найдено сайтов ${d.results.length}, подходят ${good}.` + (scanFor ? ` Нажмите «${scanFor === "grpc" ? "gRPC" : "XHTTP"}», чтобы взять сайт.` : "")
    : `${d.target}: рядом нет сайтов на порту 443. Попробуйте другую подсеть или домен.`, !d.results.length);
  for (const p of d.results.slice(0, 60)) {
    const checks = el("span", { class: "marks" });
    for (const [name, ok] of marks(p)) checks.append(el("span", { class: ok ? "mark yes" : "mark no", text: (ok ? "✓ " : "✗ ") + name }));
    const use = el("span", { class: "row" });
    for (const [proto, label] of [["grpc", "gRPC"], ["xhttp", "XHTTP"]]) {
      use.append(el("button", { type: "button", class: (scanFor === proto ? "" : "ghost ") + "small", text: label, onclick: () => useSNI(proto, p.domain) }));
    }
    t.append(el("tr", { class: p.good ? "" : "dim" },
      el("td", {}, el("b", { text: p.domain })), el("td", {}, el("code", { text: p.addr })),
      el("td", {}, checks), el("td", { text: p.latency_ms + " мс" }), el("td", {}, use)));
  }
  t.hidden = !d.results.length;
}

$("scan-form").addEventListener("submit", (e) => {
  e.preventDefault();
  scanFor = "";
  runScan();
});
