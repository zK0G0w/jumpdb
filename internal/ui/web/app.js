"use strict";

// 访问令牌来自终端打印的地址；接口以自定义请求头携带，跨站页面无法伪造。
const token = new URLSearchParams(location.search).get("token") || "";

const labels = {
  stage: { session: "会话申请", dial: "代理连接", activate: "会话激活", handshake: "代理握手", closed: "连接结束" },
  tunnel: { starting: "启动中", listening: "监听中", stopping: "停止中", stopped: "已停止", failed: "已失败" },
  tunnelClass: { starting: "warn", listening: "ok", stopping: "warn", stopped: "", failed: "bad" },
  connection: { connecting: "建立中", active: "转发中", closed: "已结束", failed: "失败" },
  reason: { client: "客户端关闭", upstream: "上游关闭", stopped: "主动停止" },
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
};

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

function notify(message, ok) {
  const notice = document.getElementById("notice");
  notice.textContent = message;
  notice.className = ok ? "notice ok" : "notice";
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

function runningTunnelOnPort(port) {
  return (ui.state?.tunnels || []).some((tunnel) => tunnel.port === port && ["starting", "listening", "stopping"].includes(tunnel.status));
}

async function refresh() {
  const status = document.getElementById("link-status");
  try {
    ui.state = await api("/api/state");
    status.textContent = "控制页在线";
    status.className = "badge ok";
  } catch (error) {
    status.textContent = "控制页已断开";
    status.className = "badge bad";
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
  const signature = JSON.stringify(ui.state.config) + ui.reconfigure;
  // 配置表单只在配置变化时重建，避免轮询清空正在输入的内容。
  if (signature !== ui.configSignature) {
    ui.configSignature = signature;
    renderConfig();
  }
  renderResources();
  renderTunnels();
}

function secretInput(name, placeholder) {
  return h("input", { name, type: "password", placeholder, autocomplete: "off", spellcheck: "false", required: true });
}

function renderConfig() {
  const root = document.getElementById("config");
  const config = ui.state.config;
  root.replaceChildren();
  if (config.error) {
    root.append(
      h("div", { class: "failure" }, config.error),
      h("p", { class: "hint" }, "控制页不会覆盖损坏或权限异常的配置；请先检查文件，或手动移走后重新配置。"),
    );
    return;
  }
  if (!config.configured || ui.reconfigure) {
    root.append(fullConfigForm(config));
    return;
  }
  root.append(
    h("p", { class: "facts" },
      h("span", {}, "门户：", h("b", { class: "mono" }, config.portal)),
      h("span", {}, "传输密钥：", h("b", {}, "已设置")),
      h("span", {}, "数据库密钥：", h("b", {}, "已设置")),
      h("span", {}, "Token：", h("b", {}, "已设置"))),
    tokenForm(),
  );
}

// tokenForm 只更新 Token；运行中的隧道在下一个新连接起使用新 Token。
function tokenForm() {
  const input = secretInput("token", "新的 Token（仅值）");
  input.id = "token-input";
  return h("form", {
    class: "row-form",
    autocomplete: "off",
    onsubmit: async (event) => {
      event.preventDefault();
      const result = await act("/api/config", { token: input.value }, "Token 已保存；未验证有效性，运行中的隧道将在新连接时使用");
      if (result) input.value = "";
    },
  },
  input,
  h("button", { type: "submit", class: "primary" }, "更新 Token"),
  h("button", { type: "button", onclick: () => { ui.reconfigure = true; render(); } }, "重新配置"));
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
        render();
      }
    },
  },
  h("div", { class: "grid-form" },
    h("label", {}, "门户 HTTPS 地址"), portal,
    h("label", {}, "传输密钥"), transKey,
    h("label", {}, "数据库密钥"), dbKey,
    h("label", {}, "Token"), tokenInput),
  h("p", { class: "hint" }, config.configured
    ? "重新配置会覆盖门户地址、两把密钥及 Token，资源列表保留；修改门户或密钥前需先停止所有隧道。"
    : "首次使用请填写全部字段。配置保存在本机，文件权限 0600。"),
  h("div", { class: "actions" },
    h("button", { type: "submit", class: "primary" }, "保存配置"),
    config.configured && h("button", { type: "button", onclick: () => { ui.reconfigure = false; render(); } }, "取消")));
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
    h("td", {}, resource.name || "—"),
    h("td", { class: "mono" }, resource.id),
    h("td", {}, resource.id_type),
    h("td", { class: "num" }, resource.port),
    h("td", {},
      h("button", {
        class: "primary",
        disabled: runningTunnelOnPort(resource.port),
        onclick: () => act("/api/tunnels/start", resource, "已开始启动隧道"),
      }, "启动"),
      " ",
      h("button", {
        class: "danger",
        onclick: () => saveResources(resources.filter((_, other) => other !== index), "资源已删除"),
      }, "删除"))));
  root.replaceChildren(h("table", {},
    h("thead", {}, h("tr", {}, ["别名", "资源 ID", "ID 类型", "本地端口", "操作"].map((title) => h("th", {}, title)))),
    h("tbody", {}, rows)));
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
  root.replaceChildren(...tunnels.map(tunnelCard));
  for (const element of root.querySelectorAll(".scroll[data-key]")) {
    element.scrollTop = scrolls.get(element.dataset.key) || 0;
  }
}

function tunnelCard(tunnel) {
  const running = ["starting", "listening"].includes(tunnel.status);
  return h("div", { class: "tunnel" },
    h("div", { class: "tunnel-head" },
      h("h3", {}, tunnel.resource.name || `资源 ${tunnel.resource.id}`),
      h("span", { class: `badge ${labels.tunnelClass[tunnel.status] || ""}` }, labels.tunnel[tunnel.status] || tunnel.status),
      h("span", { class: "mono" }, `${tunnel.host}:${tunnel.port}`),
      running && h("button", { class: "danger", onclick: () => act("/api/tunnels/stop", { id: tunnel.id }, "正在停止隧道") }, "停止"),
      !running && tunnel.status !== "stopping" && h("button", { onclick: () => act("/api/tunnels/dismiss", { id: tunnel.id }) }, "清除")),
    tunnel.type && credentials(tunnel),
    tunnel.failure && failure(tunnel),
    connections(tunnel),
    events(tunnel));
}

function credentials(tunnel) {
  const password = ui.passwords.get(tunnel.id);
  let passwordPart;
  if (password !== undefined) {
    passwordPart = h("span", {}, "初始会话密码：", h("b", { class: "mono" }, password), " ",
      h("button", { onclick: () => { ui.passwords.delete(tunnel.id); render(); } }, "隐藏"));
  } else if (tunnel.status === "listening") {
    passwordPart = h("button", {
      onclick: async () => {
        try {
          const result = await api("/api/tunnels/password", { id: tunnel.id });
          ui.passwords.set(tunnel.id, result.password);
          render();
        } catch (error) {
          notify(error.message, false);
        }
      },
    }, "显示密码");
  }
  return h("div", {},
    h("p", { class: "facts" },
      h("span", {}, "主机：", h("b", { class: "mono" }, tunnel.host)),
      h("span", {}, "端口：", h("b", { class: "mono" }, tunnel.port)),
      h("span", {}, "类型：", h("b", {}, tunnel.type)),
      h("span", {}, "账号：", h("b", { class: "mono" }, tunnel.username)),
      passwordPart),
    password !== undefined && h("p", { class: "hint" }, "这是初始会话的凭据；后续连接会重新申请会话，不保证一直有效。"));
}

function failure(tunnel) {
  const stage = labels.failureStage[tunnel.failureStage] || tunnel.failureStage;
  return h("div", { class: "failure" },
    `失败阶段：${stage}。${tunnel.failure}`,
    tokenStages.has(tunnel.failureStage) && h("span", {}, " ",
      h("button", {
        onclick: () => {
          const input = document.getElementById("token-input");
          if (input) {
            input.scrollIntoView({ block: "center" });
            input.focus();
          } else {
            notify("请先在配置区完成配置或取消重新配置", false);
          }
        },
      }, "更新 Token")));
}

function connections(tunnel) {
  const list = [...tunnel.connections].reverse();
  const summary = h("p", { class: "hint" }, `活跃连接 ${tunnel.active} 个；显示最近 ${list.length} 个连接，数据只保存在内存中。`);
  if (list.length === 0) return h("div", {}, summary);
  return h("div", {}, summary, h("div", { class: "scroll", "data-key": `connections-${tunnel.id}` }, h("table", {},
    h("thead", {}, h("tr", {}, ["#", "接入时间", "状态", "已通过阶段", "时长", "上行", "下行", "结束说明"].map((title) => h("th", {}, title)))),
    h("tbody", {}, list.map((connection) => h("tr", {},
      h("td", { class: "num" }, connection.id),
      h("td", { class: "num" }, formatTime(connection.accepted)),
      h("td", { class: connection.status === "failed" ? "bad-text" : connection.status === "active" ? "ok-text" : "" }, labels.connection[connection.status] || connection.status),
      h("td", {}, connection.stages.map((stage) => labels.stage[stage] || stage).join(" → ") || "—"),
      h("td", { class: "num" }, formatDuration(connection.durationMs)),
      h("td", { class: "num" }, formatBytes(connection.up)),
      h("td", { class: "num" }, formatBytes(connection.down)),
      h("td", {}, connection.failedStage ? `${labels.stage[connection.failedStage] || connection.failedStage}失败` : labels.reason[connection.reason] || "—")))))));
}

function events(tunnel) {
  const list = [...tunnel.events].reverse();
  const details = h("details", { open: ui.openLogs.has(tunnel.id) },
    h("summary", {}, `事件流水（最近 ${list.length} 条）`),
    h("div", { class: "scroll", "data-key": `events-${tunnel.id}` }, h("table", {},
      h("thead", {}, h("tr", {}, ["时间", "连接", "阶段", "结果"].map((title) => h("th", {}, title)))),
      h("tbody", {}, list.map((event) => h("tr", {},
        h("td", { class: "num" }, formatTime(event.time)),
        h("td", { class: "num" }, `#${event.conn}`),
        h("td", {}, labels.stage[event.stage] || event.stage),
        h("td", { class: event.success ? "" : "bad-text" }, event.stage === "closed" ? labels.reason[event.reason] || "—" : event.success ? "成功" : "失败")))))));
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

refresh();
setInterval(() => {
  if (!busy()) refresh();
}, 1000);
