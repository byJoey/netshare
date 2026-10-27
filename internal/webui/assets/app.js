"use strict";

(() => {
  const $ = (id) => document.getElementById(id);
  const state = {
    session: null, overview: null, view: "mappings", editorID: null,
    editorOpen: false, editorInitial: "", mutationBusy: false,
    refreshPromise: null, updatedAt: null, timer: null, confirmResolve: null,
    probes: new Map(), passwordBusy: false,
  };
  const labels = {
    mappings: ["端口映射", "让一台机器的公网入口，连接另一台的私网服务。"],
    nodes: ["机器状态", "确认代理在线、私网地址和现有端口。"],
    events: ["操作记录", "每次变更都有结果，失败也能找到原因。"],
  };

  function el(tag, className, text) {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined && text !== null) element.textContent = String(text);
    return element;
  }
  function icon(name) {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "icon"); svg.setAttribute("aria-hidden", "true");
    const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", `#i-${name}`); svg.append(use); return svg;
  }
  function button(text, className, onClick) {
    const result = el("button", className, text); result.type = "button";
    if (onClick) result.addEventListener("click", onClick);
    return result;
  }
  function setText(id, text) { $(id).textContent = text; }
  function showError(id, message) { setText(id, message); $(id).hidden = !message; }
  function nodes() { return state.overview?.nodes || []; }
  function mappings() { return state.overview?.mappings || []; }
  function findNode(id) { return nodes().find((node) => node.id === id); }
  function nodeName(id) { return findNode(id)?.name || id || "未知机器"; }
  function protocolLabel(value) { return value === "both" ? "TCP + UDP" : String(value || "").toUpperCase(); }
  function formatTime(value, date = false) {
    const time = new Date(value);
    if (Number.isNaN(time.getTime()) || time.getUTCFullYear() < 2000) return "尚无记录";
    return new Intl.DateTimeFormat("zh-CN", date ? {month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",second:"2-digit",hour12:false} : {hour:"2-digit",minute:"2-digit",second:"2-digit",hour12:false}).format(time);
  }
  function formatBytes(value) {
    const bytes = Number(value) || 0;
    if (bytes < 1024) return `${bytes} B`;
    const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
    const position = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)) - 1, units.length - 1);
    return `${(bytes / (1024 ** (position + 1))).toFixed(2)} ${units[position]}`;
  }
  function formatUptime(value) {
    const seconds = Math.max(0, Number(value) || 0);
    if (seconds < 60) return `${Math.floor(seconds)} 秒`;
    if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
    if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时 ${Math.floor(seconds % 3600 / 60)} 分钟`;
    return `${Math.floor(seconds / 86400)} 天 ${Math.floor(seconds % 86400 / 3600)} 小时`;
  }

  async function api(path, {method = "GET", body, timeout} = {}) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeout ?? (method === "GET" ? 30000 : 120000));
    const headers = {"Accept":"application/json"};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (method !== "GET" && state.session?.csrf_token) headers["X-CSRF-Token"] = state.session.csrf_token;
    try {
      const response = await fetch(path, {method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials:"same-origin", cache:"no-store", signal:controller.signal});
      let result;
      try { result = await response.json(); } catch (_) { result = {}; }
      if (!response.ok) {
        if (response.status === 401 && path !== "/api/login" && path !== "/api/session") expireSession();
        throw new Error(result.error || `请求失败（HTTP ${response.status}）`);
      }
      return result;
    } catch (error) {
      if (error.name === "AbortError") throw new Error("请求超时，请检查机器状态后重试。");
      if (error instanceof TypeError) throw new Error("无法连接管理台，请检查网络后重试。");
      throw error;
    } finally { clearTimeout(timer); }
  }

  function toast(message, kind = "success") {
    const item = el("div", `toast ${kind === "error" ? "error" : ""}`);
    item.append(icon(kind === "error" ? "alert" : "check"), el("p", "", message));
    const close = button("", "icon-button", () => item.remove());
    close.setAttribute("aria-label", "关闭提示"); close.append(icon("close")); item.append(close);
    $("toasts").append(item);
    while ($("toasts").children.length > 3) $("toasts").firstElementChild.remove();
    setTimeout(() => item.remove(), kind === "error" ? 11000 : 6500);
  }

  function setAuthenticated(session) {
    state.session = session?.authenticated ? session : null;
    $("boot").hidden = true; $("login-view").hidden = !!state.session; $("app").hidden = !state.session;
    const version = session?.version || state.overview?.version;
    if (version) document.querySelectorAll("[data-version]").forEach((entry) => { entry.textContent = `NetShare ${version}`; });
    if (state.session) {
      setText("current-user", session.username || "管理员");
      if (!state.overview) {
        $("mapping-loading").hidden = false; $("mapping-table-wrap").hidden = true; $("mapping-empty").hidden = true;
        $("network-strip").replaceChildren(el("p","network-empty","正在读取机器状态…")); $("node-list").replaceChildren();
        $("event-rows").replaceChildren(); $("event-table-wrap").hidden = true; $("event-empty").hidden = true;
        setText("last-updated","正在获取状态…");
      }
      startPolling();
    } else {
      clearInterval(state.timer); state.timer = null;
      if ($("password-dialog").open) $("password-dialog").close();
      $("password-form").reset();
      if ($("confirm-dialog").open) resolveConfirm(false);
    }
  }
  function expireSession() {
    if (!state.session) return;
    setAuthenticated(null); state.overview = null; state.probes.clear();
    forceCloseEditor(); showError("login-error", "登录已过期，请重新登录。");
    $("login-password").value = ""; $("login-password").focus();
  }
  function startPolling() {
    clearInterval(state.timer);
    state.timer = setInterval(() => {
      if (state.session && $("auto-refresh").checked && !document.hidden && !state.mutationBusy) refreshData();
    }, 10000);
  }

  async function boot() {
    $("boot").hidden = false; $("boot-retry").hidden = true;
    $("boot").querySelector("p").textContent = "正在连接管理台…";
    try {
      const session = await api("/api/session"); setAuthenticated(session);
      if (state.session) await refreshData();
      else $("login-username").focus();
    } catch (error) {
      $("boot").querySelector("p").textContent = error.message;
      $("boot-retry").hidden = false;
    }
  }

  async function refreshData(force = false) {
    if (!state.session) return;
    if (state.refreshPromise) { await state.refreshPromise; if (!force || !state.session) return; }
    $("refresh").classList.add("refreshing"); $("refresh").disabled = true;
    const task = (async () => {
      try {
        const overview = await api("/api/overview");
        if (!state.session) return;
        state.overview = {
          ...overview, nodes: Array.isArray(overview.nodes) ? overview.nodes : [],
          mappings: Array.isArray(overview.mappings) ? overview.mappings : [],
          events: Array.isArray(overview.events) ? overview.events : [],
        };
        state.updatedAt = new Date(); setText("page-error-text", ""); $("page-error").hidden = true;
        renderOverview();
      } catch (error) {
        if (!state.session) return;
        showError("page-error-text", `${error.message}${state.overview ? " 当前显示上次获取的数据。" : ""}`); $("page-error").hidden = false;
        if (!state.overview) { $("mapping-loading").hidden = true; $("mapping-empty").hidden = true; }
      }
    })();
    state.refreshPromise = task;
    try { await task; } finally {
      if (state.refreshPromise === task) state.refreshPromise = null;
      $("refresh").classList.remove("refreshing"); $("refresh").disabled = false;
    }
  }

  function renderOverview() {
    const data = state.overview;
    setText("nav-count", data.mappings.length);
    setText("mapping-total", data.mappings.length);
    setText("node-total", data.nodes.length);
    setText("event-total", data.events.length);
    setText("last-updated", `更新于 ${formatTime(state.updatedAt)}`);
    setText("config-revision", `配置修订 ${data.revision ?? "—"}`);
    if (data.version) document.querySelectorAll("[data-version]").forEach((entry) => { entry.textContent = `NetShare ${data.version}`; });
    renderNetwork(); renderSourceFilter(); renderMappings(); renderNodes(); renderEvents();
    if (state.editorOpen) updatePreview();
  }

  function renderNetwork() {
    const strip = $("network-strip"); strip.replaceChildren();
    if (!nodes().length) { strip.append(el("p", "network-empty", "尚未接入机器。请先完成节点代理配置。")); return; }
    nodes().forEach((node, index) => {
      if (index > 0) {
        const link = el("div", "network-link"); const label = el("span", "network-link-label");
        label.append(icon("network"), document.createTextNode("私网")); link.append(label); strip.append(link);
      }
      const part = el("div", "network-node");
      const mark = el("span", "network-node-icon"); mark.append(icon("server"));
      const body = el("div", "network-node-body"); const name = el("div", "network-node-name");
      const dot = el("span", `status-dot ${node.online ? "online" : "offline"}`); dot.title = node.online ? "代理在线" : "代理离线";
      dot.setAttribute("aria-label", dot.title); name.append(el("span", "", node.name), dot);
      const addresses = el("div", "network-node-address");
      const publicIP = el("span"); publicIP.append(document.createTextNode("公网 "), el("code", "", node.public_ip));
      const privateIP = el("span"); privateIP.append(document.createTextNode("私网 "), el("code", "", node.private_ip));
      addresses.append(publicIP, privateIP); body.append(name, addresses); part.append(mark, body); strip.append(part);
    });
  }

  function renderSourceFilter() {
    const filter = $("source-filter"), value = filter.value;
    const expected = nodes().map((node) => node.id).join("|");
    if (filter.dataset.nodes === expected) return;
    filter.replaceChildren(new Option("全部入口", "")); nodes().forEach((node) => filter.add(new Option(node.name, node.id)));
    filter.value = value; filter.dataset.nodes = expected;
  }

  function mappingState(mapping) {
    const node = findNode(mapping.source_node);
    if (!node?.online || !node.status) return {kind:"offline", label:"入口离线", tone:"danger"};
    const applied = (node.status.rules || []).find((rule) => rule.id === mapping.id);
    if (!mapping.enabled) {
      return applied?.enabled ? {kind:"pending",label:"停用待同步",tone:"warning"} : {kind:"disabled",label:"已停用",tone:""};
    }
    const target = findNode(mapping.target_node);
    const matches = applied?.enabled && applied.protocol === mapping.protocol && samePorts(applied.listen_port,mapping.listen_port) && samePorts(applied.target_port,mapping.target_port) && applied.target_ip === target?.private_ip;
    if (!node.status.forwarding) return {kind:"pending",label:"转发未开启",tone:"warning"};
    return matches ? {kind:"applied",label:"已应用",tone:"success"} : {kind:"pending",label:"待同步",tone:"warning"};
  }

  function renderMappings() {
    const previousFocus = document.activeElement?.closest("[data-row-action]");
    const focusInfo = previousFocus ? [previousFocus.dataset.rowAction, previousFocus.dataset.mappingId] : null;
    const query = $("mapping-search").value.trim().toLowerCase(), source = $("source-filter").value, status = $("status-filter").value;
    const filtered = mappings().filter((mapping) => {
      const haystack = [mapping.name, nodeName(mapping.source_node), nodeName(mapping.target_node), findNode(mapping.source_node)?.public_ip, findNode(mapping.target_node)?.private_ip, mapping.listen_port, mapping.target_port, mapping.protocol].join(" ").toLowerCase();
      if (query && !haystack.includes(query)) return false;
      if (source && mapping.source_node !== source) return false;
      if (status === "enabled" && !mapping.enabled || status === "disabled" && mapping.enabled) return false;
      if (status === "pending" && mappingState(mapping).kind !== "pending") return false;
      return true;
    });
    const enabled = mappings().filter((mapping) => mapping.enabled).length;
    const pending = mappings().filter((mapping) => mappingState(mapping).kind === "pending").length;
    setText("mapping-summary", `${enabled} 条启用 · ${mappings().length - enabled} 条停用${pending ? ` · ${pending} 条待同步` : ""}`);
    $("mapping-loading").hidden = true;
    $("mapping-table-wrap").hidden = filtered.length === 0;
    $("mapping-empty").hidden = filtered.length !== 0;
    const isFiltered = query || source || status;
    setText("empty-title", isFiltered ? "没有匹配的映射" : "还没有映射");
    setText("empty-description", isFiltered ? "换个关键词，或清除筛选查看全部规则。" : "选择公网入口与私网目标，创建第一条端口映射。");
    $("empty-new").hidden = !!isFiltered; $("clear-filters").hidden = !isFiltered;
    const rows = $("mapping-rows"); rows.replaceChildren();
    filtered.forEach((mapping) => {
      const row = el("tr", `mapping-row${state.editorOpen && state.editorID === mapping.id ? " is-editing" : ""}`);
      const details = el("td"); const name = rowButton(mapping, "edit", mapping.name, "mapping-name", () => openEditor(mapping));
      const route = el("div", "mapping-route");
      route.append(routeEndpoint(`${nodeName(mapping.source_node)} · 公网`, findNode(mapping.source_node)?.public_ip, mapping.listen_port), icon("arrow"), routeEndpoint(`${nodeName(mapping.target_node)} · 私网`, findNode(mapping.target_node)?.private_ip, mapping.target_port));
      details.append(name, route);
      const probeResult = state.probes.get(mapping.id);
      if (probeResult) {
        const probe = el("p", `probe-result ${probeResult.reachable ? "success" : "failure"}`, probeResult.message);
        if (probeResult.interface) probe.append(document.createTextNode(` · ${probeResult.interface}`));
        details.append(probe);
      }
      const proto = el("td"); proto.append(el("span", "protocol", protocolLabel(mapping.protocol)));
      const statusCell = el("td", "status-cell"), statusWrap = el("div", "mapping-status");
      const result = mappingState(mapping), tag = el("span", `status-tag ${result.tone}`);
      tag.append(icon(result.kind === "applied" ? "check" : result.kind === "pending" || result.kind === "offline" ? "alert" : "close"), document.createTextNode(result.label));
      const toggle = rowButton(mapping, "toggle", "", "state-toggle", () => toggleMapping(mapping));
      toggle.setAttribute("role", "switch"); toggle.setAttribute("aria-checked", String(mapping.enabled));
      toggle.setAttribute("aria-label", `${mapping.enabled ? "停用" : "启用"} ${mapping.name}`);
      toggle.dataset.mutate = ""; toggle.disabled = state.mutationBusy || state.editorOpen && state.editorID === mapping.id;
      if (state.editorOpen && state.editorID === mapping.id) toggle.title = "正在编辑此映射，请在表单中保存启停状态。";
      toggle.append(el("span", "toggle-track"), el("span", "", mapping.enabled ? "已启用" : "已停用")); statusWrap.append(tag, toggle); statusCell.append(statusWrap);
      const actions = el("td"), group = el("div", "row-actions");
      const edit = rowButton(mapping, "edit-action", "编辑", "text-button", () => openEditor(mapping));
      const probe = rowButton(mapping, "probe", "检查目标", "text-button", () => probeMapping(mapping));
      probe.disabled = mapping.protocol === "udp" || !findNode(mapping.source_node)?.online || state.mutationBusy;
      probe.title = mapping.protocol === "udp" ? "UDP 无法通过握手确认，请用实际客户端测试。" : "从入口代理检查目标私网的 TCP 端口，不代表公网路径已验通。";
      group.append(edit, probe); actions.append(group); row.append(details, proto, statusCell, actions); rows.append(row);
    });
    if (focusInfo) rows.querySelectorAll("[data-row-action]").forEach((entry) => {
      if (entry.dataset.rowAction === focusInfo[0] && entry.dataset.mappingId === focusInfo[1]) entry.focus({preventScroll:true});
    });
  }
  function rowButton(mapping, action, text, className, handler) {
    const result = button(text, className, handler); result.dataset.mappingId = mapping.id; result.dataset.rowAction = action; return result;
  }
  function routeEndpoint(label, address, port) {
    const endpoint = el("div", "mapping-endpoint"); endpoint.append(el("span", "mapping-endpoint-label", label), el("code", "", `${address || "—"}:${port || "—"}`)); return endpoint;
  }

  function renderNodes() {
    const list = $("node-list");
    const opened = new Set(Array.from(list.querySelectorAll("details[open]")).map((entry) => entry.dataset.node));
    list.replaceChildren();
    if (!nodes().length) { const message = el("div", "empty-state"); message.append(el("h3", "", "尚未接入机器"), el("p", "", "完成节点代理配置后，机器状态会显示在这里。")); list.append(message); return; }
    nodes().forEach((node) => {
      const panel = el("article", "node-panel"), header = el("div", "node-heading"), title = el("div", "node-title");
      const names = el("div"); names.append(el("h3", "", node.name), el("p", "", node.id)); title.append(icon("server"), names);
      const headStatus = el("div", "node-head-status"), tag = el("span", `status-tag ${node.online ? "success" : "danger"}`);
      tag.append(el("span", `status-dot ${node.online ? "online" : "offline"}`), document.createTextNode(node.online ? "代理在线" : "代理离线")); headStatus.append(tag);
      if (node.online) headStatus.append(el("span", "mono", `${node.latency_ms ?? 0} ms`)); header.append(title, headStatus); panel.append(header);
      const body = el("div", "node-details"), facts = el("dl", "node-facts");
      appendFact(facts, "公网地址", node.public_ip, node.public_interface, true);
      appendFact(facts, "私网地址", node.private_ip, node.private_interface, true);
      appendFact(facts, "IPv4 转发", node.status ? node.status.forwarding ? "已开启" : "未开启" : "—");
      appendFact(facts, "已应用规则", node.status ? `${(node.status.rules || []).filter((rule) => rule.enabled).length} 条` : "—", node.status ? `修订 ${node.status.revision}` : "");
      if (node.status) {
        appendFact(facts, "代理运行", formatUptime(node.status.uptime_seconds), `v${node.status.version || "—"}`);
        appendFact(facts, "最近应用", formatTime(node.status.applied_at, true));
      }
      body.append(facts);
      if (!node.online) {
        const error = el("div", "notice error"); error.append(icon("alert"), el("p", "", node.error || "无法连接节点代理。请检查代理服务与私网连通性。")); body.append(error);
      }
      if (node.status?.warnings?.length) { const warnings = el("ul", "node-warnings"); node.status.warnings.forEach((warning) => warnings.append(el("li", "", warning))); body.append(warnings); }
      body.append(el("h4", "node-subheading", "保留端口"));
      const ports = el("div", "reserved-ports");
      [...new Set([22, ...(node.reserved_ports || [])])].sort((a,b) => a-b).forEach((port) => ports.append(el("span", "port-chip", port))); body.append(ports);
      if (node.status?.interfaces?.length) {
        body.append(el("h4", "node-subheading", "网卡累计字节"));
        const wrapper = el("div", "node-traffic"), table = makeTable(["网卡", "累计接收", "累计发送"]);
        node.status.interfaces.forEach((iface) => { const row = el("tr"); row.append(el("td", "mono", iface.name), el("td", "", formatBytes(iface.rx_bytes)), el("td", "", formatBytes(iface.tx_bytes))); table.tBodies[0].append(row); });
        wrapper.append(table); body.append(wrapper);
      }
      if (node.status) {
        const listeners = node.status.listeners || [], details = el("details", "node-listeners"); details.dataset.node = node.id; details.open = opened.has(node.id);
        details.append(el("summary", "", `查看监听端口 · ${listeners.length} 个`));
        if (!listeners.length) details.append(el("p", "node-message", "没有读取到监听端口。"));
        else {
          const wrapper = el("div", "listeners-wrap"), table = makeTable(["协议", "监听地址", "端口", "进程"], "listener-table");
          listeners.forEach((listener) => { const row = el("tr"); row.append(el("td", "", protocolLabel(listener.protocol)), el("td", "", listener.address), el("td", "", listener.port), el("td", "", listener.process || "—")); table.tBodies[0].append(row); }); wrapper.append(table); details.append(wrapper);
        }
        body.append(details);
      }
      panel.append(body); list.append(panel);
    });
  }
  function appendFact(parent, label, value, detail, mono = false) {
    const item = el("div"), description = el("dd");
    description.append(el(mono ? "code" : "span", "", value)); if (detail) description.append(el("small", "", detail)); item.append(el("dt", "", label), description); parent.append(item);
  }
  function makeTable(headings, className = "") {
    const table = el("table", className), header = el("thead"), row = el("tr");
    headings.forEach((heading) => { const cell = el("th", "", heading); cell.scope = "col"; row.append(cell); });
    header.append(row); table.append(header, el("tbody")); return table;
  }
  function renderEvents() {
    const query = $("event-search").value.trim().toLowerCase(), filter = $("event-filter").value;
    const events = (state.overview?.events || []).filter((entry) => {
      if (filter === "success" && !entry.success || filter === "failure" && entry.success) return false;
      return !query || `${entry.action} ${entry.detail}`.toLowerCase().includes(query);
    }).sort((a,b) => new Date(b.time)-new Date(a.time));
    const rows = $("event-rows"); rows.replaceChildren();
    $("event-empty").hidden = events.length !== 0; $("event-table-wrap").hidden = events.length === 0;
    const emptyTitle = $("event-empty").querySelector("h3"), emptyDescription = $("event-empty").querySelector("p");
    emptyTitle.textContent = query || filter ? "没有匹配的记录" : "暂无操作记录";
    emptyDescription.textContent = query || filter ? "试试其他关键词或结果筛选。" : "保存映射或同步规则后，结果会显示在这里。";
    events.forEach((entry) => { const row = el("tr"), outcome = el("td"), tag = el("span", `status-tag ${entry.success ? "success" : "danger"}`); tag.append(icon(entry.success ? "check" : "alert"), document.createTextNode(entry.success ? "成功" : "失败")); outcome.append(tag); row.append(el("td", "", formatTime(entry.time,true)), el("td", "", entry.action), el("td", "", entry.detail), outcome); rows.append(row); });
  }

  async function navigate(view) {
    if (!labels[view]) view = "mappings";
    if (view !== state.view && state.editorOpen && !await closeEditor()) { history.replaceState(null,"",`#${state.view}`); return; }
    state.view = view;
    Object.keys(labels).forEach((name) => { $("view-" + name).hidden = name !== view; });
    document.querySelectorAll("[data-view]").forEach((entry) => { const active = entry.dataset.view === view; entry.classList.toggle("active",active); if (active) entry.setAttribute("aria-current","page"); else entry.removeAttribute("aria-current"); });
    setText("page-title", labels[view][0]); setText("page-description", labels[view][1]); setText("topbar-location", labels[view][0]);
    document.title = `${labels[view][0]} · NetShare`;
    if (location.hash !== `#${view}`) history.replaceState(null,"",`#${view}`);
  }

  function draftFromForm() {
    return {name:$("mapping-name").value.trim(),source_node:$("mapping-source").value,target_node:$("mapping-target").value,protocol:$("mapping-protocol").value,listen_port:$("mapping-listen-port").value.trim(),target_port:$("mapping-target-port").value.trim(),enabled:$("mapping-enabled").checked};
  }
  function mappingPayload(mapping) {
    return {name:mapping.name,source_node:mapping.source_node,target_node:mapping.target_node,protocol:mapping.protocol,listen_port:mapping.listen_port,target_port:mapping.target_port,enabled:mapping.enabled};
  }
  async function openEditor(mapping = null, preset = null) {
    if (state.mutationBusy || !state.overview) return;
    if (state.editorOpen && !await closeEditor()) return;
    if (nodes().length < 2) { toast("至少需要两台已配置的机器才能建立映射。", "error"); return; }
    await navigate("mappings");
    state.editorOpen = true; state.editorID = mapping?.id || null;
    $("mapping-layout").classList.add("editor-open"); $("mapping-editor").hidden = false;
    setText("editor-title", mapping ? "编辑映射" : "新建映射"); setText("editor-mode", mapping ? "修改规则" : "创建规则");
    $("mapping-form").reset(); showError("form-error", "");
    const data = preset || mapping || {name:"",source_node:nodes()[0].id,target_node:nodes()[1].id,protocol:"tcp",listen_port:"",target_port:"",enabled:true};
    [$("mapping-source"),$("mapping-target")].forEach((select) => { select.replaceChildren(); nodes().forEach((node) => select.add(new Option(`${node.name}${node.online ? "" : " · 离线"}`, node.id))); });
    $("mapping-name").value = data.name; $("mapping-source").value = data.source_node; $("mapping-target").value = data.target_node;
    $("mapping-protocol").value = data.protocol; $("mapping-listen-port").value = data.listen_port; $("mapping-target-port").value = data.target_port; $("mapping-enabled").checked = data.enabled;
    $("editor-existing-actions").hidden = !mapping;
    state.editorInitial = JSON.stringify(draftFromForm());
    updatePreview(); renderMappings();
    $("mapping-name").focus({preventScroll:true});
    if (window.matchMedia("(max-width:920px)").matches) $("mapping-editor").scrollIntoView({behavior:"smooth",block:"start"});
  }
  function forceCloseEditor() {
    const returnFocus = $("mapping-editor").contains(document.activeElement);
    state.editorOpen = false; state.editorID = null; state.editorInitial = "";
    $("mapping-editor").hidden = true; $("mapping-layout").classList.remove("editor-open");
    if (state.overview) renderMappings();
    if (returnFocus && state.session) $("new-mapping").focus({preventScroll:true});
  }
  async function closeEditor() {
    if (state.mutationBusy) return false;
    if (!state.editorOpen) return true;
    if (JSON.stringify(draftFromForm()) !== state.editorInitial) {
      const discard = await confirmAction("放弃未保存的修改？", "这次填写的内容还没有保存。关闭后需要重新填写。", "放弃修改", false);
      if (!discard) return false;
    }
    forceCloseEditor(); return true;
  }
  function updatePreview() {
    const draft = draftFromForm(), source = findNode(draft.source_node), target = findNode(draft.target_node);
    setText("source-address", source ? `公网 ${source.public_ip}` : ""); setText("target-address", target ? `私网 ${target.private_ip}` : "");
    Array.from($("mapping-target").options).forEach((option) => { option.disabled = option.value === draft.source_node; });
    const preview = $("route-preview"); preview.replaceChildren();
    const left = el("div","preview-endpoint"), right = el("div","preview-endpoint");
    left.append(el("span","",source?.name || "入口"),el("code","",`${source?.public_ip || "—"}:${draft.listen_port || "端口"}`));
    right.append(el("span","",target?.name || "目标"),el("code","",`${target?.private_ip || "—"}:${draft.target_port || "端口"}`)); preview.append(left,icon("arrow"),right);
    const notice = $("port-notice"), ports = [...new Set([22,...(source?.reserved_ports || [])])].sort((a,b) => a-b);
    notice.hidden = !source; notice.querySelector("p").textContent = `入口保留端口：${ports.join("、")}。已有监听端口也会检查冲突。`;
    setText("mapping-save", draft.enabled ? "保存并应用" : "保存为停用");
  }

  function parsePorts(value) {
    if (!/^\d{1,5}(?:-\d{1,5})?$/.test(value)) throw new Error("端口格式应为 8080 或 8080-8090。");
    const parts = value.split("-").map(Number), start = parts[0], end = parts[1] ?? start;
    if (start < 1 || end > 65535 || end < start) throw new Error("端口必须在 1–65535 之间，范围从小到大填写。");
    if (end - start > 999) throw new Error("单条映射最多包含 1000 个端口。");
    return {start,end};
  }
  function samePorts(a,b) { try { const left = parsePorts(a), right = parsePorts(b); return left.start === right.start && left.end === right.end; } catch (_) { return false; } }
  function portString(range) { return range.start === range.end ? String(range.start) : `${range.start}-${range.end}`; }
  function validateDraft(draft) {
    if (!draft.name) throw new Error("请填写映射名称。");
    if (draft.source_node === draft.target_node) throw new Error("入口和目标需要选择不同的机器。");
    const source = findNode(draft.source_node), target = findNode(draft.target_node);
    if (!source || !target) throw new Error("机器配置已变化，请重新选择入口与目标。");
    const listen = parsePorts(draft.listen_port), destination = parsePorts(draft.target_port);
    if (listen.end-listen.start !== destination.end-destination.start) throw new Error("入口与目标的端口数量必须一致。");
    draft.listen_port = portString(listen); draft.target_port = portString(destination);
    if (draft.enabled) {
      const reserved = [22,...(source.reserved_ports || [])].find((port) => port >= listen.start && port <= listen.end);
      if (reserved) throw new Error(`入口端口 ${reserved} 为管理或已有服务保留，请换一个公网端口。`);
      const overlap = mappings().find((mapping) => {
        if (!mapping.enabled || mapping.id === state.editorID || mapping.source_node !== draft.source_node || !(mapping.protocol === draft.protocol || mapping.protocol === "both" || draft.protocol === "both")) return false;
        try { const ports = parsePorts(mapping.listen_port); return listen.start <= ports.end && ports.start <= listen.end; } catch (_) { return false; }
      });
      if (overlap) throw new Error(`入口端口与「${overlap.name}」重叠，请调整端口或协议。`);
    }
  }
  function setMutationBusy(busy) {
    state.mutationBusy = busy;
    $("operation-status").hidden = !busy; $("password-change").disabled = busy;
    document.querySelectorAll("[data-mutate]").forEach((entry) => { entry.disabled = busy; });
    document.querySelectorAll('[data-row-action="toggle"]').forEach((entry) => { entry.disabled = busy || state.editorOpen && state.editorID === entry.dataset.mappingId; });
    $("mapping-form").querySelectorAll("input,select").forEach((entry) => { entry.disabled = busy; });
    ["editor-close","mapping-cancel","mapping-reverse"].forEach((id) => { $(id).disabled = busy; });
    document.querySelectorAll('[data-row-action="probe"]').forEach((entry) => { const mapping = mappings().find((item) => item.id === entry.dataset.mappingId); entry.disabled = busy || mapping?.protocol === "udp" || !findNode(mapping?.source_node)?.online; });
  }

  async function saveMapping(event) {
    event.preventDefault(); if (state.mutationBusy) return;
    const payload = draftFromForm(); showError("form-error", "");
    try { validateDraft(payload); } catch (error) { showError("form-error", error.message); return; }
    const id = state.editorID; setMutationBusy(true); setText("mapping-save", "正在应用…");
    try {
      await api(id ? `/api/mappings/${encodeURIComponent(id)}` : "/api/mappings", {method:id ? "PUT" : "POST",body:payload});
      state.probes.delete(id); forceCloseEditor(); toast(id ? "映射已更新。" : payload.enabled ? "映射已保存并应用。" : "映射已保存，暂未启用。");
      await refreshData(true);
    } catch (error) { showError("form-error",error.message); }
    finally { setMutationBusy(false); if (state.editorOpen) updatePreview(); }
  }
  async function toggleMapping(mapping) {
    if (state.mutationBusy) return;
    if (mapping.enabled && !await confirmAction("停用这条映射？", `「${mapping.name}」将停止接收新连接。已有连接会自然结束。`, "停用映射", false)) return;
    setMutationBusy(true);
    try {
      await api(`/api/mappings/${encodeURIComponent(mapping.id)}`,{method:"PUT",body:{...mappingPayload(mapping),enabled:!mapping.enabled}});
      toast(mapping.enabled ? "映射已停用，新连接将不再转发。" : "映射已启用。");
      if (state.editorID === mapping.id) { $("mapping-enabled").checked = !mapping.enabled; state.editorInitial = JSON.stringify(draftFromForm()); updatePreview(); }
      await refreshData(true);
    } catch (error) { toast(error.message,"error"); }
    finally { setMutationBusy(false); }
  }
  async function deleteMapping() {
    const mapping = mappings().find((entry) => entry.id === state.editorID); if (!mapping || state.mutationBusy) return;
    if (!await confirmAction("删除这条映射？", `「${mapping.name}」将从配置中移除。入口停止接收新连接，已有连接会自然结束。目标机器上的服务不受影响。`, "删除映射", true)) return;
    setMutationBusy(true);
    try {
      await api(`/api/mappings/${encodeURIComponent(mapping.id)}`,{method:"DELETE"}); state.probes.delete(mapping.id); forceCloseEditor(); toast("映射已删除。"); await refreshData(true);
    } catch (error) { showError("form-error",error.message); }
    finally { setMutationBusy(false); }
  }
  async function reverseMapping() {
    const mapping = mappings().find((entry) => entry.id === state.editorID); if (!mapping || state.mutationBusy) return;
    const preset = {name:`${mapping.name} · 反向`.slice(0,80),source_node:mapping.target_node,target_node:mapping.source_node,listen_port:mapping.target_port,target_port:mapping.listen_port,protocol:mapping.protocol,enabled:false};
    await openEditor(null,preset);
    if (state.editorOpen && !state.editorID) toast("已交换入口与目标，请核对两端端口后保存。");
  }
  async function probeMapping(mapping) {
    if (mapping.protocol === "udp" || state.mutationBusy) return;
    const entries = Array.from(document.querySelectorAll('[data-row-action="probe"]')).filter((entry) => entry.dataset.mappingId === mapping.id);
    entries.forEach((entry) => { entry.disabled = true; entry.textContent = "检查中…"; });
    try {
      const result = await api(`/api/mappings/${encodeURIComponent(mapping.id)}/probe`,{method:"POST",timeout:12000});
      state.probes.set(mapping.id,{...result,message:result.message || (result.reachable ? `目标 TCP 可达 · ${result.latency_ms ?? 0} ms` : "目标 TCP 暂不可达")}); renderMappings();
      toast(result.reachable ? "目标私网 TCP 端口可达；公网入口仍需用实际客户端测试。" : result.message || "目标私网 TCP 端口暂不可达。", result.reachable ? "success" : "error");
    } catch (error) { toast(error.message,"error"); renderMappings(); }
  }

  function confirmAction(title, description, label, danger = false) {
    if (state.confirmResolve) return Promise.resolve(false);
    setText("confirm-title",title); setText("confirm-description",description); setText("confirm-ok",label);
    $("confirm-ok").className = `button ${danger ? "danger" : "primary"}`;
    $("confirm-icon").classList.toggle("neutral", !danger);
    $("confirm-dialog").showModal(); $("confirm-cancel").focus();
    return new Promise((resolve) => { state.confirmResolve = resolve; });
  }
  function resolveConfirm(result) {
    const resolve = state.confirmResolve; state.confirmResolve = null;
    if ($("confirm-dialog").open) $("confirm-dialog").close(); if (resolve) resolve(result);
  }

  async function reconcile() {
    if (state.mutationBusy) return;
    setMutationBusy(true); const label = $("reconcile").querySelector("span"); label.textContent = "正在同步…";
    try { await api("/api/reconcile",{method:"POST"}); toast("规则已重新下发到节点。"); await refreshData(true); }
    catch (error) { toast(error.message,"error"); await refreshData(true); }
    finally { setMutationBusy(false); label.textContent = "同步规则"; }
  }
  async function exportConfig() {
    $("export").disabled = true;
    try {
      const data = await api("/api/export"), blob = new Blob([JSON.stringify(data,null,2)+"\n"],{type:"application/json"}), url = URL.createObjectURL(blob);
      const link = el("a"); link.href = url; link.download = `netshare-${new Date().toISOString().slice(0,10)}.json`; link.hidden = true; document.body.append(link); link.click(); link.remove(); setTimeout(() => URL.revokeObjectURL(url),1000); toast("配置已导出。导出文件不包含登录凭据。");
    } catch (error) { toast(error.message,"error"); }
    finally { $("export").disabled = false; }
  }
  function openPassword() {
    $("password-form").reset(); showError("password-error",""); $("password-dialog").showModal(); $("password-current").focus();
  }
  function closePassword() { if (state.passwordBusy) return; $("password-dialog").close(); $("password-form").reset(); }
  async function savePassword(event) {
    event.preventDefault(); if (state.passwordBusy) return;
    const current = $("password-current").value, password = $("password-new").value;
    showError("password-error","");
    if (Array.from(password).length < 12 || new TextEncoder().encode(password).length > 72) { showError("password-error","新密码至少 12 个字符，最多 72 字节。"); return; }
    if (password !== $("password-repeat").value) { showError("password-error","两次输入的新密码不一致。"); return; }
    state.passwordBusy = true; $("password-save").disabled = true; $("password-cancel").disabled = true; setText("password-save","正在保存…");
    try {
      const result = await api("/api/password",{method:"POST",body:{current_password:current,new_password:password}});
      if (result.csrf_token && state.session) state.session.csrf_token = result.csrf_token;
      $("password-dialog").close(); $("password-form").reset(); toast("密码已修改，其他设备的登录已失效。");
    } catch (error) { showError("password-error",error.message); }
    finally { state.passwordBusy = false; $("password-save").disabled = false; $("password-cancel").disabled = false; setText("password-save","保存新密码"); }
  }

  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault(); if ($("login-submit").disabled) return;
    $("login-submit").disabled = true; setText("login-submit","正在登录…"); showError("login-error","");
    try {
      const session = await api("/api/login",{method:"POST",body:{username:$("login-username").value.trim(),password:$("login-password").value}});
      if (!session.authenticated) throw new Error("登录失败，请重新输入账号与密码。");
      $("login-password").value = ""; setAuthenticated(session); await refreshData(true);
    } catch (error) { showError("login-error",error.message); $("login-password").focus(); }
    finally { $("login-submit").disabled = false; setText("login-submit","登录"); }
  });
  $("logout").addEventListener("click",async () => {
    if (state.mutationBusy || !await closeEditor()) return;
    $("logout").disabled = true;
    try { await api("/api/logout",{method:"POST"}); setAuthenticated(null); state.overview = null; state.probes.clear(); showError("login-error",""); $("login-password").value=""; $("login-username").focus(); }
    catch (error) { toast(error.message,"error"); }
    finally { $("logout").disabled = false; }
  });
  $("password-visibility").addEventListener("click",() => { const visible = $("login-password").type === "password"; $("login-password").type = visible ? "text" : "password"; setText("password-visibility",visible ? "隐藏" : "显示"); $("password-visibility").setAttribute("aria-label",visible ? "隐藏密码" : "显示密码"); $("password-visibility").setAttribute("aria-pressed",String(visible)); });
  document.querySelectorAll("[data-view]").forEach((entry) => entry.addEventListener("click",() => navigate(entry.dataset.view)));
  window.addEventListener("hashchange",() => navigate(location.hash.slice(1)));
  $("mapping-search").addEventListener("input",renderMappings); $("source-filter").addEventListener("change",renderMappings); $("status-filter").addEventListener("change",renderMappings);
  $("event-search").addEventListener("input",renderEvents); $("event-filter").addEventListener("change",renderEvents);
  $("clear-filters").addEventListener("click",() => { $("mapping-search").value=""; $("source-filter").value=""; $("status-filter").value=""; renderMappings(); });
  $("new-mapping").addEventListener("click",() => openEditor()); $("empty-new").addEventListener("click",() => openEditor());
  $("editor-close").addEventListener("click",closeEditor); $("mapping-cancel").addEventListener("click",closeEditor);
  $("mapping-form").addEventListener("submit",saveMapping);
  $("mapping-form").addEventListener("input",updatePreview);
  $("mapping-form").addEventListener("change",(event) => {
    if (event.target.id === "mapping-source" && $("mapping-source").value === $("mapping-target").value) {
      const alternative = nodes().find((node) => node.id !== $("mapping-source").value); if (alternative) $("mapping-target").value=alternative.id;
    }
    updatePreview();
  });
  $("mapping-delete").addEventListener("click",deleteMapping); $("mapping-reverse").addEventListener("click",reverseMapping);
  $("refresh").addEventListener("click",() => refreshData(true)); $("error-retry").addEventListener("click",() => refreshData(true)); $("boot-retry").addEventListener("click",boot);
  $("reconcile").addEventListener("click",reconcile); $("export").addEventListener("click",exportConfig);
  $("confirm-ok").addEventListener("click",() => resolveConfirm(true)); $("confirm-cancel").addEventListener("click",() => resolveConfirm(false));
  $("confirm-dialog").addEventListener("cancel",(event) => { event.preventDefault(); resolveConfirm(false); });
  $("confirm-dialog").addEventListener("close",() => { if (state.confirmResolve) resolveConfirm(false); });
  $("password-change").addEventListener("click",openPassword); $("password-cancel").addEventListener("click",closePassword); $("password-form").addEventListener("submit",savePassword);
  $("password-dialog").addEventListener("cancel",(event) => { if (state.passwordBusy) event.preventDefault(); else $("password-form").reset(); });
  document.addEventListener("visibilitychange",() => { if (!document.hidden && state.session && $("auto-refresh").checked && !state.mutationBusy) refreshData(); });
  document.addEventListener("keydown",(event) => {
    const editing = ["INPUT","TEXTAREA","SELECT"].includes(document.activeElement?.tagName);
    if (!state.session || editing || event.metaKey || event.ctrlKey || event.altKey || $("confirm-dialog").open || $("password-dialog").open) return;
    if (event.key === "/" && state.view === "mappings") { event.preventDefault(); $("mapping-search").focus(); }
    if (event.key === "Escape" && state.editorOpen) closeEditor();
  });
  window.addEventListener("beforeunload",(event) => { if (state.editorOpen && JSON.stringify(draftFromForm()) !== state.editorInitial) { event.preventDefault(); event.returnValue=""; } });
  document.querySelectorAll("svg.icon").forEach((entry) => entry.setAttribute("aria-hidden","true"));
  navigate(location.hash.slice(1) || "mappings");
  boot();
})();
