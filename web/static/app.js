function cfnatPayload() {
  return {
    addr: $("natAddr").value.trim(),
    code: number("natCode"),
    colo: $("natColo").value.trim(),
    delay: number("natDelay"),
    domain: $("natDomain").value.trim(),
    fixed: $("natFixedIPs").value.trim(),
    ipnum: number("natIPNum"),
    ips: $("natIPs").value,
    num: number("natNum"),
    port: number("natPort"),
    random: $("natRandom").checked,
    task: number("natTask"),
    tls: $("natTLS").checked,
  };
}

function proxyScanPayload() {
  return {
    ips: $("proxyScanIPs").value,
    subscription: $("proxyScanSubscription").value,
    sources: Array.from($("proxyScanSources").selectedOptions, (option) => option.value),
    host: $("proxyScanHost").value.trim(),
    port: number("proxyScanPort"),
    concurrency: number("proxyScanConcurrency"),
    maxLatency: number("proxyScanLatency"),
    limit: number("proxyScanLimit"),
    tls: $("proxyScanTLS").checked,
  };
}

let latestProxyScanResults = [];
let autoCandidateSnapshot = { ips: [], errors: [] };

function renderAutoCandidates(snapshot, loadIntoInput = false) {
  autoCandidateSnapshot = snapshot || { ips: [], errors: [] };
  const updated = snapshot.updatedAt ? new Date(snapshot.updatedAt).toLocaleString() : "尚未刷新";
  const next = snapshot.nextRefresh ? new Date(snapshot.nextRefresh).toLocaleString() : "--";
  const active = snapshot.active || {};
  const activeCount = active.ips?.length || 0;
  const vlessPassed = active.results?.filter((result) => result.stage === "VLESS_PASS").length || 0;
  const activeText = activeCount ? `生效 ${activeCount} 个${vlessPassed ? ` · VLESS 已验 ${vlessPassed} 个` : ""}` : "尚无生效池";
  $("autoCandidateStatus").textContent = `${snapshot.ips?.length || 0} 个候选 · ${activeText} · 更新 ${updated}${active.error ? ` · ${active.error}` : ""}`;
  $("candidateCount").textContent = snapshot.ips?.length || 0;
  $("activePoolCount").textContent = activeCount;
  $("nextRefresh").textContent = next;
  $("activePoolIPs").textContent = active.ips?.length ? active.ips.join("  ·  ") : "尚无自动生效池";
  if (loadIntoInput && snapshot.ips?.length) {
    $("proxyScanIPs").value = snapshot.ips.join("\n");
  }
}

async function loadAutoCandidates(loadIntoInput = false) {
  const snapshot = await api("/api/cfnat/proxy-candidates");
  renderAutoCandidates(snapshot, loadIntoInput);
  return snapshot;
}

function renderProxyScanResults(data) {
  latestProxyScanResults = data.results || [];
  const passed = latestProxyScanResults.filter((r) => !r.error);
  const sourceErrors = data.sourceErrors || [];
  $("proxyScanSummary").textContent = `已扫描 ${data.scanned} 个，${passed.length} 个通过延迟筛选${sourceErrors.length ? `；${sourceErrors.length} 个候选源解析失败` : ""}`;
  $("proxyScanResults").textContent = latestProxyScanResults.length
    ? [...sourceErrors.map((error) => `SOURCE  ${error}`), ...latestProxyScanResults.map((r) => `${r.error ? "FAIL" : "PASS"}  ${r.ip.padEnd(16)} ${String(r.latency).padStart(5)} ms${r.error ? `  ${r.error}` : ""}`)].join("\n")
    : "没有结果";
}

function cfdataPayload() {
  return {
    forceUpdate: $("dataForce").checked,
    ipType: number("dataIPType"),
    dataCenter: $("dataCenter").value.trim(),
    scan: number("dataScan"),
    test: number("dataTest"),
    port: number("dataPort"),
    delay: number("dataDelay"),
  };
}

function chip(el, status, label) {
  el.textContent = `${label}: ${status.running ? "运行中" : "已停止"}`;
  el.classList.toggle("ok", status.running);
  el.classList.toggle("bad", !status.running);
}

function activatePanel(panelId) {
  document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.panel === panelId));
  document.querySelectorAll(".result-panel").forEach((p) => p.classList.toggle("active", p.id === panelId));
}

function normalizeRoute(path = window.location.pathname) {
  return routes.has(path) ? path : "/";
}

function applyRoute(path = normalizeRoute()) {
  document.querySelectorAll(".route-page").forEach((page) => {
    page.classList.toggle("active", page.dataset.page === path);
  });
  document.querySelectorAll("[data-route]").forEach((link) => {
    link.classList.toggle("active", link.dataset.route === path);
  });
  const meta = routeMeta[path] || routeMeta["/"];
  $("pageOverline").textContent = meta[0];
  $("pageTitle").textContent = meta[1];

  if (path === "/cfdata") {
    $("logTarget").value = "cfdata";
    refreshCfdataResults().catch((e) => toast(`结果刷新失败：${e.message}`));
    refreshLogs().catch((e) => toast(`日志刷新失败：${e.message}`));
    return;
  }
  if (path === "/files") {
    refreshFiles().catch((e) => toast(`文件刷新失败：${e.message}`));
    return;
  }
  if (path === "/") {
    refreshAll();
  }
}

async function refreshStatus() {
  const st = await api("/api/status");
  chip($("cfnatChip"), st.cfnat, "CFnat");
  chip($("cfdataChip"), st.cfdata, "CFdata");
  $("cfnatPid").textContent = st.cfnat.pid || "--";
  $("cfdataPid").textContent = st.cfdata.pid || "--";
  $("healthDot").className = "health-dot ok";
  $("sidebarHealth").textContent = "服务正常";
  if (st.proxyAuto) {
    $("activePoolCount").textContent = st.proxyAuto.ips?.length || 0;
    $("activePoolIPs").textContent = st.proxyAuto.ips?.length ? st.proxyAuto.ips.join("  ·  ") : "尚无自动生效池";
  }
  const cfdataStatus = $("cfdataStatusText");
  if (cfdataStatus) {
    cfdataStatus.textContent = st.cfdata.running ? "CFdata 运行中" : "CFdata 已停止";
  }
  return st;
}

function filteredLogLines() {
  const query = $("logQuery").value.trim().toLowerCase();
  const level = $("logLevel").value;
  return latestLogLines.filter((line) => {
    const text = String(line || "");
    const lower = text.toLowerCase();
    if (query && !lower.includes(query)) return false;
    if (level === "error") return /error|fail|failed|timeout|denied|失败|错误|超时/i.test(text);
    if (level === "success") return /pass|success|completed|applied|verified|101|完成|成功|通过/i.test(text);
    return true;
  });
}

function renderLogs() {
  const box = $("logBox");
  const lines = filteredLogLines();
  box.replaceChildren();
  if (!lines.length) {
    const empty = document.createElement("span");
    empty.className = "log-empty";
    empty.textContent = "没有匹配日志";
    box.appendChild(empty);
    return;
  }
  const frag = document.createDocumentFragment();
  for (const line of lines) {
    const text = String(line || "");
    const kind = /error|fail|failed|timeout|denied|失败|错误|超时/i.test(text)
      ? "error"
      : /pass|success|completed|applied|verified|101|完成|成功|通过/i.test(text)
        ? "success"
        : /GET \/api\/health/i.test(text) ? "muted" : "";
    const row = document.createElement("span");
    row.className = `log-line ${kind}`;
    row.textContent = text;
    frag.appendChild(row);
  }
  box.appendChild(frag);
  if ($("logAutoscroll").checked) box.scrollTop = box.scrollHeight;
}

async function refreshLogs() {
  if ($("logPaused").checked) return;
  const target = $("logTarget").value;
  const limit = Number($("logLines").value || 320);
  const data = await api(`/api/logs?target=${encodeURIComponent(target)}&lines=${limit}`);
  latestLogLines = data.lines || [];
  renderLogs();
  if (target === "cfdata") updateCfdataProgress(latestLogLines.join("\n"));
}

function fmtSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[c]));
}

function latencyClass(ms) {
  if (!ms || Number.isNaN(ms)) return "";
  if (ms < 100) return "lat-good";
  if (ms < 200) return "lat-mid";
  return "lat-bad";
}

function setRows(tbody, rows, render, emptyText) {
  const el = $(tbody);
  el.replaceChildren();
  if (!rows?.length) {
    const tr = document.createElement("tr");
    const td = document.createElement("td");
    td.colSpan = 8;
    td.className = "inline-status";
    td.textContent = emptyText;
    tr.appendChild(td);
    el.appendChild(tr);
    return;
  }
  const frag = document.createDocumentFragment();
  for (const row of rows) {
    const markup = `<table><tbody><tr>${render(row)}</tr></tbody></table>`;
    const parsed = new DOMParser().parseFromString(markup, "text/html");
    frag.appendChild(parsed.querySelector("tr"));
  }
  el.appendChild(frag);
}

function renderLocationCells(r) {
  return `
    <td><strong>${esc(r.dataCenter)}</strong></td>
    <td>${esc(r.dataCenterName || r.dataCenterZh || r.cityZh || r.dataCenter)}</td>
    <td>${esc(r.regionZh || r.region)}<span class="sub">${esc(r.regionEn && r.regionEn !== r.regionZh ? r.regionEn : r.region)}</span></td>
    <td>${esc(r.cityZh || r.city)}<span class="sub">${esc(r.cityEn && r.cityEn !== r.cityZh ? r.cityEn : r.city)}</span></td>
  `;
}

function updateCfdataProgress(text) {
  const label = $("cfdataProgressText");
  const fill = $("cfdataProgressFill");
  if (!label || !fill) return;
  const matches = [...String(text || "").matchAll(/详细测试进度:\s*(\d+)\/(\d+)\s*\(([\d.]+)%\)/g)];
  if (matches.length) {
    const last = matches[matches.length - 1];
    const done = Number(last[1]);
    const total = Number(last[2]);
    const pct = Math.max(0, Math.min(100, Number(last[3])));
    label.textContent = done >= total ? `详细测试完成 · ${done}/${total}` : `详细测试中 · ${done}/${total}`;
    fill.style.width = `${pct}%`;
    return;
  }
  if (String(text || "").includes("详细测试结束")) {
    label.textContent = "详细测试完成";
    fill.style.width = "100%";
    return;
  }
  label.textContent = "等待详细测试";
  fill.style.width = "0%";
}

function renderLiveAndScanTables() {
  const liveRows = latestCfdataResults.liveRows || [];
  const scanRows = latestCfdataResults.scanRows || [];
  const liveShown = showAllLive ? liveRows : liveRows.slice(-120);
  const scanShown = showAllScan ? scanRows : scanRows.slice(0, 300);
  $("liveCount").textContent = liveRows.length;
  $("scanCount").textContent = scanRows.length;
  $("toggleLiveExpand").textContent = showAllLive ? "收起" : `展开全部 (${liveRows.length})`;
  $("toggleScanExpand").textContent = showAllScan ? "收起" : `展开全部 (${scanRows.length})`;

  setRows("liveTableBody", liveShown, (r) => `
    <td class="mono">${esc(r.ip)}</td>
    ${renderLocationCells(r)}
    <td class="${latencyClass(r.latencyMs)}">${esc(r.latency || `${r.latencyMs} ms`)}</td>
  `, "暂无实时扫描结果");

  setRows("scanTableBody", scanShown, (r) => `
    <td class="mono">${esc(r.ip)}</td>
    ${renderLocationCells(r)}
    <td class="${latencyClass(r.latencyMs)}">${esc(r.latency)}</td>
  `, "暂无扫描结果");
}

async function refreshCfdataResults() {
  const data = await api("/api/cfdata/results");
  latestCfdataResults = data;
  const source = data.ipListSource || {};
  $("ipSourceHint").textContent = (source.cacheFiles || []).join(" · ") || "IP 来源未就绪";

  const dcRows = data.dataCenters || [];
  $("dcCount").textContent = dcRows.length;

  setRows("dcTableBody", dcRows, (r) => `
    <td><strong>${esc(r.dataCenter)}</strong><span class="sub">${esc(r.dataCenterZh)}</span></td>
    <td>${esc(r.dataCenterName || r.cityZh)}</td>
    <td>${esc(r.regionZh || r.region)}<span class="sub">${esc(r.regionEn || r.region)}</span></td>
    <td>${esc(r.cityZh || r.city)}<span class="sub">${esc(r.cityEn || r.city)}</span></td>
    <td>${esc(r.ipCount)}</td>
    <td class="${latencyClass(r.minLatencyMs)}">${esc(r.minLatencyMs)} ms</td>
    <td><button class="row-action" data-test-dc="${esc(r.dataCenter)}">选择测试</button></td>
  `, "暂无数据中心结果");

  renderLiveAndScanTables();

  const select = $("detailFileSelect");
  const previous = select.value;
  select.replaceChildren();
  for (const file of data.detailFiles || []) {
    const opt = document.createElement("option");
    opt.value = file.name;
    opt.textContent = `${file.name} (${file.rows})`;
    select.appendChild(opt);
  }
  select.disabled = !select.options.length;
  if ([...select.options].some((o) => o.value === previous)) select.value = previous;
  await refreshDetailResults();
}

async function refreshDetailResults() {
  const file = $("detailFileSelect").value;
  const data = await api(`/api/cfdata/detail${file ? `?file=${encodeURIComponent(file)}` : ""}`);
  const rows = data.rows || [];
  $("detailCount").textContent = rows.length;
  setRows("detailTableBody", rows, (r) => `
    <td class="mono">${esc(r.ip)}</td>
    <td class="${latencyClass(r.minLatencyMs)}">${esc(r.minLatencyMs)} ms</td>
    <td class="${latencyClass(r.maxLatencyMs)}">${esc(r.maxLatencyMs)} ms</td>
    <td class="${latencyClass(r.avgLatencyMs)}">${esc(r.avgLatencyMs)} ms</td>
    <td class="${r.lossRate === 0 ? "lat-good" : r.lossRate < 50 ? "lat-mid" : "lat-bad"}">${esc(r.lossRate)}%</td>
    <td><button class="row-action secondary" data-speed-ip="${esc(r.ip)}">测速</button><span class="speed-result" id="speed-${esc(r.ip).replaceAll(".", "-").replaceAll(":", "-")}"></span></td>
  `, "暂无详细测试结果");
}

async function runDataCenterDetail(dc) {
  $("dataCenter").value = dc;
  $("dataForce").checked = false;
  activatePanel("detailPanel");
  $("cfdataProgressText").textContent = `准备测试 ${dc}`;
  $("cfdataProgressFill").style.width = "0%";
  await api("/api/cfdata/run", { method: "POST", body: JSON.stringify(cfdataPayload()) });
  toast(`${dc} 详细测试已启动`);
  refreshAll();
}

async function speedTestIP(ip, button) {
  button.disabled = true;
  const old = button.textContent;
  button.textContent = "测速中";
  const resultId = `speed-${ip.replaceAll(".", "-").replaceAll(":", "-")}`;
  const resultEl = $(resultId);
  if (resultEl) resultEl.textContent = "";
  try {
    const data = await api(`/api/cfdata/speed?ip=${encodeURIComponent(ip)}&bytes=2000000`);
    if (resultEl) resultEl.textContent = `${data.mbps.toFixed(2)} Mbps`;
    toast(`${ip} 下载测速完成`);
  } catch (e) {
    if (resultEl) resultEl.textContent = "失败";
    toast(`测速失败：${e.message}`);
  } finally {
    button.disabled = false;
    button.textContent = old;
  }
}

async function refreshFiles() {
  const data = await api("/api/files");
  const dl = $("dataCenters");
  dl.replaceChildren();
  for (const dc of data.dataCenters || []) {
    const opt = document.createElement("option");
    opt.value = dc;
    dl.appendChild(opt);
  }

  const list = $("fileList");
  list.replaceChildren();
  if (!data.files?.length) {
    const empty = document.createElement("p");
    empty.className = "inline-status";
    empty.textContent = "暂无输出文件";
    list.appendChild(empty);
    return;
  }
  for (const f of data.files) {
    const row = document.createElement("div");
    row.className = "file-row";
    const identity = document.createElement("div");
    const link = document.createElement("a");
    link.href = `/api/file/${encodeURIComponent(f.name)}`;
    link.target = "_blank";
    link.rel = "noopener";
    link.textContent = f.name;
    const modTime = document.createElement("div");
    modTime.className = "file-meta";
    modTime.textContent = new Date(f.modTime).toLocaleString();
    identity.append(link, modTime);
    const kind = document.createElement("span");
    kind.className = "file-meta";
    kind.textContent = String(f.kind || "").toUpperCase();
    const size = document.createElement("span");
    size.className = "file-meta";
    size.textContent = fmtSize(f.size);
    row.append(identity, kind, size);
    list.appendChild(row);
  }
}

async function loadDefaults() {
  const cfg = await api("/api/config");
  const n = cfg.cfnat;
  $("natAddr").value = n.addr;
  $("natColo").value = n.colo;
  $("natPort").value = n.port;
  $("natCode").value = n.code;
  $("natDomain").value = n.domain;
  $("natFixedIPs").value = n.fixed || "";
  $("natDelay").value = n.delay;
  $("natIPNum").value = n.ipnum;
  $("natNum").value = n.num;
  $("natTask").value = n.task;
  $("natIPs").value = n.ips;
  $("natTLS").checked = n.tls;
  $("natRandom").checked = n.random;
}

async function refreshAll() {
  await Promise.allSettled([refreshStatus(), refreshLogs(), refreshFiles(), refreshCfdataResults()]);
}

async function runFullOptimizationFlow() {
  if (fullOptimizationRunning) return;
  fullOptimizationRunning = true;
  $("logTarget").value = "cfdata";
  $("logPaused").checked = false;
  try {
    setOptimizationStage("启动 CFdata", 10);
    let status = await refreshStatus();
    if (!status.cfdata.running) {
      await api("/api/cfdata/run", { method: "POST", body: JSON.stringify(cfdataPayload()) });
    }

    const deadline = Date.now() + 12 * 60 * 1000;
    while (Date.now() < deadline) {
      await sleep(2000);
      status = await refreshStatus();
      await refreshLogs();
      if (!status.cfdata.running) break;
      setOptimizationStage("CFdata 扫描中", 35);
    }
    if (status.cfdata.running) throw new Error("CFdata 扫描超过 12 分钟");
    if (status.cfdata.exitCode !== 0) throw new Error(status.cfdata.lastError || `CFdata exit ${status.cfdata.exitCode}`);

    setOptimizationStage("汇合候选", 58);
    $("logTarget").value = "cfnat";
    const snapshot = await api("/api/cfnat/proxy-candidates", { method: "POST", body: "{}" });
    await refreshStatus();
    renderAutoCandidates(snapshot, false);
    await refreshLogs();

    if (snapshot.active?.error) {
      setOptimizationStage("保留旧池", 100);
      toast(snapshot.active.error);
      return;
    }
    setOptimizationStage(`已应用 ${snapshot.active?.ips?.length || 0} 个 IP`, 100);
    toast("完整优选已完成");
  } catch (error) {
    setOptimizationStage("执行失败", 100);
    toast(`完整优选失败：${error.message}`);
    throw error;
  } finally {
    fullOptimizationRunning = false;
  }
}

async function restartCFnat() {
  await api("/api/cfnat/start", { method: "POST", body: JSON.stringify(cfnatPayload()) });
  await refreshAll();
}

async function runCFdata() {
  await api("/api/cfdata/run", { method: "POST", body: JSON.stringify(cfdataPayload()) });
  $("logTarget").value = "cfdata";
  await refreshAll();
}
