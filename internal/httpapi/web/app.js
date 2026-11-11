const state = {
  token: localStorage.getItem("mayfly.token") || "",
  name: localStorage.getItem("mayfly.name") || "web",
  config: null,
  sessions: [],
  current: null,
  session: null,
  ws: null,
  reconnectTimer: null,
  draft: null,
  artifact: null,
};

const $ = (id) => document.getElementById(id);

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = text;
  return node;
}

function toast(message) {
  const box = $("toast");
  box.textContent = message;
  box.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { box.hidden = true; }, 7000);
}

async function api(path, options = {}) {
  const headers = Object.assign({}, options.headers || {});
  if (state.token) headers.Authorization = "Bearer " + state.token;
  if (options.body && typeof options.body !== "string") {
    headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(options.body);
  }
  const res = await fetch(path, Object.assign({}, options, { headers }));
  if (res.status === 204) return null;
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = { error: text }; }
  }
  if (!res.ok) {
    if (res.status === 401) openSettings();
    throw new Error((data && data.error) || ("HTTP " + res.status));
  }
  return data;
}

function setConn(text, live) {
  const node = $("conn");
  node.textContent = text;
  node.className = "conn" + (live ? " live" : "");
}

function openSettings() {
  $("token-input").value = state.token;
  $("name-input").value = state.name;
  $("auth-dialog").showModal();
}

async function loadConfig() {
  state.config = await api("/api/v1/config");
  const select = $("create-model");
  select.textContent = "";
  const models = Array.from(new Set(state.config.allowed_models || [state.config.default_model]));
  for (const model of models) {
    const option = el("option", null, model);
    option.value = model;
    if (model === state.config.default_model) option.selected = true;
    select.appendChild(option);
  }
  $("create-ttl").value = formatSeconds(state.config.default_ttl_seconds || 1800);
  fillAgentSelect(state.config.agents || []);
  fillChecks($("create-skills"), state.config.skills || []);
  fillChecks($("create-mcp"), state.config.mcp_servers || []);
}

function fillAgentSelect(agents) {
  const select = $("create-agent");
  select.textContent = "";
  if (!agents.length) {
    const option = el("option", null, "default");
    option.value = "default";
    select.appendChild(option);
    return;
  }
  for (const agent of agents) {
    const option = el("option", null, agent.name + (agent.builtin ? " (built-in)" : ""));
    option.value = agent.name;
    if (agent.default) option.selected = true;
    select.appendChild(option);
  }
}

function fillChecks(container, names) {
  container.textContent = "";
  if (!names.length) {
    container.appendChild(el("span", "hint", "None configured"));
    return;
  }
  for (const name of names) {
    const label = el("label");
    const input = document.createElement("input");
    input.type = "checkbox";
    input.value = name;
    label.appendChild(input);
    label.appendChild(document.createTextNode(name));
    container.appendChild(label);
  }
}

function formatSeconds(seconds) {
  if (seconds % 3600 === 0) return (seconds / 3600) + "h";
  if (seconds % 60 === 0) return (seconds / 60) + "m";
  return seconds + "s";
}

function checkedValues(container) {
  return Array.from(container.querySelectorAll("input:checked")).map((input) => input.value);
}

async function loadSessions() {
  const data = await api("/api/v1/sessions");
  state.sessions = data.sessions || [];
  renderSessions();
  if (state.current) {
    const found = state.sessions.find((s) => s.id === state.current);
    if (found) updateHeader(found);
  }
}

function renderSessions() {
  const box = $("sessions");
  box.textContent = "";
  if (!state.sessions.length) {
    box.appendChild(el("p", "hint", "No sessions yet."));
    return;
  }
  for (const sess of state.sessions) {
    const item = el("div", "session-item" + (sess.id === state.current ? " active" : ""));
    item.appendChild(el("div", "title", sess.title || sess.id));
    const sub = el("div", "sub");
    sub.appendChild(el("span", null, sess.status));
    sub.appendChild(el("span", null, new Date(sess.created_at).toLocaleTimeString()));
    item.appendChild(sub);
    item.addEventListener("click", () => openSession(sess.id));
    box.appendChild(item);
  }
}

function updateHeader(sess) {
  state.session = sess;
  state.artifact = sess.artifact || state.artifact;
  $("session-title").textContent = sess.title || "Session " + sess.id;
  const badge = $("session-status");
  badge.textContent = sess.status;
  badge.className = "badge " + sess.status;
  $("session-id").textContent = sess.id;
  $("session-agent").textContent = sess.agent || "default";
  $("session-harness").textContent = sess.harness || "mayfly";
  $("download").hidden = !state.artifact;
  const terminal = ["completed", "failed", "expired"].includes(sess.status);
  $("close").disabled = terminal;
  $("composer").querySelector("textarea").disabled = terminal;
}

async function openSession(id) {
  if (state.current === id && state.ws && state.ws.readyState === WebSocket.OPEN) return;
  state.current = id;
  state.artifact = null;
  $("empty").hidden = true;
  $("session").hidden = false;
  $("events").textContent = "";
  state.draft = null;
  if (location.hash !== "#/sessions/" + id) {
    history.replaceState(null, "", "#/sessions/" + id);
  }
  renderSessions();
  const sess = state.sessions.find((s) => s.id === id);
  if (sess) updateHeader(sess);
  else {
    const data = await api("/api/v1/sessions/" + encodeURIComponent(id));
    updateHeader(data.session);
  }
  connect(id);
}

function closeSocket() {
  if (state.reconnectTimer) {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
  }
  if (state.ws) {
    const ws = state.ws;
    state.ws = null;
    ws.onclose = null;
    ws.close();
  }
}

async function connect(id) {
  closeSocket();
  state.draft = null;
  $("events").textContent = "";
  setConn("connecting…", false);
  let ticket;
  try {
    ticket = await api("/api/v1/sessions/" + encodeURIComponent(id) + "/attach-ticket", {
      method: "POST",
      body: { actor: state.name },
    });
  } catch (err) {
    toast(err.message);
    return;
  }
  const ws = new WebSocket(ticket.ws_url);
  state.ws = ws;
  ws.onopen = () => setConn("live", true);
  ws.onmessage = (msg) => {
    let payload;
    try { payload = JSON.parse(msg.data); } catch { return; }
    handlePayload(payload);
  };
  ws.onerror = () => setConn("error", false);
  ws.onclose = () => {
    if (state.ws !== ws) return;
    state.ws = null;
    setConn("disconnected", false);
    const sess = state.session;
    const terminal = sess && ["completed", "failed", "expired"].includes(sess.status);
    if (!terminal && state.current === id) {
      state.reconnectTimer = setTimeout(() => connect(id), 2500);
    }
  };
}

function handlePayload(payload) {
  if (payload.type === "hello") {
    updateHeader(payload.session);
    return;
  }
  if (payload.type === "error") {
    toast(payload.message);
    return;
  }
  if (payload.type !== "event") return;
  const ev = payload.event;
  const events = $("events");
  const stick = events.scrollHeight - events.scrollTop - events.clientHeight < 140;

  switch (ev.type) {
    case "user_message":
      addEvent("user", ev.actor, ev.text);
      break;
    case "assistant_delta": {
      if (!state.draft) {
        state.draft = addEvent("assistant", "agent", "", true);
      }
      state.draft.textNode.textContent += ev.text || "";
      break;
    }
    case "assistant_message": {
      if (state.draft) {
        state.draft.textNode.textContent = ev.text || "";
        state.draft = null;
      } else if (ev.text) {
        addEvent("assistant", "agent", ev.text);
      }
      break;
    }
    case "tool_call": {
      const data = ev.data || {};
      const box = el("div", "event tool");
      box.appendChild(el("div", "who", "tool call · " + (data.tool || "unknown")));
      const details = el("details");
      details.appendChild(el("summary", null, "arguments"));
      const pre = el("pre", null, prettyJSON(data.arguments));
      details.appendChild(pre);
      box.appendChild(details);
      events.appendChild(box);
      break;
    }
    case "tool_result": {
      const data = ev.data || {};
      const box = el("div", "event tool");
      box.appendChild(el("div", "who", "tool result · " + (data.tool || "unknown") + (data.error ? " · error" : "")));
      const details = el("details");
      details.appendChild(el("summary", null, "output"));
      details.appendChild(el("pre", null, ev.text || ""));
      box.appendChild(details);
      events.appendChild(box);
      break;
    }
    case "status": {
      addEvent("status", "status", ev.text || (ev.data && ev.data.status) || "");
      if (ev.data && ev.data.status && state.session) {
        const next = Object.assign({}, state.session, { status: ev.data.status });
        updateHeader(next);
        if (["completed", "failed", "expired"].includes(ev.data.status)) {
          closeSocket();
          loadSessions().catch(() => {});
        }
      }
      break;
    }
    case "artifact": {
      addEvent("artifact", "artifact", ev.text || "Artifact exported.", false);
      if (ev.data) {
        state.artifact = ev.data;
        $("download").hidden = false;
      }
      break;
    }
    case "error":
      addEvent("error", ev.actor || "error", ev.text || "error");
      break;
    case "system":
      addEvent("system", "system", ev.text || "");
      break;
    default:
      if (ev.text) addEvent("system", ev.type, ev.text);
  }

  if (stick) events.scrollTop = events.scrollHeight;
}

function addEvent(kind, who, text, returnRefs) {
  const box = el("div", "event " + kind);
  if (who) box.appendChild(el("div", "who", who));
  const body = el("div");
  body.textContent = text || "";
  box.appendChild(body);
  $("events").appendChild(box);
  if (returnRefs) return { box, textNode: body };
  return box;
}

function prettyJSON(value) {
  if (value === undefined || value === null) return "";
  try {
    return JSON.stringify(typeof value === "string" ? JSON.parse(value) : value, null, 2);
  } catch {
    return String(value);
  }
}

async function sendMessage(text) {
  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
    toast("Not connected.");
    return;
  }
  text = text.trim();
  if (!text) return;
  state.ws.send(JSON.stringify({ type: "user_message", text }));
}

async function downloadArtifact() {
  if (!state.current) return;
  const res = await fetch("/api/v1/sessions/" + encodeURIComponent(state.current) + "/artifact", {
    headers: { Authorization: "Bearer " + state.token },
  });
  if (!res.ok) {
    toast("Artifact is not available yet.");
    return;
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = "mayfly-" + state.current + ".tar.gz";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

function bindUI() {
  $("settings").addEventListener("click", openSettings);
  $("refresh").addEventListener("click", () => { loadSessions().catch((e) => toast(e.message)); });

  $("auth-form").addEventListener("submit", (event) => {
    event.preventDefault();
    const token = $("token-input").value.trim();
    const name = $("name-input").value.trim() || "web";
    if (!token) return;
    state.token = token;
    state.name = name;
    localStorage.setItem("mayfly.token", token);
    localStorage.setItem("mayfly.name", name);
    $("auth-dialog").close();
    initData();
  });
  $("auth-cancel").addEventListener("click", () => $("auth-dialog").close());

  $("new-session").addEventListener("click", () => {
    if (!state.config) {
      loadConfig().then(() => $("create-dialog").showModal()).catch((e) => toast(e.message));
      return;
    }
    $("create-dialog").showModal();
  });
  $("create-cancel").addEventListener("click", () => $("create-dialog").close());

  $("create-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = $("create-form").querySelector("button.primary");
    button.disabled = true;
    try {
      const data = await api("/api/v1/sessions", {
        method: "POST",
        body: {
          prompt: $("create-prompt").value,
          title: $("create-title").value,
          ttl: $("create-ttl").value,
          model: $("create-model").value,
          agent: $("create-agent").value,
          skills: checkedValues($("create-skills")),
          mcp_servers: checkedValues($("create-mcp")),
        },
      });
      $("create-dialog").close();
      $("create-prompt").value = "";
      $("create-title").value = "";
      await loadSessions();
      openSession(data.session.id);
    } catch (err) {
      toast(err.message);
    } finally {
      button.disabled = false;
    }
  });

  $("composer").addEventListener("submit", (event) => {
    event.preventDefault();
    const input = $("message");
    const text = input.value;
    input.value = "";
    sendMessage(text).catch((e) => toast(e.message));
  });
  $("message").addEventListener("keydown", (event) => {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      $("composer").requestSubmit();
    }
  });

  $("close").addEventListener("click", async () => {
    if (!state.current) return;
    $("close").disabled = true;
    try {
      await api("/api/v1/sessions/" + encodeURIComponent(state.current) + "/close", {
        method: "POST",
        body: { reason: "closed" },
      });
      $("close").disabled = true;
    } catch (err) {
      $("close").disabled = false;
      toast(err.message);
    }
  });

  $("download").addEventListener("click", downloadArtifact);

  window.addEventListener("hashchange", () => {
    const id = (location.hash.match(/^#\/sessions\/([a-zA-Z0-9]+)$/) || [])[1];
    if (id && id !== state.current) openSession(id);
  });
}

async function initData() {
  if (!state.token) {
    openSettings();
    return;
  }
  try {
    if (!state.config) await loadConfig();
    await loadSessions();
    const id = (location.hash.match(/^#\/sessions\/([a-zA-Z0-9]+)$/) || [])[1];
    if (id) openSession(id);
  } catch (err) {
    toast(err.message);
  }
}

bindUI();
initData();
setInterval(() => { if (state.current) loadSessions().catch(() => {}); }, 15000);
