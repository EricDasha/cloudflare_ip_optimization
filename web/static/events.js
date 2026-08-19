$("startCfnat").addEventListener("click", async () => {
  try { await withButton($("startCfnat"), "正在重启", restartCFnat); toast("CFnat 已启动"); }
  catch (e) { toast(`启动失败：${e.message}`); }
});

$("stopCfnat").addEventListener("click", async () => {
  if (!window.confirm("停止 CFnat 转发？")) return;
  try {
    await withButton($("stopCfnat"), "正在停止", () => api("/api/cfnat/stop", { method: "POST", body: "{}" }));
    toast("CFnat 停止指令已下发");
    await refreshAll();
  } catch (e) { toast(`停止失败：${e.message}`); }
});

$("runProxyScan").addEventListener("click", async () => {
  const button = $("runProxyScan");
  try {
    const data = await withButton(button, "正在扫描", () => api("/api/cfnat/proxy-scan", { method: "POST", body: JSON.stringify(proxyScanPayload()) }));
    renderProxyScanResults(data);
    toast("反代 IP 扫描完成");
  } catch (e) { toast(`扫描失败：${e.message}`); }
});

document.querySelectorAll("[data-workspace]").forEach((tab) => {
  tab.addEventListener("click", () => activateWorkspace(tab.dataset.workspace));
});

$("proxyScanSources").addEventListener("change", updateProxySourceSummary);
$("selectAllProxySources").addEventListener("click", () => {
  $("proxyScanSources").querySelectorAll("input").forEach((input) => { input.checked = true; });
  updateProxySourceSummary();
});
$("clearProxySources").addEventListener("click", () => {
  $("proxyScanSources").querySelectorAll("input").forEach((input) => { input.checked = false; });
  updateProxySourceSummary();
});

$("backgroundOptimizerEnabled").addEventListener("change", async (event) => {
  const input = event.currentTarget;
  input.disabled = true;
  try {
    const status = await api("/api/cfnat/background-optimizer", {
      method: "POST",
      body: JSON.stringify({ enabled: input.checked }),
    });
    renderBackgroundOptimizer(status);
    toast(input.checked ? "后台慢速优选已开启" : "后台慢速优选已关闭");
  } catch (error) {
    input.checked = !input.checked;
    toast(`设置失败：${error.message}`);
  } finally {
    input.disabled = false;
  }
});

$("useProxyScanResults").addEventListener("click", () => {
  const ips = latestProxyScanResults.filter((r) => !r.error).map((r) => r.ip);
  if (!ips.length) { toast("没有可采用的通过 IP"); return; }
  $("natFixedIPs").value = ips.join(",");
  $("cfnatAdvanced").open = false;
  toast(`已填入 ${ips.length} 个固定转发 IP`);
});

$("refreshAutoCandidates").addEventListener("click", async () => {
  const button = $("refreshAutoCandidates");
  try {
    const snapshot = await withButton(button, "正在终审", () => api("/api/cfnat/proxy-candidates", { method: "POST", body: "{}" }));
    renderAutoCandidates(snapshot, false);
    toast(`已拉取 ${snapshot.ips?.length || 0} 个候选 IP`);
  } catch (e) { toast(`候选拉取失败：${e.message}`); }
});

$("loadAutoCandidates").addEventListener("click", async () => {
  try {
    const snapshot = await loadAutoCandidates(true);
    toast(`已载入 ${snapshot.ips?.length || 0} 个候选 IP`);
  } catch (e) { toast(`候选载入失败：${e.message}`); }
});

$("runCfdata").addEventListener("click", async () => {
  try { await withButton($("runCfdata"), "正在启动", runCFdata); toast("CFdata 已开扫"); }
  catch (e) { toast(`运行失败：${e.message}`); }
});

$("stopCfdata").addEventListener("click", async () => {
  if (!window.confirm("停止当前 CFdata 扫描？")) return;
  try {
    await withButton($("stopCfdata"), "正在停止", () => api("/api/cfdata/stop", { method: "POST", body: "{}" }));
    toast("CFdata 停止指令已下发");
    await refreshAll();
  } catch (e) { toast(`停止失败：${e.message}`); }
});

$("runFullOptimization").addEventListener("click", async () => {
  try { await withButton($("runFullOptimization"), "优选执行中", runFullOptimizationFlow); }
  catch (_) {}
});

$("quickRestartCfnat").addEventListener("click", async () => {
  try { await withButton($("quickRestartCfnat"), "正在重启", restartCFnat); toast("CFnat 已重启"); }
  catch (e) { toast(`重启失败：${e.message}`); }
});

$("quickRunCfdata").addEventListener("click", async () => {
  try { await withButton($("quickRunCfdata"), "正在启动", runCFdata); toast("CFdata 已开扫"); }
  catch (e) { toast(`运行失败：${e.message}`); }
});

$("refreshBtn").addEventListener("click", () => withButton($("refreshBtn"), "", refreshAll));
$("refreshBtnCfdata").addEventListener("click", () => {
  refreshStatus();
  refreshCfdataResults();
  refreshLogs();
});
$("refreshFiles").addEventListener("click", refreshFiles);
$("logTarget").addEventListener("change", refreshLogs);
$("logQuery").addEventListener("input", renderLogs);
$("logLevel").addEventListener("change", renderLogs);
$("logLines").addEventListener("change", refreshLogs);
$("logPaused").addEventListener("change", () => {
  $("logStateText").textContent = $("logPaused").checked ? "已暂停" : "实时";
  if (!$("logPaused").checked) refreshLogs();
});
$("logAutoscroll").addEventListener("change", renderLogs);
$("copyLogs").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(filteredLogLines().join("\n"));
    toast("日志已复制");
  } catch (e) { toast(`复制失败：${e.message}`); }
});
$("refreshCfdataResults").addEventListener("click", refreshCfdataResults);
$("detailFileSelect").addEventListener("change", refreshDetailResults);
$("toggleLiveExpand").addEventListener("click", () => {
  showAllLive = !showAllLive;
  renderLiveAndScanTables();
});
$("toggleScanExpand").addEventListener("click", () => {
  showAllScan = !showAllScan;
  renderLiveAndScanTables();
});

document.querySelectorAll(".tab").forEach((tab) => {
  tab.addEventListener("click", () => {
    activatePanel(tab.dataset.panel);
  });
});

$("dcTableBody").addEventListener("click", async (event) => {
  const btn = event.target.closest("[data-test-dc]");
  if (!btn) return;
  try {
    await runDataCenterDetail(btn.dataset.testDc);
  } catch (e) {
    toast(`详细测试启动失败：${e.message}`);
  }
});

$("detailTableBody").addEventListener("click", (event) => {
  const btn = event.target.closest("[data-speed-ip]");
  if (!btn) return;
  speedTestIP(btn.dataset.speedIp, btn);
});

document.querySelectorAll("[data-route]").forEach((link) => {
  link.addEventListener("click", (event) => {
    event.preventDefault();
    const next = link.dataset.route || "/";
    history.pushState({}, "", next);
    applyRoute(next);
  });
});

window.addEventListener("popstate", () => applyRoute());

loadDefaults().then(() => {
  updateProxySourceSummary();
  const workspaceFromHash = window.location.hash === "#candidates" ? "candidateWorkspace" : null;
  activateWorkspace(workspaceFromHash || sessionStorage.getItem("cfnatWorkspace") || "forwardWorkspace");
  applyRoute();
  refreshStatus();
  refreshFiles();
  refreshCfdataResults();
  refreshLogs();
  loadAutoCandidates(false).catch((e) => toast(`自动候选载入失败：${e.message}`));
});
setInterval(refreshStatus, 3000);
setInterval(refreshLogs, 2500);
setInterval(refreshFiles, 12000);
setInterval(refreshCfdataResults, 12000);
