"use strict";
const form = document.getElementById("login");
const errorBox = document.getElementById("error");

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  errorBox.hidden = true;
  const data = Object.fromEntries(new FormData(form));
  try {
    const res = await fetch("api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data),
      credentials: "same-origin",
    });
    if (res.ok) {
      location.reload();
      return;
    }
    const body = await res.json().catch(() => ({}));
    errorBox.textContent = body.error || "Не удалось войти";
  } catch {
    errorBox.textContent = "Нет связи с панелью. Проверьте SSH-туннель.";
  }
  errorBox.hidden = false;
});
