const $ = (id) => document.getElementById(id);

let toastTimer;
const toast = (msg) => {
  const el = $("toast");
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 3200);
};

let latestCfdataResults = { liveRows: [], scanRows: [], dataCenters: [], detailFiles: [] };
let showAllLive = false;
let showAllScan = false;
let latestLogLines = [];
let fullOptimizationRunning = false;
const routes = new Set(["/", "/cfnat", "/cfdata", "/files"]);
const routeMeta = {
  "/": ["CONTROL PLANE", "运行总览"],
  "/cfnat": ["CFNAT", "转发与候选池"],
  "/cfdata": ["CFDATA", "扫描与测速"],
  "/files": ["ARTIFACTS", "数据文件"],
};

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
  return res.json();
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function withButton(button, pendingLabel, task) {
  if (button.disabled) return;
  const previous = button.textContent;
  button.disabled = true;
  button.classList.add("is-busy");
  button.textContent = pendingLabel;
  try {
    return await task();
  } finally {
    button.disabled = false;
    button.classList.remove("is-busy");
    button.textContent = previous;
  }
}

function setOptimizationStage(label, progress) {
  $("optimizationStage").textContent = label;
  $("optimizationProgress").style.width = `${Math.max(0, Math.min(100, progress))}%`;
}

function number(id) {
  return Number($(id).value || 0);
}
