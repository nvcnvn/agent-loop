const state = {
  sessions: [],
  selectedSession: null,
  messages: [],
  runs: [],
  agents: [],
  tools: [],
  events: [],
  requests: [],
  activeRunID: "",
  eventSource: null,
  lastEventID: "",
};

const els = {
  healthText: document.querySelector("#health-text"),
  newSessionForm: document.querySelector("#new-session-form"),
  sessionTitle: document.querySelector("#session-title"),
  refreshSessions: document.querySelector("#refresh-sessions"),
  sessionList: document.querySelector("#session-list"),
  sessionHeading: document.querySelector("#session-heading"),
  agentSelect: document.querySelector("#agent-select"),
  cancelRun: document.querySelector("#cancel-run"),
  messages: document.querySelector("#messages"),
  composer: document.querySelector("#composer"),
  messageInput: document.querySelector("#message-input"),
  sendMessage: document.querySelector("#send-message"),
  startRunToggle: document.querySelector("#start-run-toggle"),
  streamStatus: document.querySelector("#stream-status"),
  eventList: document.querySelector("#event-list"),
  runList: document.querySelector("#run-list"),
  refreshRuns: document.querySelector("#refresh-runs"),
  agentList: document.querySelector("#agent-list"),
  toolList: document.querySelector("#tool-list"),
  requestLog: document.querySelector("#request-log"),
  clearLog: document.querySelector("#clear-log"),
};

const eventTypes = [
  "human_message_created",
  "agent_message_created",
  "agent_trace_recorded",
  "system_event_recorded",
  "model_call_started",
  "model_call_completed",
  "tool_call_scheduled",
  "tool_call_started",
  "tool_call_completed",
  "approval_requested",
  "approval_resolved",
  "snapshot_created",
  "artifact_created",
  "task_spawned",
  "task_completed",
  "run_state_changed",
  "error_recorded",
];

function valueOf(obj, snake, title) {
  if (!obj) return "";
  return obj[snake] ?? obj[title] ?? "";
}

function sessionID(session) {
  return valueOf(session, "id", "ID");
}

function runID(run) {
  return valueOf(run, "id", "ID");
}

function statusOf(run) {
  return valueOf(run, "status", "Status") || "unknown";
}

function titleOf(session) {
  return valueOf(session, "title", "Title") || "Untitled session";
}

function formatTime(value) {
  if (!value || value === "0001-01-01T00:00:00Z") return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString();
}

function pretty(value) {
  if (value === undefined || value === null || value === "") return "";
  if (typeof value === "string") {
    try {
      return JSON.stringify(JSON.parse(value), null, 2);
    } catch {
      return value;
    }
  }
  return JSON.stringify(value, null, 2);
}

function pushRequest(entry) {
  state.requests.unshift({ at: new Date().toISOString(), ...entry });
  state.requests = state.requests.slice(0, 80);
  renderRequests();
}

async function api(path, options = {}) {
  const method = options.method || "GET";
  const requestBody = options.body && typeof options.body !== "string" ? JSON.stringify(options.body) : options.body;
  const headers = { ...(options.headers || {}) };
  if (requestBody && !headers["Content-Type"]) headers["Content-Type"] = "application/json";

  const started = performance.now();
  const response = await fetch(path, { ...options, method, headers, body: requestBody });
  const text = await response.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }

  pushRequest({
    method,
    path,
    status: response.status,
    duration_ms: Math.round(performance.now() - started),
    request: requestBody ? JSON.parse(requestBody) : null,
    response: data,
  });

  if (!response.ok) {
    const message = data?.message || data?.Message || response.statusText;
    throw new Error(message);
  }
  return data;
}

async function refreshHealth() {
  try {
    await fetch("/healthz", { cache: "no-store" });
    els.healthText.textContent = "Server online";
  } catch {
    els.healthText.textContent = "Server unavailable";
    els.healthText.classList.add("error-text");
  }
}

async function loadSetup() {
  const [agents, tools] = await Promise.all([
    api("/v1/debug/agents"),
    api("/v1/debug/tools"),
  ]);
  state.agents = agents?.agents || agents?.Agents || [];
  state.tools = tools?.tools || tools?.Tools || [];
  renderSetup();
}

async function loadSessions(selectFirst = false) {
  const data = await api("/v1/sessions");
  state.sessions = data?.sessions || data?.Sessions || [];
  renderSessions();
  if (!state.selectedSession && selectFirst && state.sessions.length) {
    await selectSession(sessionID(state.sessions[0]));
  }
}

async function selectSession(id) {
  const session = state.sessions.find((item) => sessionID(item) === id) || await api(`/v1/sessions/${id}`);
  state.selectedSession = session;
  state.events = [];
  state.lastEventID = "";
  closeStream();
  renderSessions();
  renderHeader();
  await Promise.all([loadMessages(), loadRuns()]);
  renderEvents();
}

async function loadMessages() {
  if (!state.selectedSession) return;
  const data = await api(`/v1/sessions/${sessionID(state.selectedSession)}/messages`);
  state.messages = data?.messages || data?.Messages || [];
  renderMessages();
}

async function loadRuns() {
  if (!state.selectedSession) return;
  const data = await api(`/v1/sessions/${sessionID(state.selectedSession)}/runs`);
  state.runs = data?.runs || data?.Runs || [];
  renderRuns();
}

async function createSession(title) {
  const data = await api("/v1/sessions", {
    method: "POST",
    body: { title: title || `Debug ${new Date().toLocaleTimeString()}` },
  });
  await loadSessions();
  await selectSession(data.session_id || data.SessionID);
}

function selectedAgentName() {
  return els.agentSelect.value || state.agents[0]?.name || state.agents[0]?.Name || "";
}

function workflowFor(message) {
  return {
    name: "debug-chat",
    stages: [{
      name: "reply",
      kind: "sequential",
      agents: [{
        name: selectedAgentName(),
        prompt: "Respond to the latest user message in this debug chat. Message: {{input.message}}",
      }],
    }],
  };
}

async function sendMessage(content) {
  if (!state.selectedSession || !content.trim()) return;
  const sid = sessionID(state.selectedSession);
  const message = content.trim();
  els.messageInput.value = "";
  await api(`/v1/sessions/${sid}/messages`, {
    method: "POST",
    body: { role: "human", content: message },
  });
  await loadMessages();
  if (!els.startRunToggle.checked) return;

  const data = await api("/v1/runs", {
    method: "POST",
    body: {
      session_id: sid,
      workflow: workflowFor(message),
      input: { message },
      limits: {
        max_model_calls: 8,
        max_tool_calls: 8,
        max_concurrent: 2,
        max_runtime: 900000000000,
      },
    },
  });
  const id = data.run_id || data.RunID;
  state.activeRunID = id;
  els.cancelRun.disabled = false;
  await loadRuns();
  openStream(id);
}

function openStream(id, after = "") {
  closeStream();
  state.lastEventID = after || state.lastEventID || "";
  setStreamStatus("live");
  const url = `/v1/runs/${id}/stream${state.lastEventID ? `?after=${encodeURIComponent(state.lastEventID)}` : ""}`;
  const source = new EventSource(url);
  state.eventSource = source;

  source.onopen = () => setStreamStatus("live");
  source.onerror = () => setStreamStatus("reconnecting");
  source.onmessage = (event) => handleSSEEvent("message", event);
  eventTypes.forEach((type) => source.addEventListener(type, (event) => handleSSEEvent(type, event)));

  pushRequest({
    method: "GET",
    path: url,
    status: "SSE",
    request: { "Last-Event-ID": state.lastEventID || null },
    response: "stream opened",
  });
}

function closeStream() {
  if (state.eventSource) state.eventSource.close();
  state.eventSource = null;
  if (!state.activeRunID) setStreamStatus("idle");
}

function handleSSEEvent(type, event) {
  state.lastEventID = event.lastEventId || state.lastEventID;
  let data = event.data;
  try {
    data = JSON.parse(event.data);
  } catch {
    data = { data: event.data };
  }
  const item = { type, id: event.lastEventId, at: new Date().toISOString(), data };
  state.events.push(item);
  state.events = state.events.slice(-200);
  maybeAppendAssistantMessage(type, data);
  renderEvents();

  const status = data?.payload?.status || data?.payload?.Status;
  if (type === "run_state_changed" && ["succeeded", "failed", "canceled", "timed_out"].includes(status)) {
    setStreamStatus(status);
    els.cancelRun.disabled = true;
    loadRuns().catch(showError);
    loadMessages().catch(showError);
    closeStream();
    state.activeRunID = "";
  }
}

function maybeAppendAssistantMessage(type, data) {
  if (type !== "agent_message_created" && type !== "task_completed") return;
  const payload = data?.payload || {};
  const content = payload.content || payload.message || payload.output || payload.text || "";
  if (!content) return;
  const rendered = String(content);
  if (state.messages.some((message) => valueOf(message, "content", "Content") === rendered)) return;
  state.messages.push({ Role: "assistant", Content: rendered, CreatedAt: new Date().toISOString() });
  renderMessages();
}

async function cancelRun() {
  if (!state.activeRunID) return;
  await api(`/v1/runs/${state.activeRunID}/cancel`, { method: "POST" });
  els.cancelRun.disabled = true;
  await loadRuns();
}

function setStreamStatus(value) {
  els.streamStatus.textContent = value;
  els.streamStatus.classList.toggle("live", value === "live");
}

function showError(error) {
  state.events.push({ type: "ui_error", id: "", at: new Date().toISOString(), data: { message: error.message } });
  renderEvents();
}

function renderSessions() {
  els.sessionList.innerHTML = "";
  if (!state.sessions.length) {
    els.sessionList.append(emptyState("No sessions yet."));
    return;
  }
  const selectedID = state.selectedSession ? sessionID(state.selectedSession) : "";
  state.sessions.forEach((session) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = `session-item${sessionID(session) === selectedID ? " active" : ""}`;
    button.innerHTML = `<span class="session-title"></span><span class="session-meta"></span>`;
    button.querySelector(".session-title").textContent = titleOf(session);
    button.querySelector(".session-meta").textContent = `${sessionID(session).slice(0, 8)} ${formatTime(valueOf(session, "created_at", "CreatedAt"))}`;
    button.addEventListener("click", () => selectSession(sessionID(session)).catch(showError));
    els.sessionList.append(button);
  });
}

function renderHeader() {
  const hasSession = Boolean(state.selectedSession);
  els.sessionHeading.textContent = hasSession ? titleOf(state.selectedSession) : "No session selected";
  els.messageInput.disabled = !hasSession;
  els.sendMessage.disabled = !hasSession;
}

function renderMessages() {
  els.messages.innerHTML = "";
  if (!state.selectedSession) {
    els.messages.append(emptyState("Create or select a session to begin."));
    return;
  }
  if (!state.messages.length) {
    els.messages.append(emptyState("No messages have been sent in this session."));
    return;
  }
  state.messages.forEach((message) => {
    const role = valueOf(message, "role", "Role") || "system";
    const item = document.createElement("article");
    item.className = `message ${role === "human" || role === "user" ? "human" : role === "assistant" ? "assistant" : "system"}`;
    item.innerHTML = `<div class="message-role"></div><div class="message-body"></div>`;
    item.querySelector(".message-role").textContent = role;
    item.querySelector(".message-body").textContent = valueOf(message, "content", "Content") || pretty(message);
    els.messages.append(item);
  });
  els.messages.scrollTop = els.messages.scrollHeight;
}

function renderRuns() {
  els.runList.innerHTML = "";
  if (!state.runs.length) {
    els.runList.append(emptyState("No runs for this session."));
    return;
  }
  [...state.runs].reverse().forEach((run) => {
    const item = document.createElement("div");
    item.className = "debug-item";
    item.innerHTML = `<strong></strong><div class="debug-meta"></div><pre></pre>`;
    item.querySelector("strong").textContent = `${statusOf(run)} ${runID(run).slice(0, 8)}`;
    item.querySelector(".debug-meta").textContent = `queued ${formatTime(valueOf(run, "queued_at", "QueuedAt"))}`;
    item.querySelector("pre").textContent = pretty({ workflow: run.workflow || run.Workflow, input: run.input || run.Input, output: run.output || run.Output, usage: run.usage || run.Usage });
    item.addEventListener("click", () => {
      state.activeRunID = runID(run);
      openStream(state.activeRunID);
    });
    els.runList.append(item);
  });
}

function renderSetup() {
  els.agentSelect.innerHTML = "";
  state.agents.forEach((agent) => {
    const option = document.createElement("option");
    option.value = agent.name || agent.Name;
    option.textContent = `${agent.name || agent.Name} (${agent.model || agent.Model || "model"})`;
    els.agentSelect.append(option);
  });

  els.agentList.innerHTML = "";
  state.agents.forEach((agent) => {
    els.agentList.append(debugItem(agent.name || agent.Name, `${agent.role || agent.Role || "agent"} ${agent.model || agent.Model || ""}`, agent));
  });
  if (!state.agents.length) els.agentList.append(emptyState("No agents reported."));

  els.toolList.innerHTML = "";
  state.tools.forEach((tool) => {
    els.toolList.append(debugItem(tool.name || tool.Name, tool.concurrency || tool.Concurrency || "tool", tool));
  });
  if (!state.tools.length) els.toolList.append(emptyState("No tools reported."));
}

function renderEvents() {
  els.eventList.innerHTML = "";
  if (!state.events.length) {
    els.eventList.append(emptyState("Run stream events will appear here."));
    return;
  }
  [...state.events].reverse().forEach((event) => {
    const item = document.createElement("div");
    item.className = "debug-item";
    item.innerHTML = `<strong></strong><div class="debug-meta"></div><pre></pre>`;
    item.querySelector("strong").textContent = event.type;
    item.querySelector(".debug-meta").textContent = `${event.id || "local"} ${formatTime(event.at)}`;
    item.querySelector("pre").textContent = pretty(event.data);
    els.eventList.append(item);
  });
}

function renderRequests() {
  els.requestLog.innerHTML = "";
  if (!state.requests.length) {
    els.requestLog.append(emptyState("HTTP requests and stream opens will appear here."));
    return;
  }
  state.requests.forEach((request) => {
    const item = document.createElement("div");
    item.className = "debug-item";
    item.innerHTML = `<strong></strong><div class="debug-meta"></div><pre></pre>`;
    item.querySelector("strong").textContent = `${request.method} ${request.path}`;
    item.querySelector(".debug-meta").textContent = `${request.status} ${request.duration_ms ? `${request.duration_ms}ms` : ""} ${formatTime(request.at)}`;
    item.querySelector("pre").textContent = pretty({ request: request.request, response: request.response });
    els.requestLog.append(item);
  });
}

function debugItem(title, meta, data) {
  const item = document.createElement("div");
  item.className = "debug-item";
  item.innerHTML = `<strong></strong><div class="debug-meta"></div><pre></pre>`;
  item.querySelector("strong").textContent = title || "unknown";
  item.querySelector(".debug-meta").textContent = meta || "";
  item.querySelector("pre").textContent = pretty(data);
  return item;
}

function emptyState(text) {
  const node = document.createElement("div");
  node.className = "empty-state";
  node.textContent = text;
  return node;
}

function bindEvents() {
  els.newSessionForm.addEventListener("submit", (event) => {
    event.preventDefault();
    createSession(els.sessionTitle.value.trim()).then(() => {
      els.sessionTitle.value = "";
    }).catch(showError);
  });
  els.refreshSessions.addEventListener("click", () => loadSessions().catch(showError));
  els.refreshRuns.addEventListener("click", () => loadRuns().catch(showError));
  els.cancelRun.addEventListener("click", () => cancelRun().catch(showError));
  els.clearLog.addEventListener("click", () => {
    state.requests = [];
    renderRequests();
  });
  els.composer.addEventListener("submit", (event) => {
    event.preventDefault();
    sendMessage(els.messageInput.value).catch(showError);
  });
  document.querySelectorAll(".tab").forEach((tab) => {
    tab.addEventListener("click", () => {
      document.querySelectorAll(".tab").forEach((item) => item.classList.remove("active"));
      document.querySelectorAll(".tab-panel").forEach((item) => item.classList.remove("active"));
      tab.classList.add("active");
      document.querySelector(`#tab-${tab.dataset.tab}`).classList.add("active");
    });
  });
}

async function init() {
  bindEvents();
  renderHeader();
  renderMessages();
  renderEvents();
  renderRuns();
  renderRequests();
  await refreshHealth();
  await loadSetup();
  await loadSessions(true);
}

init().catch(showError);
