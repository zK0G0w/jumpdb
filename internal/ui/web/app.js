"use strict";

// 访问令牌来自终端打印的地址；接口以自定义请求头携带，跨站页面无法伪造。
const token = new URLSearchParams(location.search).get("token") || "";

const labels = {
  stage: { session: "会话申请", dial: "代理连接", activate: "会话激活", handshake: "代理握手", closed: "连接结束" },
  tunnel: { starting: "启动中", listening: "监听中", stopping: "停止中", stopped: "已停止", failed: "已失败" },
  tunnelClass: { starting: "warn", listening: "ok", stopping: "warn", stopped: "", failed: "bad" },
  connection: { connecting: "建立中", active: "转发中", closed: "已结束", failed: "失败" },
  // stopped 涵盖手动停止、因其他连接失败而停止隧道及控制页退出。
  reason: { client: "客户端关闭", upstream: "上游关闭", stopped: "隧道停止" },
  failureStage: { config: "本地配置", probe: "门户探测", initial: "初始会话", listen: "本地监听", session: "会话申请", dial: "代理连接", activate: "会话激活", handshake: "代理握手" },
};

// 这些阶段失败通常与 Token 有关，失败卡片提供跳转到 Token 更新的入口。
const tokenStages = new Set(["probe", "initial", "session", "activate"]);

const ui = {
  state: null,
  configSignature: "",
  reconfigure: false,
  passwords: new Map(),
  openLogs: new Set(),
  pointerDown: false,
  // themeLocal 是未完成配置时的页内主题选择，无处持久化；themePending 是保存中的主题，避免轮询把界面闪回旧值。
  themeLocal: null,
  themePending: null,
};

const themes = [
  { value: "system", label: "跟随系统", icon: ["M4 5h16a1 1 0 0 1 1 1v9a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z", "M8 20h8", "M12 16v4"] },
  { value: "light", label: "浅色", icon: ["M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8z", "M12 2v2", "M12 20v2", "M4.93 4.93l1.41 1.41", "M17.66 17.66l1.41 1.41", "M2 12h2", "M20 12h2", "M4.93 19.07l1.41-1.41", "M17.66 6.34l1.41-1.41"] },
  { value: "dark", label: "深色", icon: ["M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"] },
];

async function api(path, body) {
  const options = { headers: { "X-Jumpdb-Token": token } };
  if (body !== undefined) {
    options.method = "POST";
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  const response = await fetch(path, options);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(data.error || `请求失败（HTTP ${response.status}）`);
  }
  return data;
}

// h 以 DOM 节点构建界面；文本一律作为文本节点插入，不解析 HTML。
function h(tag, attributes, ...children) {
  const element = document.createElement(tag);
  for (const [key, value] of Object.entries(attributes || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith("on")) element.addEventListener(key.slice(2), value);
    else if (key === "class") element.className = value;
    else if (value === true) element.setAttribute(key, "");
    else element.setAttribute(key, String(value));
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    element.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return element;
}

// icon 以 SVG 节点绘制线性图标，路径均为本文件内的常量。
function icon(paths) {
  const namespace = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(namespace, "svg");
  for (const [key, value] of Object.entries({ viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": "2", "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true" })) {
    svg.setAttribute(key, value);
  }
  for (const d of paths) {
    const path = document.createElementNS(namespace, "path");
    path.setAttribute("d", d);
    svg.append(path);
  }
  return svg;
}

function notify(message, ok) {
  const notice = document.getElementById("notice");
  notice.textContent = message;
  notice.className = ok ? "toast ok" : "toast";
  notice.hidden = false;
  clearTimeout(notify.timer);
  notify.timer = setTimeout(() => { notice.hidden = true; }, ok ? 3000 : 8000);
}

// act 执行一次修改操作：失败时提示服务端给出的已脱敏原因，成功后立即刷新。
async function act(path, body, success) {
  try {
    const result = await api(path, body);
    if (success) notify(success, true);
    await refresh();
    return result;
  } catch (error) {
    notify(error.message, false);
    return null;
  }
}

function formatBytes(value) {
  const units = ["B", "KiB", "MiB", "GiB"];
  let size = value;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit++;
  }
  return unit === 0 ? `${size} B` : `${size.toFixed(1)} ${units[unit]}`;
}

function formatDuration(ms) {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分 ${seconds % 60} 秒`;
  return `${Math.floor(minutes / 60)} 时 ${minutes % 60} 分`;
}

function formatTime(value) {
  return new Date(value).toLocaleTimeString("zh-CN", { hour12: false });
}

function anyTunnelRunning() {
  return (ui.state?.tunnels || []).some((tunnel) => ["starting", "listening", "stopping"].includes(tunnel.status));
}

function runningTunnelOnPort(port) {
  return (ui.state?.tunnels || []).some((tunnel) => tunnel.port === port && ["starting", "listening", "stopping"].includes(tunnel.status));
}

async function refresh() {
  const status = document.getElementById("link-status");
  try {
    ui.state = await api("/api/state");
    status.textContent = "控制页在线";
    status.className = "pill ok";
  } catch (error) {
    status.textContent = "控制页已断开";
    status.className = "pill bad";
    return;
  }
  render();
}

// busy 表示用户正在点击或选中隧道区的文本（如复制密码），此时跳过轮询重绘以免打断操作。
function busy() {
  const selection = document.getSelection();
  const selecting = selection && !selection.isCollapsed && document.getElementById("tunnels").contains(selection.anchorNode);
  return ui.pointerDown || selecting;
}

function render() {
  renderTheme();
  const signature = JSON.stringify(ui.state.config) + ui.reconfigure + anyTunnelRunning();
  // 配置表单只在配置变化时重建，避免轮询清空正在输入的内容。
  if (signature !== ui.configSignature) {
    ui.configSignature = signature;
    renderConfig();
  }
  renderResources();
  renderTunnels();
}

// effectiveTheme 优先取保存中的选择；已配置时以配置文件为准，否则用页内选择或页面预置值。
function effectiveTheme() {
  if (ui.themePending) return ui.themePending;
  const config = ui.state?.config;
  if (config?.configured) return config.theme;
  return ui.themeLocal || "system";
}

function renderTheme() {
  const theme = ui.state ? effectiveTheme() : document.documentElement.dataset.theme;
  document.documentElement.dataset.theme = theme;
  for (const button of document.querySelectorAll("#theme button")) {
    button.setAttribute("aria-checked", String(button.dataset.value === theme));
  }
}

// chooseTheme 立即切换外观；配置可用时写入配置文件，否则只在本页生效，首次保存配置时再一并写入。
async function chooseTheme(theme) {
  if (!ui.state?.config.configured) {
    ui.themeLocal = theme;
    renderTheme();
    notify("完成配置后才能保存主题，当前仅本页生效", true);
    return;
  }
  ui.themePending = theme;
  renderTheme();
  await act("/api/theme", { theme });
  ui.themePending = null;
  if (ui.state) render();
}

function initTheme() {
  document.getElementById("theme").replaceChildren(...themes.map((theme) => h("button", {
    type: "button",
    role: "radio",
    title: theme.label,
    "aria-label": theme.label,
    "data-value": theme.value,
    onclick: () => chooseTheme(theme.value),
  }, icon(theme.icon), h("span", { class: "label" }, theme.label))));
  renderTheme();
}

function secretInput(name, placeholder) {
  return h("input", { name, type: "password", placeholder, autocomplete: "off", spellcheck: "false", required: true });
}

function field(label, input, className) {
  return h("label", { class: className ? `field ${className}` : "field" }, h("span", {}, label), input);
}

// kv 渲染一组“标签—值”单元格；值区域内的按钮与文本同行垂直居中。
function kv(items) {
  return h("dl", { class: "kv" }, items.filter(Boolean).map(([label, value, className]) =>
    h("div", { class: className }, h("dt", {}, label), h("dd", {}, value))));
}

function setMark() {
  return h("span", { class: "set" }, "已设置");
}

function renderConfig() {
  const root = document.getElementById("config");
  const config = ui.state.config;
  root.replaceChildren();
  if (config.error) {
    root.append(
      h("div", { class: "failure" }, h("span", { class: "text" }, config.error)),
      h("p", { class: "hint config-actions" }, "控制页不会覆盖损坏或权限异常的配置；请先检查文件，或手动移走后重新配置。"),
    );
    return;
  }
  if (!config.configured || ui.reconfigure) {
    root.append(fullConfigForm(config));
    return;
  }
  root.append(
    kv([
      ["门户", h("span", { class: "mono" }, config.portal), "wide"],
      ["传输密钥", setMark()],
      ["数据库密钥", setMark()],
      ["Token", setMark()],
    ]),
    tokenForm(),
  );
}

// tokenForm 只更新 Token；运行中的隧道在下一个新连接起使用新 Token。
function tokenForm() {
  const input = secretInput("token", "新的 Token（仅值）");
  input.id = "token-input";
  // 运行中的隧道已锁定门户和密钥，重新配置须先停止全部隧道；Token 可随时更新。
  const locked = anyTunnelRunning();
  return h("div", { class: "config-actions" }, h("form", {
    class: "inline-form",
    autocomplete: "off",
    onsubmit: async (event) => {
      event.preventDefault();
      const result = await act("/api/config", { token: input.value }, "Token 已保存；未验证有效性，运行中的隧道将在新连接时使用");
      if (result) input.value = "";
    },
  },
  field("更新 Token", input),
  h("button", { type: "submit", class: "btn primary" }, "更新 Token"),
  h("button", { type: "button", class: "btn", disabled: locked, title: locked ? "请先停止所有隧道" : undefined, onclick: () => { ui.reconfigure = true; render(); } }, "重新配置")),
  locked && h("p", { class: "hint" }, "有隧道运行时不能修改门户地址或密钥，请先停止所有隧道；更新 Token 不受影响。"));
}

// fullConfigForm 用于首次配置或显式重新配置；密钥和 Token 从不回填，门户地址可回填核对。
function fullConfigForm(config) {
  const portal = h("input", { name: "portal", type: "url", placeholder: "https://…", autocomplete: "off", spellcheck: "false", required: true });
  portal.value = config.portal || "";
  const transKey = secretInput("transKey", "原始 16 字节");
  const dbKey = secretInput("dbKey", "原始 16 字节");
  const tokenInput = secretInput("token", "仅 Token 值");
  return h("form", {
    autocomplete: "off",
    onsubmit: async (event) => {
      event.preventDefault();
      const body = { portal: portal.value.trim(), transKey: transKey.value, dbKey: dbKey.value, token: tokenInput.value, configure: true };
      if (await act("/api/config", body, "配置已保存（0600）；未验证 Token 有效性")) {
        ui.reconfigure = false;
        // 配置前在页内选过的主题，此时才有文件可写。
        if (ui.themeLocal) {
          const theme = ui.themeLocal;
          ui.themeLocal = null;
          await chooseTheme(theme);
        }
        render();
      }
    },
  },
  h("div", { class: "config-form" },
    field("门户 HTTPS 地址", portal, "wide"),
    field("传输密钥", transKey),
    field("数据库密钥", dbKey),
    field("Token", tokenInput, "wide")),
  h("div", { class: "form-foot" },
    h("p", { class: "hint" }, config.configured
      ? "重新配置会覆盖门户地址、两把密钥及 Token，资源列表保留；修改门户或密钥前需先停止所有隧道。"
      : "首次使用请填写全部字段。配置保存在本机，文件权限 0600。"),
    config.configured && h("button", { type: "button", class: "btn", onclick: () => { ui.reconfigure = false; render(); } }, "取消"),
    h("button", { type: "submit", class: "btn primary" }, "保存配置")));
}

function saveResources(resources, success) {
  return act("/api/resources", { resources }, success);
}

function renderResources() {
  const root = document.getElementById("resources");
  const resources = ui.state.config.resources || [];
  if (resources.length === 0) {
    root.replaceChildren(h("p", { class: "empty" }, ui.state.config.configured ? "尚未保存资源。添加后可一键启动隧道，不会自动启动。" : "完成配置后可添加资源。"));
    return;
  }
  const rows = resources.map((resource, index) => h("tr", {},
    h("td", {}, resource.name || h("span", { class: "muted" }, "—")),
    h("td", { class: "mono" }, resource.id),
    h("td", {}, h("span", { class: "tag" }, resource.id_type)),
    h("td", { class: "num mono" }, resource.port),
    h("td", { class: "actions" },
      h("button", {
        class: "btn primary sm",
        disabled: runningTunnelOnPort(resource.port),
        onclick: () => act("/api/tunnels/start", resource, "已开始启动隧道"),
      }, "启动"),
      h("button", {
        class: "btn danger sm",
        onclick: () => saveResources(resources.filter((_, other) => other !== index), "资源已删除"),
      }, "删除"))));
  root.replaceChildren(h("div", { class: "table-wrap resource-table" }, h("table", {},
    h("thead", {}, h("tr", {}, ["别名", "资源 ID", "ID 类型", "本地端口", ""].map((title) => h("th", {}, title)))),
    h("tbody", {}, rows))));
}

function renderTunnels() {
  const root = document.getElementById("tunnels");
  const tunnels = ui.state.tunnels || [];
  // 隧道不在监听时立即丢弃已显示的密码，不在页面中保留。
  for (const id of ui.passwords.keys()) {
    if (!tunnels.some((tunnel) => tunnel.id === id && tunnel.status === "listening")) ui.passwords.delete(id);
  }
  if (tunnels.length === 0) {
    root.replaceChildren(h("p", { class: "empty" }, "暂无隧道。在上方资源列表中点击“启动”。"));
    return;
  }
  // 重绘后恢复各表格的滚动位置。
  const scrolls = new Map([...root.querySelectorAll(".scroll[data-key]")].map((element) => [element.dataset.key, element.scrollTop]));
  root.replaceChildren(h("div", { class: "tunnel-list" }, tunnels.map(tunnelCard)));
  for (const element of root.querySelectorAll(".scroll[data-key]")) {
    element.scrollTop = scrolls.get(element.dataset.key) || 0;
  }
}

function tunnelCard(tunnel) {
  const running = ["starting", "listening"].includes(tunnel.status);
  return h("div", { class: "tunnel" },
    h("div", { class: "tunnel-head" },
      h("h3", {}, tunnel.resource.name || `资源 ${tunnel.resource.id}`),
      h("span", { class: `pill ${labels.tunnelClass[tunnel.status] || ""}` }, labels.tunnel[tunnel.status] || tunnel.status),
      h("span", { class: "mono address" }, `${tunnel.host}:${tunnel.port}`),
      h("span", { class: "spacer" }),
      running && h("button", { class: "btn danger sm", onclick: () => act("/api/tunnels/stop", { id: tunnel.id }, "正在停止隧道") }, "停止"),
      !running && tunnel.status !== "stopping" && h("button", { class: "btn sm", onclick: () => act("/api/tunnels/dismiss", { id: tunnel.id }) }, "清除")),
    h("div", { class: "tunnel-body" },
      tunnel.type && credentials(tunnel),
      tunnel.failure && failure(tunnel),
      connections(tunnel),
      events(tunnel)));
}

function credentials(tunnel) {
  const password = ui.passwords.get(tunnel.id);
  let passwordPart = h("span", { class: "muted" }, "—");
  if (password !== undefined) {
    passwordPart = [h("span", { class: "mono" }, password),
      h("button", { class: "btn sm", onclick: () => { ui.passwords.delete(tunnel.id); render(); } }, "隐藏")];
  } else if (tunnel.status === "listening") {
    passwordPart = [h("span", { class: "secret-mask" }, "••••••"),
      h("button", {
        class: "btn sm",
        onclick: async () => {
          try {
            const result = await api("/api/tunnels/password", { id: tunnel.id });
            ui.passwords.set(tunnel.id, result.password);
            render();
          } catch (error) {
            notify(error.message, false);
          }
        },
      }, "显示密码")];
  }
  return h("div", {},
    kv([
      ["主机", h("span", { class: "mono" }, tunnel.host)],
      ["端口", h("span", { class: "mono" }, tunnel.port)],
      ["类型", tunnel.type],
      ["账号", h("span", { class: "mono" }, tunnel.username)],
      ["初始会话密码", passwordPart, "wide"],
    ]),
    password !== undefined && h("p", { class: "hint config-actions" }, "这是初始会话的凭据；后续连接会重新申请会话，不保证一直有效。"));
}

function failure(tunnel) {
  const stage = labels.failureStage[tunnel.failureStage] || tunnel.failureStage;
  return h("div", { class: "failure" },
    h("span", { class: "text" }, `失败阶段：${stage}。${tunnel.failure}`),
    tokenStages.has(tunnel.failureStage) && h("button", {
      class: "btn sm",
      onclick: () => {
        const input = document.getElementById("token-input");
        if (input) {
          input.scrollIntoView({ block: "center" });
          input.focus();
        } else {
          notify("请先在配置区完成配置或取消重新配置", false);
        }
      },
    }, "更新 Token"));
}

const connectionClass = { connecting: "warn", active: "ok", closed: "", failed: "bad" };

function connections(tunnel) {
  const list = [...tunnel.connections].reverse();
  const head = h("div", { class: "section-label" },
    h("h4", {}, "连接"),
    h("span", { class: "hint" }, `活跃 ${tunnel.active} 个 · 最近 ${list.length} 个 · 数据只保存在内存中`));
  if (list.length === 0) return h("div", {}, head, h("div", { class: "table-box" }, h("p", { class: "empty" }, "暂无客户端连接")));
  return h("div", {}, head, h("div", { class: "table-box" }, h("div", { class: "scroll", "data-key": `connections-${tunnel.id}` }, h("table", {},
    h("thead", {}, h("tr", {}, ["#", "接入时间", "状态", "已通过阶段", "时长", "上行", "下行", "结束说明"].map((title) => h("th", {}, title)))),
    h("tbody", {}, list.map((connection) => h("tr", {},
      h("td", { class: "num muted" }, connection.id),
      h("td", { class: "num" }, formatTime(connection.accepted)),
      h("td", {}, h("span", { class: `pill ${connectionClass[connection.status] || ""}` }, labels.connection[connection.status] || connection.status)),
      h("td", {}, connection.stages.map((stage) => labels.stage[stage] || stage).join(" → ") || "—"),
      h("td", { class: "num" }, formatDuration(connection.durationMs)),
      h("td", { class: "num" }, formatBytes(connection.up)),
      h("td", { class: "num" }, formatBytes(connection.down)),
      h("td", { class: connection.failedStage ? "bad-text" : "" }, connection.failedStage ? `${labels.stage[connection.failedStage] || connection.failedStage}失败` : labels.reason[connection.reason] || "—"))))))));
}

function events(tunnel) {
  const list = [...tunnel.events].reverse();
  const details = h("details", { open: ui.openLogs.has(tunnel.id) },
    h("summary", {}, `事件流水（最近 ${list.length} 条）`),
    h("div", { class: "table-box" }, h("div", { class: "scroll", "data-key": `events-${tunnel.id}` }, h("table", {},
      h("thead", {}, h("tr", {}, ["时间", "连接", "阶段", "结果"].map((title) => h("th", {}, title)))),
      h("tbody", {}, list.map((event) => h("tr", {},
        h("td", { class: "num" }, formatTime(event.time)),
        h("td", { class: "num muted" }, `#${event.conn}`),
        h("td", {}, labels.stage[event.stage] || event.stage),
        h("td", { class: event.success ? "" : "bad-text" }, event.stage === "closed" ? labels.reason[event.reason] || "—" : event.success ? "成功" : "失败"))))))));
  details.addEventListener("toggle", () => {
    if (details.open) ui.openLogs.add(tunnel.id);
    else ui.openLogs.delete(tunnel.id);
  });
  return details;
}

document.getElementById("resource-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.target;
  const resource = {
    id: form.elements.id.value.trim(),
    id_type: form.elements.id_type.value,
    port: Number(form.elements.port.value),
  };
  const name = form.elements.name.value.trim();
  if (name) resource.name = name;
  const resources = [...(ui.state?.config.resources || []), resource];
  if (await saveResources(resources, "资源已保存")) {
    form.elements.id.value = "";
    form.elements.name.value = "";
  }
});

document.addEventListener("pointerdown", () => { ui.pointerDown = true; });
document.addEventListener("pointerup", () => { ui.pointerDown = false; });
document.addEventListener("pointercancel", () => { ui.pointerDown = false; });

initTheme();
refresh();
setInterval(() => {
  if (!busy()) refresh();
}, 1000);
