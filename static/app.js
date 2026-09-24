const $ = (selector, root = document) => root.querySelector(selector);
const state = {
  sessions: [], selectedId: null, connected: false,
  events: new Map(), cursors: new Map(), sentMessages: new Map(),
  tasks: new Map(), diffs: new Map(), stream: null, toastTimer: null, refreshTimer: null,
  sessionSignature: "",
};
const sessionList = $("#session-list");
const messageList = $("#message-list");
const sidebar = $("#session-sidebar");
const backdrop = $("#drawer-backdrop");
const composer = $("#instruction-text");
const sessionInfo = $("#session-info");

const eventTitles = {
  SessionStarted: "Pi に接続しました",
  TaskStarted: "実行を開始しました",
  ToolStarted: "ツールを実行中です",
  ToolFinished: "ツールの実行が終わりました",
  TaskUpdated: "進捗を更新しました",
  VerificationFailed: "検証で問題を検出しました",
  SessionCompleted: "セッションを終了しました",
};

function text(value, fallback = "—") {
  return typeof value === "string" && value.trim() ? value : fallback;
}
function shortPath(value) {
  if (!value) return "作業ディレクトリなし";
  return value.replace(/\\/g, "/").replace(/\/$/, "").split("/").filter(Boolean).pop() || value;
}
function formatTime(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat("ja-JP", { hour: "2-digit", minute: "2-digit" }).format(date);
}
function relativeTime(value) {
  if (!value) return "更新なし";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "更新なし";
  const minutes = Math.max(0, Math.floor((Date.now() - date.getTime()) / 60000));
  if (minutes < 1) return "たった今";
  if (minutes < 60) return `${minutes} 分前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 時間前`;
  return `${Math.floor(hours / 24)} 日前`;
}
function statusFor(session) {
  if (!session.connected) return "offline";
  return ["working", "completed", "idle"].includes(session.status) ? session.status : "idle";
}
function statusLabel(status) {
  return ({ working: "作業中", completed: "完了", idle: "待機中", offline: "切断" })[status] || "待機中";
}
function formatCount(value) {
  return Number.isFinite(Number(value)) ? new Intl.NumberFormat("ja-JP").format(Number(value)) : "—";
}
function formatCost(value) {
  return Number.isFinite(Number(value)) ? `$${Number(value).toFixed(3)}` : "—";
}
function taskStatusLabel(status) {
  return ({ todo: "未着手", doing: "進行中", done: "完了" })[status] || status;
}

async function api(url, options = {}) {
  const response = await fetch(url, {
    ...options,
    headers: { ...(options.body ? { "Content-Type": "application/json" } : {}), ...options.headers },
    cache: "no-store",
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(payload.error || `リクエストに失敗しました（${response.status}）`);
  return payload;
}

function showToast(message, kind = "success") {
  const toast = $("#toast");
  toast.textContent = message;
  toast.dataset.kind = kind;
  toast.dataset.visible = "true";
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => { toast.dataset.visible = "false"; }, 3200);
}

function setConnection(connected) {
  state.connected = connected;
  const indicator = $("#connection-state");
  indicator.dataset.state = connected ? "online" : "offline";
  indicator.lastElementChild.textContent = connected ? "bridge 接続中" : "bridge 切断中";
}

function renderSessionList() {
  $("#session-count").textContent = String(state.sessions.length);
  sessionList.replaceChildren();
  $("#empty-sessions").hidden = state.sessions.length !== 0;

  for (const session of state.sessions) {
    const status = statusFor(session);
    const button = document.createElement("button");
    button.type = "button";
    button.className = "session-card";
    button.setAttribute("aria-current", String(session.sessionId === state.selectedId));
    button.setAttribute("aria-label", `${text(session.name, shortPath(session.cwd))}、${statusLabel(status)}`);
    button.addEventListener("click", () => selectSession(session.sessionId));

    const top = document.createElement("span");
    top.className = "session-card-top";
    const name = document.createElement("span");
    name.className = "session-name";
    name.textContent = text(session.name, shortPath(session.cwd));
    const statusNode = document.createElement("span");
    statusNode.className = "session-status";
    statusNode.dataset.status = status;
    statusNode.textContent = statusLabel(status);
    top.append(name, statusNode);

    const path = document.createElement("span");
    path.className = "session-path";
    path.textContent = text(session.cwd, "作業ディレクトリなし");
    const updated = document.createElement("span");
    updated.className = "session-updated";
    updated.textContent = relativeTime(session.updatedAt);
    button.append(top, path, updated);
    sessionList.append(button);
  }
}

function updateHeader(session) {
  const name = $("#current-name");
  const path = $("#current-path");
  const badge = $("#session-status");
  if (!session) {
    name.textContent = "セッションを選択";
    path.textContent = "Pi の作業状況をここに表示します";
    if (badge) badge.hidden = true;
    composer.disabled = true;
    composer.placeholder = "Pi セッションを選ぶと指示を送れます";
    $("#send-button").disabled = true;
    $("#composer-hint").textContent = "Pi セッションを選ぶと指示を送れます。";
    $("#diff-button").disabled = true;
    return;
  }
  name.textContent = text(session.name, shortPath(session.cwd));
  path.textContent = text(session.cwd, "作業ディレクトリなし");
  if (badge) {
    const status = statusFor(session);
    badge.hidden = false;
    badge.dataset.status = status;
    badge.textContent = statusLabel(status);
  }
  composer.disabled = !session.connected;
  composer.placeholder = session.connected ? "続けてほしい作業や、確認してほしい点を書く" : "Pi が接続すると指示を送れます";
  $("#send-button").disabled = !session.connected;
  $("#composer-hint").textContent = session.connected ? "指示は Pi への follow-up として送信します。" : "このセッションは切断中です。";
  $("#diff-button").disabled = !session.connected;
  $("#info-path").textContent = text(session.cwd, "作業ディレクトリなし");
  const telemetry = session.telemetry || {};
  $("#metric-tools").textContent = formatCount(telemetry.toolCalls);
  $("#metric-runs").textContent = formatCount(telemetry.runs);
  $("#metric-cost").textContent = formatCost(telemetry.mainModelCostUsd);
}

function toggleDrawer(open) {
  const shouldOpen = typeof open === "boolean" ? open : !sidebar.classList.contains("is-open");
  sidebar.classList.toggle("is-open", shouldOpen);
  backdrop.hidden = !shouldOpen;
  $("#sessions-toggle").setAttribute("aria-expanded", String(shouldOpen));
  $("#sessions-toggle").setAttribute("aria-label", shouldOpen ? "セッション一覧を閉じる" : "セッション一覧を開く");
}

function showEmptyThread(hasSession) {
  const empty = document.createElement("div");
  empty.className = "chat-empty";
  const avatar = document.createElement("span");
  avatar.className = "empty-avatar";
  avatar.setAttribute("aria-hidden", "true");
  avatar.textContent = "π";
  const heading = document.createElement("h2");
  heading.textContent = hasSession ? "まだイベントはありません" : "Pi と作業を続ける";
  const description = document.createElement("p");
  description.textContent = hasSession
    ? "Pi が作業を始めると、ここに状況が表示されます。下の欄からフォローアップも送れます。"
    : "セッションを選ぶと、作業イベントと送信した指示がここに並びます。";
  empty.append(avatar, heading, description);
  messageList.append(empty);
}

function eventSummary(item) {
  const summary = text(item.summary, "イベントを受信しました。");
  if (summary === "Pi run started") return "Pi が作業を開始しました。";
  if (summary === "Pi run settled") return "Pi の実行が完了しました。";
  if (summary === "Subagent settled") return "サブエージェントの処理が完了しました。";
  if (summary.startsWith("External model cost: ")) return `外部モデルの費用を記録しました。${summary.slice("External model cost: ".length)}`;
  return summary;
}

function makePiMessage(item) {
  const row = document.createElement("article");
  row.className = "message-row pi-message";
  row.dataset.kind = text(item.type, "event");
  if (item.type === "VerificationFailed") row.classList.add("is-warning");

  const avatar = document.createElement("span");
  avatar.className = "message-avatar";
  avatar.setAttribute("aria-hidden", "true");
  avatar.textContent = "π";

  const stack = document.createElement("div");
  stack.className = "message-stack";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const sender = document.createElement("strong");
  sender.textContent = "Pi の作業ログ";
  const time = document.createElement("time");
  time.textContent = formatTime(item.at);
  if (item.at) time.dateTime = item.at;
  meta.append(sender, time);

  const bubble = document.createElement("div");
  bubble.className = "message-bubble pi-bubble";
  const label = document.createElement("span");
  label.className = "event-label";
  label.textContent = eventTitles[item.type] || "作業イベント";
  const summary = document.createElement("p");
  summary.className = "bubble-text";
  summary.textContent = eventSummary(item);
  bubble.append(label, summary);
  stack.append(meta, bubble);
  row.append(avatar, stack);
  return row;
}

function makeUserMessage(item) {
  const row = document.createElement("article");
  row.className = "message-row user-message";
  const stack = document.createElement("div");
  stack.className = "message-stack";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const time = document.createElement("time");
  time.textContent = formatTime(item.at);
  if (item.at) time.dateTime = item.at;
  const sender = document.createElement("strong");
  sender.textContent = "送信した指示";
  meta.append(time, sender);
  const bubble = document.createElement("div");
  bubble.className = "message-bubble user-bubble";
  const content = document.createElement("p");
  content.className = "bubble-text";
  content.textContent = item.text;
  bubble.append(content);
  stack.append(meta, bubble);
  row.append(stack);
  return row;
}

function renderMessages(scrollToBottom = false) {
  const wasNearBottom = messageList.scrollHeight - messageList.clientHeight - messageList.scrollTop < 90;
  messageList.replaceChildren();
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  const events = state.events.get(state.selectedId) || [];
  const sent = state.sentMessages.get(state.selectedId) || [];
  const messages = [
    ...events.map((item) => ({ ...item, messageRole: "pi", sortTime: Date.parse(item.at) || 0 })),
    ...sent.map((item) => ({ ...item, messageRole: "user", sortTime: Date.parse(item.at) || 0 })),
  ].sort((a, b) => a.sortTime - b.sortTime);

  if (!messages.length) {
    showEmptyThread(Boolean(session));
    return;
  }
  for (const item of messages) {
    messageList.append(item.messageRole === "user" ? makeUserMessage(item) : makePiMessage(item));
  }
  if (scrollToBottom || wasNearBottom) messageList.scrollTop = messageList.scrollHeight;
}

function renderTasks() {
  const list = $("#task-list");
  list.replaceChildren();
  const snapshot = state.tasks.get(state.selectedId);
  if (!snapshot || !snapshot.snapshotFound) {
    $("#task-count").textContent = snapshot ? "未取得" : "未読み込み";
    const empty = document.createElement("p");
    empty.className = "info-empty";
    empty.textContent = snapshot ? "このセッションにタスクの記録はありません。" : "セッションを選ぶとタスクを読み込みます。";
    list.append(empty);
    return;
  }
  const tasks = snapshot.tasks || [];
  const unfinished = tasks.filter((task) => task.status !== "done").length;
  $("#task-count").textContent = `${unfinished} 件進行中`;
  if (!tasks.length) {
    const empty = document.createElement("p");
    empty.className = "info-empty";
    empty.textContent = "タスクはありません。";
    list.append(empty);
    return;
  }
  for (const task of tasks.slice(0, 50)) {
    const row = document.createElement("div");
    row.className = "task-item";
    row.dataset.status = task.status;
    const marker = document.createElement("span");
    marker.className = "task-marker";
    marker.setAttribute("aria-hidden", "true");
    const body = document.createElement("div");
    const title = document.createElement("p");
    title.className = "task-title";
    title.textContent = text(task.title, "無題のタスク");
    const status = document.createElement("small");
    status.className = "task-status";
    status.textContent = taskStatusLabel(task.status);
    body.append(title, status);
    row.append(marker, body);
    list.append(row);
  }
}

function renderDiff() {
  const output = $("#diff-summary");
  const result = state.diffs.get(state.selectedId);
  if (!result) return;
  output.textContent = result.summary || "差分はありません。";
}

async function selectSession(sessionId) {
  if (state.selectedId === sessionId) {
    toggleDrawer(false);
    return;
  }
  state.selectedId = sessionId;
  toggleDrawer(false);
  renderSessionList();
  updateHeader(state.sessions.find((item) => item.sessionId === sessionId));
  renderMessages(true);
  await refreshDetails(true);
}

async function refreshDetails(reset = false) {
  const sessionId = state.selectedId;
  if (!sessionId) return;
  const session = state.sessions.find((item) => item.sessionId === sessionId);
  if (!session) return;
  if (reset) {
    state.events.set(sessionId, []);
    state.cursors.set(sessionId, 0);
    state.tasks.delete(sessionId);
    state.diffs.delete(sessionId);
    renderMessages();
    renderTasks();
    $("#diff-summary").textContent = "必要なときに取得します。";
  }

  const after = state.cursors.get(sessionId) || 0;
  try {
    const result = await api(`/api/sessions/${encodeURIComponent(sessionId)}/events?after=${after}`);
    const previous = state.events.get(sessionId) || [];
    if (result.dropped) {
      state.events.set(sessionId, []);
      showToast("古いイベントは bridge の保持範囲外です。取得できた分を表示します。");
    }
    const base = state.events.get(sessionId) || previous;
    const merged = [...base, ...(result.events || [])];
    const unique = new Map(merged.map((item) => [item.seq, item]));
    const next = [...unique.values()].slice(-100);
    const changed = next.length !== previous.length || result.dropped;
    state.events.set(sessionId, next);
    state.cursors.set(sessionId, Number.isSafeInteger(result.cursor) ? result.cursor : after);
    if (changed && state.selectedId === sessionId) renderMessages();
  } catch (error) {
    if (reset && state.selectedId === sessionId) showToast(error.message, "error");
  }

  if (!session.connected) {
    renderTasks();
    return;
  }
  try {
    const tasks = await api(`/api/sessions/${encodeURIComponent(sessionId)}/tasks`);
    state.tasks.set(sessionId, tasks);
    if (state.selectedId === sessionId) renderTasks();
  } catch (error) {
    if (reset && state.selectedId === sessionId) showToast(error.message, "error");
  }
}

async function loadDiff() {
  const sessionId = state.selectedId;
  if (!sessionId) return;
  const button = $("#diff-button");
  button.disabled = true;
  button.textContent = "取得中…";
  try {
    const result = await api(`/api/sessions/${encodeURIComponent(sessionId)}/diff`);
    state.diffs.set(sessionId, result);
    if (state.selectedId === sessionId) renderDiff();
  } catch (error) {
    showToast(error.message, "error");
  } finally {
    if (button.isConnected) {
      button.disabled = !state.sessions.find((item) => item.sessionId === sessionId)?.connected;
      button.textContent = "取得";
    }
  }
}

async function sendInstruction(event) {
  event.preventDefault();
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  const button = $("#send-button");
  const value = composer.value;
  if (!session?.connected || !value.trim()) return;
  button.disabled = true;
  button.classList.add("is-sending");
  button.setAttribute("aria-label", "送信中");
  try {
    const result = await api(`/api/sessions/${encodeURIComponent(session.sessionId)}/instruction`, {
      method: "POST", body: JSON.stringify({ text: value }),
    });
    const sent = state.sentMessages.get(session.sessionId) || [];
    sent.push({ id: `local-${Date.now()}`, at: new Date().toISOString(), text: value });
    state.sentMessages.set(session.sessionId, sent);
    composer.value = "";
    composer.style.height = "auto";
    renderMessages(true);
    showToast(result.accepted ? "Pi に指示を送りました。" : "Pi が指示を受け付けました。");
  } catch (error) {
    showToast(error.message, "error");
  } finally {
    if (button.isConnected) {
      button.disabled = !state.sessions.find((item) => item.sessionId === session.sessionId)?.connected;
      button.classList.remove("is-sending");
      button.setAttribute("aria-label", "Pi に指示を送信");
    }
  }
}

function applySnapshot(payload) {
  const previousSelection = state.selectedId;
  const nextSessions = Array.isArray(payload.sessions) ? payload.sessions : [];
  const signature = JSON.stringify(nextSessions);
  state.sessions = nextSessions;
  setConnection(payload.connected === true);
  if (!state.selectedId || !state.sessions.some((session) => session.sessionId === state.selectedId)) {
    state.selectedId = state.sessions[0]?.sessionId || null;
  }
  if (signature !== state.sessionSignature) {
    state.sessionSignature = signature;
    renderSessionList();
  }
  const selected = state.sessions.find((session) => session.sessionId === state.selectedId);
  updateHeader(selected);
  if (previousSelection !== state.selectedId) {
    renderMessages(true);
    if (state.selectedId) void refreshDetails(true);
  }
}

function connectLive() {
  const stream = new EventSource("/api/live");
  state.stream = stream;
  stream.addEventListener("message", (event) => {
    try { applySnapshot(JSON.parse(event.data)); } catch { /* Ignore malformed bridge updates. */ }
  });
  stream.addEventListener("error", () => setConnection(false));
}

async function refreshSessions() {
  try {
    const sessions = await api("/api/sessions");
    applySnapshot({ connected: true, sessions });
  } catch {
    setConnection(false);
  }
}

$("#sessions-toggle").addEventListener("click", () => toggleDrawer());
backdrop.addEventListener("click", () => toggleDrawer(false));
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") toggleDrawer(false);
});
$("#refresh-button").addEventListener("click", refreshSessions);
$("#session-info-button").addEventListener("click", () => sessionInfo.showModal());
$("#close-info").addEventListener("click", () => sessionInfo.close());
sessionInfo.addEventListener("click", (event) => {
  if (event.target === sessionInfo) sessionInfo.close();
});
$("#diff-button").addEventListener("click", loadDiff);
$("#instruction-form").addEventListener("submit", sendInstruction);
composer.addEventListener("input", () => {
  composer.style.height = "auto";
  composer.style.height = `${Math.min(composer.scrollHeight, 176)}px`;
});
composer.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    $("#instruction-form").requestSubmit();
  }
});
connectLive();
refreshSessions();
state.refreshTimer = setInterval(() => refreshDetails(false), 8000);
if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => navigator.serviceWorker.register("/sw.js").catch(() => {}));
}
