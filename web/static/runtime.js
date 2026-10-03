/* 优选台运行时：$、api、toast、busy。零依赖。 */
const $ = (id) => document.getElementById(id);

let toastTimer;
const toast = (msg) => {
  const el = $("toast");
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 3200);
};

async function api(path, opts = {}) {
  const res = await fetch(path, { headers: { "Content-Type": "application/json" }, ...opts });
  if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
  return res.json();
}

/* busy：按钮转圈并禁用，任务完解除。重复点击天然防抖。 */
async function busy(button, label, task) {
  if (button.disabled) return;
  const text = $(label);
  const prev = text.textContent;
  button.disabled = true;
  button.classList.add("busy");
  text.textContent = "进行中…";
  try {
    return await task();
  } finally {
    button.disabled = false;
    button.classList.remove("busy");
    text.textContent = prev;
  }
}

const fmtTime = (iso) => {
  if (!iso || String(iso).startsWith("0001-")) return "--";
  const d = new Date(iso);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
};
