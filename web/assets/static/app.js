"use strict";
const ROLES = { main: "Основной сервер", router: "Точка маршрутизации" };

async function load() {
  const res = await fetch("api/me", { credentials: "same-origin" });
  if (res.status === 401) {
    location.reload();
    return;
  }
  const me = await res.json();
  document.getElementById("who").textContent = me.username;
  document.getElementById("role").textContent = ROLES[me.role] || me.role;
  document.getElementById("domain").textContent = me.domain || "не задан";
  document.getElementById("version").textContent = me.version;
}

document.getElementById("logout").addEventListener("click", async () => {
  await fetch("api/logout", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
    credentials: "same-origin",
  });
  location.reload();
});

load();
