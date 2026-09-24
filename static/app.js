const $ = (selector, root = document) => root.querySelector(selector);
const state = {
  sessions: [], selectedId: null, connected: false,
  events: new Map(), cursors: new Map(), sentMessages: new Map(), transcripts: new Map(), transcriptCursors: new Map(), detailRefreshes: new Map(),
  drafts: new Map(), scrollPositions: new Map(),
  transcriptEnabled: true, transcriptLoadingFor: null, transcriptRequest: null,
  tasks: new Map(), diffs: new Map(), models: new Map(), codexUsage: new Map(), loadingCodexUsage: new Set(), loadingModels: new Set(), modelSwitching: false,
  stream: null, liveConnected: false, toastTimer: null, detailRefreshAgain: new Set(),
  sessionSignature: "",
  sidebarMode: "live", historyFolders: [], historyLoaded: false,
  herdr: null, herdrLoaded: false, selectedHerdrWorkspaceId: null, selectedHerdrTabId: null, selectedHerdrPaneId: null,
  historySelection: null, herdrPaneRequest: null,
};
const MAX_TRANSCRIPT_MESSAGES = 200;
const MAX_TRANSCRIPT_JSON_BYTES = 6 * 1024 * 1024;
const transcriptEncoder = new TextEncoder();
const sessionList = $("#session-list");
const messageList = $("#message-list");
const sidebar = $("#session-sidebar");
const backdrop = $("#drawer-backdrop");
const composer = $("#instruction-text");
const sessionInfo = $("#session-info");
const sidebarLabel = $("#sidebar-label");

const eventTitles = {
  SessionStarted: "接続",
  TaskStarted: "開始",
  ToolStarted: "ツール開始",
  ToolFinished: "ツール完了",
  TaskUpdated: "更新",
  VerificationFailed: "検証",
  SessionCompleted: "終了",
  ModelChanged: "モデル切替",
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
function formatCompactCount(value) {
  if (!Number.isFinite(Number(value)) || Number(value) < 0) return "—";
  return new Intl.NumberFormat("ja-JP", { notation: "compact", maximumFractionDigits: 1 }).format(Number(value));
}
function taskStatusLabel(status) {
  return ({ todo: "未着手", doing: "進行中", done: "完了" })[status] || status;
}
function capTranscript(messages) {
  const kept = [];
  let jsonBytes = 2; // `[]`
  let truncated = false;
  for (let index = messages.length - 1; index >= 0; index--) {
    if (kept.length >= MAX_TRANSCRIPT_MESSAGES) {
      truncated = true;
      break;
    }
    let encoded;
    try { encoded = JSON.stringify(messages[index]); } catch { encoded = null; }
    if (typeof encoded !== "string") {
      truncated = true;
      break;
    }
    const itemBytes = transcriptEncoder.encode(encoded).byteLength;
    const additionalBytes = itemBytes + (kept.length ? 1 : 0);
    if (jsonBytes + additionalBytes > MAX_TRANSCRIPT_JSON_BYTES) {
      truncated = true;
      break;
    }
    jsonBytes += additionalBytes;
    kept.push(messages[index]);
  }
  if (kept.length < messages.length) truncated = true;
  return { messages: kept.reverse(), truncated };
}

async function api(url, options = {}) {
  const response = await fetch(url, {
    ...options,
    headers: { ...(options.body ? { "Content-Type": "application/json" } : {}), ...options.headers },
    cache: "no-store",
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(payload.error || `リクエストに失敗しました（${response.status}）`);
    error.code = typeof payload.code === "string" ? payload.code : "";
    throw error;
  }
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
  const label = connected ? "接続" : "切断";
  indicator.lastElementChild.textContent = label;
  indicator.setAttribute("aria-label", label);
  indicator.title = label;
}

function renderSessionList() {
  const liveSessions = state.sessions.filter((session) => session.historyOnly !== true);
  $("#session-count").textContent = String(liveSessions.length);
  sessionList.replaceChildren();
  $("#empty-sessions").hidden = liveSessions.length !== 0;

  for (const session of liveSessions) {
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

function currentModelFor(session) {
  const ref = session?.currentModel || state.models.get(session?.sessionId)?.current;
  if (!ref || typeof ref.provider !== "string" || typeof ref.id !== "string") return null;
  const catalog = state.models.get(session.sessionId);
  return catalog?.models?.find((item) => item.provider === ref.provider && item.id === ref.id) || ref;
}

function isCodexProvider(provider) {
  return provider === "openai-codex" || /^openai-codex-[0-9]+$/.test(provider || "");
}

function formatResetTime(value) {
  if (!Number.isSafeInteger(value) || value <= 0) return "";
  const date = new Date(value * 1000);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat("ja-JP", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(date);
}

function updateCodexUsage(session) {
  const section = $("#codex-usage-section");
  const button = $("#codex-usage-refresh");
  const model = currentModelFor(session);
  const supported = isCodexProvider(model?.provider);
  section.hidden = !supported;
  const loading = Boolean(session && state.loadingCodexUsage.has(session.sessionId));
  button.disabled = !session?.connected || !supported || loading;
  button.textContent = loading ? "取得中" : state.codexUsage.has(session?.sessionId) ? "更新" : "取得";
  const snapshot = session ? state.codexUsage.get(session.sessionId) : null;
  for (const [label, prefix] of [["5h", "codex-5h"], ["7d", "codex-7d"]]) {
    const window = snapshot?.[label === "5h" ? "fiveHour" : "weekly"];
    const percent = Number.isFinite(window?.usedPercent) ? Math.max(0, Math.min(100, window.usedPercent)) : null;
    const track = $(`#${prefix}-track`);
    $(`#${prefix}-fill`).style.width = percent === null ? "0%" : `${percent}%`;
    $(`#${prefix}-value`).textContent = percent === null ? "—" : `${Math.round(percent)}%`;
    if (percent === null) track.removeAttribute("aria-valuenow");
    else track.setAttribute("aria-valuenow", String(Math.round(percent)));
    $(`#${prefix}-reset`).textContent = formatResetTime(window?.resetAt);
  }
}

function updateUsage(session) {
  const telemetry = session?.telemetry || {};
  const context = Number.isFinite(telemetry.contextTokens) ? telemetry.contextTokens : null;
  const windowSize = Number.isFinite(telemetry.contextWindow) ? telemetry.contextWindow : null;
  const percent = Number.isFinite(telemetry.contextPercent)
    ? Math.max(0, Math.min(100, telemetry.contextPercent))
    : context !== null && windowSize > 0 ? Math.max(0, Math.min(100, context / windowSize * 100)) : null;
  $("#context-value").textContent = context !== null
    ? `${formatCompactCount(context)}${windowSize ? ` / ${formatCompactCount(windowSize)}` : ""}`
    : "—";
  $("#context-percent").textContent = percent !== null ? `${Math.round(percent)}%` : "";
  $("#context-fill").style.width = percent !== null ? `${percent}%` : "0%";
  const input = [telemetry.inputTokens, telemetry.subagentInputTokens, telemetry.externalInputTokens]
    .reduce((sum, value) => sum + (Number.isFinite(value) && value > 0 ? value : 0), 0);
  const output = [telemetry.outputTokens, telemetry.subagentOutputTokens, telemetry.externalOutputTokens]
    .reduce((sum, value) => sum + (Number.isFinite(value) && value > 0 ? value : 0), 0);
  const total = input + output;
  $("#token-value").textContent = total > 0 ? `Tokens ${formatCompactCount(total)}` : "Tokens —";
}

function updateHeader(session) {
  const name = $("#current-name");
  const path = $("#current-path");
  const badge = $("#session-status");
  const modelButton = $("#model-button");
  if (!session) {
    name.textContent = "セッション";
    path.textContent = "";
    if (badge) badge.hidden = true;
    modelButton.textContent = "モデル";
    modelButton.disabled = true;
    composer.disabled = true;
    composer.placeholder = "メッセージ";
    $("#send-button").disabled = true;
    $("#composer-hint").textContent = "追送";
    $("#diff-button").disabled = true;
    $("#info-path").textContent = "";
    $("#metric-tools").textContent = "—";
    $("#metric-runs").textContent = "—";
    $("#metric-cost").textContent = "—";
    updateUsage(null);
    updateCodexUsage(null);
    updateTranscriptControl();
    return;
  }
  name.textContent = text(session.name, shortPath(session.cwd));
  path.textContent = text(session.cwd, "");
  if (badge) {
    const status = statusFor(session);
    badge.hidden = false;
    badge.dataset.status = status;
    badge.textContent = statusLabel(status);
  }
  const model = currentModelFor(session);
  modelButton.textContent = model ? text(model.name, `${model.provider}/${model.id}`) : "モデル";
  modelButton.disabled = !session.connected || statusFor(session) === "working" || state.modelSwitching;
  composer.disabled = !session.connected;
  composer.placeholder = "メッセージ";
  $("#send-button").disabled = !session.connected;
  $("#composer-hint").textContent = session.connected ? "追送" : "切断中";
  $("#diff-button").disabled = !session.connected;
  $("#info-path").textContent = text(session.cwd, "");
  const telemetry = session.telemetry || {};
  $("#metric-tools").textContent = formatCount(telemetry.toolCalls);
  $("#metric-runs").textContent = formatCount(telemetry.runs);
  $("#metric-cost").textContent = formatCost(telemetry.mainModelCostUsd);
  updateUsage(session);
  updateCodexUsage(session);
  updateTranscriptControl();
}

function updateTranscriptControl() {
  const button = $("#transcript-toggle");
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  button.disabled = !session?.connected && !session?.historyOnly;
  button.setAttribute("aria-pressed", String(state.transcriptEnabled));
  button.textContent = state.transcriptEnabled ? "隠す" : "表示";
  button.title = state.transcriptEnabled ? "会話を隠す" : "会話を表示";
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
  const loading = state.transcriptLoadingFor === state.selectedId;
  heading.textContent = loading ? "読込中" : hasSession ? "会話なし" : "Pi";
  empty.append(avatar, heading);
  messageList.append(empty);
}

function eventSummary(item) {
  const summary = text(item.summary, "イベントを受信しました。");
  if (summary === "Pi run started") return "開始";
  if (summary === "Pi run settled") return "完了";
  if (summary === "Subagent settled") return "サブエージェント完了";
  if (summary === "Model changed") return "モデルを切替";
  if (summary.startsWith("External model cost: ")) return `外部費用 · ${summary.slice("External model cost: ".length)}`;
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
  sender.textContent = "Pi";
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

function appendTranscriptBlocks(container, blocks) {
  const allowedImages = new Set(["image/png", "image/jpeg", "image/webp", "image/gif"]);
  for (const block of Array.isArray(blocks) ? blocks : []) {
    if (block?.type === "text" && typeof block.text === "string") {
      const content = document.createElement("p");
      content.className = "bubble-text";
      content.textContent = block.text;
      container.append(content);
    } else if (block?.type === "image" && allowedImages.has(block.mimeType) && typeof block.data === "string" && block.data.length <= 2 * 1024 * 1024) {
      const image = document.createElement("img");
      image.className = "message-image";
      image.alt = "会話に添付された画像";
      image.loading = "lazy";
      image.decoding = "async";
      image.src = `data:${block.mimeType};base64,${block.data}`;
      container.append(image);
    } else if (block?.type === "toolCall" && typeof block.name === "string") {
      const disclosure = document.createElement("details");
      disclosure.className = "tool-call-details";
      const summary = document.createElement("summary");
      summary.textContent = `ツール呼び出し · ${block.name}`;
      const argumentsText = document.createElement("pre");
      argumentsText.textContent = typeof block.arguments === "string" ? block.arguments : "{}";
      disclosure.append(summary, argumentsText);
      container.append(disclosure);
    }
  }
}

function makeUserMessage(item, senderLabel = "送信した指示") {
  const row = document.createElement("article");
  row.className = "message-row user-message transcript-message";
  const stack = document.createElement("div");
  stack.className = "message-stack";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const time = document.createElement("time");
  time.textContent = formatTime(item.at);
  if (item.at) time.dateTime = item.at;
  const sender = document.createElement("strong");
  sender.textContent = senderLabel;
  meta.append(time, sender);
  const bubble = document.createElement("div");
  bubble.className = "message-bubble user-bubble";
  if (Array.isArray(item.content)) appendTranscriptBlocks(bubble, item.content);
  else {
    const content = document.createElement("p");
    content.className = "bubble-text";
    content.textContent = item.text;
    bubble.append(content);
  }
  stack.append(meta, bubble);
  row.append(stack);
  return row;
}

function makeAssistantMessage(item) {
  const row = document.createElement("article");
  row.className = "message-row pi-message transcript-message";
  row.dataset.kind = "transcript";
  const avatar = document.createElement("span");
  avatar.className = "message-avatar";
  avatar.setAttribute("aria-hidden", "true");
  avatar.textContent = "π";
  const stack = document.createElement("div");
  stack.className = "message-stack";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const sender = document.createElement("strong");
  sender.textContent = "Pi";
  const messageModel = item.model;
  if (messageModel && typeof messageModel.provider === "string" && typeof messageModel.id === "string") {
    const catalogModel = state.models.get(state.selectedId)?.models?.find((model) => model.provider === messageModel.provider && model.id === messageModel.id);
    const model = document.createElement("span");
    model.className = "message-model";
    model.textContent = catalogModel?.name || messageModel.name || messageModel.id;
    model.title = `${messageModel.provider}/${messageModel.id}`;
    meta.append(sender, model);
  } else {
    meta.append(sender);
  }
  const time = document.createElement("time");
  time.textContent = formatTime(item.at);
  if (item.at) time.dateTime = item.at;
  meta.append(time);
  const bubble = document.createElement("div");
  bubble.className = "message-bubble pi-bubble";
  appendTranscriptBlocks(bubble, item.content);
  stack.append(meta, bubble);
  row.append(avatar, stack);
  return row;
}

function bashExecutionStatus(item) {
  const exitCode = Number.isSafeInteger(item.exitCode) ? item.exitCode : null;
  const cancelled = typeof item.cancelled === "boolean" ? item.cancelled : typeof item.canceled === "boolean" ? item.canceled : null;
  const truncated = typeof item.truncated === "boolean" ? item.truncated : typeof item.outputTruncated === "boolean" ? item.outputTruncated : null;
  const labels = [];
  if (exitCode !== null) labels.push(`code ${exitCode}`);
  if (cancelled === true) labels.push("中断");
  if (truncated === true) labels.push("省略");
  return { labels, warning: cancelled === true || (exitCode !== null && exitCode !== 0) };
}

function makeToolMessage(item) {
  const row = document.createElement("article");
  const bashStatus = item.role === "bashExecution" ? bashExecutionStatus(item) : null;
  const warning = item.isError === true || bashStatus?.warning;
  row.className = `message-row pi-message tool-message${warning ? " is-warning" : ""}`;
  row.dataset.kind = item.role;
  const avatar = document.createElement("span");
  avatar.className = "message-avatar tool-avatar";
  avatar.setAttribute("aria-hidden", "true");
  avatar.textContent = "⚙";
  const stack = document.createElement("div");
  stack.className = "message-stack";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const sender = document.createElement("strong");
  sender.textContent = item.role === "toolResult" ? `ツール出力 · ${text(item.toolName, "tool")}` : item.role === "bashExecution" ? "bash 実行" : `Pi extension · ${text(item.label, "custom")}`;
  const time = document.createElement("time");
  time.textContent = formatTime(item.at);
  if (item.at) time.dateTime = item.at;
  meta.append(sender, time);
  const bubble = document.createElement("div");
  bubble.className = "message-bubble pi-bubble tool-bubble";
  if (item.command) {
    const command = document.createElement("details");
    command.className = "tool-call-details";
    const summary = document.createElement("summary");
    summary.textContent = "実行コマンド";
    const value = document.createElement("pre");
    value.textContent = item.command;
    command.append(summary, value);
    bubble.append(command);
  }
  if (bashStatus?.labels.length) {
    const status = document.createElement("p");
    status.className = "tool-status";
    status.textContent = bashStatus.labels.join(" · ");
    bubble.append(status);
  }
  // Render only known scalar status fields above; in particular, never expose fullOutputPath.
  appendTranscriptBlocks(bubble, item.content);
  stack.append(meta, bubble);
  row.append(avatar, stack);
  return row;
}

function makeSummaryMessage(item) {
  const row = document.createElement("article");
  row.className = "message-row pi-message transcript-message";
  row.dataset.kind = "summary";
  const avatar = document.createElement("span");
  avatar.className = "message-avatar";
  avatar.setAttribute("aria-hidden", "true");
  avatar.textContent = "π";
  const stack = document.createElement("div");
  stack.className = "message-stack";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const sender = document.createElement("strong");
  sender.textContent = text(item.label, "会話の要約");
  const time = document.createElement("time");
  time.textContent = formatTime(item.at);
  if (item.at) time.dateTime = item.at;
  meta.append(sender, time);
  const bubble = document.createElement("div");
  bubble.className = "message-bubble pi-bubble summary-bubble";
  appendTranscriptBlocks(bubble, item.content);
  stack.append(meta, bubble);
  row.append(avatar, stack);
  return row;
}

function updateJumpButton() {
  const awayFromLatest = messageList.scrollHeight - messageList.clientHeight - messageList.scrollTop > 120;
  $("#jump-latest").hidden = !state.selectedId || !awayFromLatest;
}

function restoreSessionScroll(sessionId) {
  const position = state.scrollPositions.get(sessionId);
  if (!Number.isFinite(position)) return;
  requestAnimationFrame(() => {
    if (state.selectedId !== sessionId) return;
    messageList.scrollTop = Math.max(0, Math.min(position, messageList.scrollHeight - messageList.clientHeight));
    updateJumpButton();
  });
}

function renderMessages(scrollToBottom = false) {
  const wasNearBottom = messageList.scrollHeight - messageList.clientHeight - messageList.scrollTop < 90;
  messageList.replaceChildren();
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  const events = state.events.get(state.selectedId) || [];
  const transcript = state.transcriptEnabled ? state.transcripts.get(state.selectedId)?.messages || [] : [];
  const transcriptUserTexts = new Set(transcript.filter((item) => item.role === "user").map((item) => (item.content || []).filter((block) => block.type === "text").map((block) => block.text).join("")));
  const sent = (state.sentMessages.get(state.selectedId) || []).filter((item) => !transcriptUserTexts.has(item.text));
  const messages = [
    ...events.map((item) => ({ ...item, messageRole: "event", sortTime: Date.parse(item.at) || 0 })),
    ...transcript.map((item) => ({ ...item, messageRole: item.role, senderLabel: item.role === "user" ? "あなた" : "Pi", sortTime: Date.parse(item.at) || 0 })),
    ...sent.map((item) => ({ ...item, messageRole: "user", sortTime: Date.parse(item.at) || 0 })),
  ].sort((a, b) => a.sortTime - b.sortTime);

  if (!messages.length) {
    showEmptyThread(Boolean(session));
    updateJumpButton();
    return;
  }
  for (const item of messages) {
    if (item.messageRole === "user") messageList.append(makeUserMessage(item, item.senderLabel));
    else if (item.messageRole === "assistant") messageList.append(makeAssistantMessage(item));
    else if (item.messageRole === "toolResult" || item.messageRole === "bashExecution" || item.messageRole === "custom") messageList.append(makeToolMessage(item));
    else if (item.messageRole === "summary") messageList.append(makeSummaryMessage(item));
    else messageList.append(makePiMessage(item));
  }
  if (scrollToBottom || wasNearBottom) messageList.scrollTop = messageList.scrollHeight;
  if (state.selectedId) state.scrollPositions.set(state.selectedId, messageList.scrollTop);
  updateJumpButton();
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

function cancelTranscriptRequest() {
  const request = state.transcriptRequest;
  state.transcriptRequest = null;
  state.transcriptLoadingFor = null;
  request?.controller.abort();
}

function cancelDetailRefresh(sessionId) {
  const controller = state.detailRefreshes.get(sessionId);
  if (!controller) return;
  state.detailRefreshes.delete(sessionId);
  controller.abort();
}

function cancelAllDetailRefreshes() {
  for (const sessionId of state.detailRefreshes.keys()) cancelDetailRefresh(sessionId);
}

function resizeComposer() {
  composer.style.height = "auto";
  composer.style.height = `${Math.min(composer.scrollHeight, 176)}px`;
}

async function selectSession(sessionId) {
  if (state.selectedId === sessionId) {
    toggleDrawer(false);
    return;
  }
  if (state.selectedId) {
    state.drafts.set(state.selectedId, composer.value);
    state.scrollPositions.set(state.selectedId, messageList.scrollTop);
  }
  cancelDetailRefresh(state.selectedId);
  cancelTranscriptRequest();
  state.transcripts.clear();
  state.transcriptCursors.clear();
  state.historySelection = null;
  state.sessions = state.sessions.filter((item) => item.historyOnly !== true);
  state.selectedId = sessionId;
  composer.value = state.drafts.get(sessionId) || "";
  resizeComposer();
  updateTranscriptControl();
  toggleDrawer(false);
  renderSessionList();
  updateHeader(state.sessions.find((item) => item.sessionId === sessionId));
  renderMessages(true);
  restoreSessionScroll(sessionId);
  await Promise.all([refreshDetails(true), loadModels(sessionId)]);
}

async function selectHistorySession(historySession) {
  if (state.sessions.some((item) => item.sessionId === historySession.sessionId && item.historyOnly !== true && item.connected)) {
    setSidebarMode("live");
    await selectSession(historySession.sessionId);
    return;
  }
  const sessionId = historySession.sessionId;
  if (state.selectedId) {
    state.drafts.set(state.selectedId, composer.value);
    state.scrollPositions.set(state.selectedId, messageList.scrollTop);
  }
  cancelDetailRefresh(state.selectedId);
  cancelTranscriptRequest();
  state.transcripts.clear();
  state.transcriptCursors.clear();
  state.historySelection = {
    ...historySession, connected: false, status: "offline", historyOnly: true,
  };
  state.sessions = state.sessions.filter((item) => item.historyOnly !== true);
  state.sessions.push(state.historySelection);
  state.selectedId = sessionId;
  state.events.delete(sessionId);
  state.cursors.delete(sessionId);
  state.tasks.delete(sessionId);
  state.diffs.delete(sessionId);
  composer.value = "";
  resizeComposer();
  updateTranscriptControl();
  toggleDrawer(false);
  setSidebarMode("folders");
  renderSessionList();
  updateHeader(state.historySelection);
  renderMessages(true);
  await loadHistoryTranscript(sessionId);
}

function resumeSelectedHistoryTranscript() {
  const sessionId = state.selectedId;
  if (!sessionId || state.historySelection?.sessionId !== sessionId || !state.transcriptEnabled
    || state.transcripts.has(sessionId) || state.transcriptRequest) return;
  void loadHistoryTranscript(sessionId);
}

async function loadHistoryTranscript(sessionId) {
  if (!state.transcriptEnabled || state.selectedId !== sessionId || state.transcripts.has(sessionId) || state.transcriptRequest) return;
  const request = { sessionId, controller: new AbortController() };
  state.transcriptRequest = request;
  state.transcriptLoadingFor = sessionId;
  updateTranscriptControl();
  try {
    const transcript = await api(`/api/history/${encodeURIComponent(sessionId)}/transcript`, { signal: request.controller.signal });
    if (state.transcriptRequest !== request || state.selectedId !== sessionId || !state.transcriptEnabled) return;
    const messages = Array.isArray(transcript.messages) ? transcript.messages : [];
    const capped = capTranscript(messages);
    state.transcripts.set(sessionId, {
      ...transcript,
      messages: capped.messages,
      truncated: transcript.truncated === true || capped.truncated,
    });
    if (typeof transcript.cursor === "string") state.transcriptCursors.set(sessionId, transcript.cursor);
    renderMessages(true);
  } catch (error) {
    if (!request.controller.signal.aborted && state.selectedId === sessionId) showToast(error.message, "error");
  } finally {
    if (state.transcriptRequest === request) {
      state.transcriptRequest = null;
      state.transcriptLoadingFor = null;
      updateTranscriptControl();
    }
  }
}

async function loadModels(sessionId) {
  const session = state.sessions.find((item) => item.sessionId === sessionId);
  if (!session?.connected || state.loadingModels.has(sessionId) || state.models.has(sessionId)) return;
  state.loadingModels.add(sessionId);
  try {
    const catalog = await api(`/api/sessions/${encodeURIComponent(sessionId)}/models`);
    state.models.set(sessionId, catalog);
  } catch (error) {
    state.models.set(sessionId, { current: null, models: [], error: error.message });
  } finally {
    state.loadingModels.delete(sessionId);
    if (state.selectedId === sessionId) updateHeader(state.sessions.find((item) => item.sessionId === sessionId));
  }
}

async function refreshDetails(reset = false) {
  const sessionId = state.selectedId;
  if (!sessionId) return;
  if (state.detailRefreshes.has(sessionId)) {
    state.detailRefreshAgain.add(sessionId);
    return;
  }
  const controller = new AbortController();
  state.detailRefreshes.set(sessionId, controller);
  try {
    await refreshDetailsForSession(sessionId, reset, controller.signal);
  } finally {
    if (state.detailRefreshes.get(sessionId) === controller) state.detailRefreshes.delete(sessionId);
    if (state.detailRefreshAgain.delete(sessionId) && state.selectedId === sessionId && state.liveConnected) {
      queueMicrotask(() => void refreshDetails(false));
    }
  }
}

async function refreshDetailsForSession(sessionId, reset, signal) {
  const session = state.sessions.find((item) => item.sessionId === sessionId);
  if (!session) return;
  let receivedEvents = [];
  let transcriptReset = reset;
  if (reset) {
    state.events.set(sessionId, []);
    state.cursors.set(sessionId, 0);
    state.tasks.delete(sessionId);
    state.diffs.delete(sessionId);
    renderMessages();
    renderTasks();
    $("#diff-summary").textContent = "—";
  }

  const after = state.cursors.get(sessionId) || 0;
  try {
    const result = await api(`/api/sessions/${encodeURIComponent(sessionId)}/events?after=${after}`, { signal });
    const previous = state.events.get(sessionId) || [];
    if (result.dropped) {
      state.events.set(sessionId, []);
      transcriptReset = true;
    }
    receivedEvents = Array.isArray(result.events) ? result.events : [];
    const base = state.events.get(sessionId) || previous;
    const merged = [...base, ...(result.events || [])];
    const unique = new Map(merged.map((item) => [item.seq, item]));
    const next = [...unique.values()].slice(-100);
    const changed = next.length !== previous.length || result.dropped;
    state.events.set(sessionId, next);
    state.cursors.set(sessionId, Number.isSafeInteger(result.cursor) ? result.cursor : after);
    if (Number.isSafeInteger(session.eventSeq) && Number.isSafeInteger(result.cursor) && result.cursor < session.eventSeq) {
      state.detailRefreshAgain.add(sessionId);
    }
    if (changed && state.selectedId === sessionId) renderMessages();
  } catch (error) {
    if (signal.aborted) return;
    if (reset && state.selectedId === sessionId) showToast(error.message, "error");
  }
  if (signal.aborted) return;

  if (!session.connected) {
    renderTasks();
    return;
  }
  const taskChanged = reset || receivedEvents.some((item) => item.type === "TaskStarted" || item.type === "TaskUpdated");
  if (taskChanged) {
    try {
      const tasks = await api(`/api/sessions/${encodeURIComponent(sessionId)}/tasks`, { signal });
      state.tasks.set(sessionId, tasks);
      if (state.selectedId === sessionId) renderTasks();
    } catch (error) {
      if (signal.aborted) return;
      if (reset && state.selectedId === sessionId) showToast(error.message, "error");
    }
  }
  if (signal.aborted) return;
  if (state.transcriptEnabled && state.selectedId === sessionId && (reset || receivedEvents.length > 0)) void refreshTranscript(transcriptReset);
}

async function refreshTranscript(notifyError = false) {
  const sessionId = state.selectedId;
  const session = state.sessions.find((item) => item.sessionId === sessionId);
  if (!state.transcriptEnabled || !session?.connected || !sessionId || state.transcriptRequest) return;
  const request = { sessionId, controller: new AbortController() };
  state.transcriptRequest = request;
  state.transcriptLoadingFor = sessionId;
  updateTranscriptControl();
  let continuePaging = false;
  const isCurrentRequest = () => state.transcriptRequest === request && state.selectedId === sessionId && state.transcriptEnabled;
  try {
    const cursor = state.transcriptCursors.get(sessionId);
    const query = cursor ? `?afterEntryId=${encodeURIComponent(cursor)}` : "";
    const transcript = await api(`/api/sessions/${encodeURIComponent(sessionId)}/transcript${query}`, { signal: request.controller.signal });
    if (!isCurrentRequest()) return;
    if (!Array.isArray(transcript.messages)) throw new Error("Pi から会話データを読み取れませんでした。");

    const replace = transcript.reset === true || !state.transcripts.has(sessionId);
    const previousSnapshot = state.transcripts.get(sessionId);
    const previous = replace ? [] : previousSnapshot?.messages || [];
    const messages = new Map(previous.map((item) => [item.id, item]));
    for (const item of transcript.messages) messages.set(item.id, item);
    const sorted = [...messages.values()].sort((a, b) => (Date.parse(a.at) || 0) - (Date.parse(b.at) || 0));
    const capped = capTranscript(sorted);
    state.transcripts.set(sessionId, {
      ...transcript,
      messages: capped.messages,
      truncated: transcript.truncated === true || (!replace && previousSnapshot?.truncated === true) || capped.truncated,
    });
    if (typeof transcript.cursor === "string") state.transcriptCursors.set(sessionId, transcript.cursor);
    else if (replace) state.transcriptCursors.delete(sessionId);
    if (replace || transcript.messages.length > 0) renderMessages();
    continuePaging = transcript.more === true;
    updateTranscriptControl();
  } catch (error) {
    if (request.controller.signal.aborted || !isCurrentRequest()) return;
    if (notifyError) {
      state.transcriptEnabled = false;
      state.transcripts.delete(sessionId);
      state.transcriptCursors.delete(sessionId);
      renderMessages();
      const unsupported = /unknown command|unsupported command|not implemented/i.test(error.message);
      showToast(unsupported ? "会話表示には pi-harness bridge の更新が必要です。" : error.message, "error");
    }
  } finally {
    if (state.transcriptRequest === request) {
      state.transcriptRequest = null;
      state.transcriptLoadingFor = null;
      updateTranscriptControl();
    }
    if (continuePaging && state.selectedId === sessionId && state.transcriptEnabled) setTimeout(() => void refreshTranscript(), 0);
  }
}

async function toggleTranscript() {
  const sessionId = state.selectedId;
  const session = state.sessions.find((item) => item.sessionId === sessionId);
  if (session?.historyOnly) {
    if (state.transcriptEnabled) {
      state.transcriptEnabled = false;
      cancelTranscriptRequest();
      state.transcripts.delete(sessionId);
      state.transcriptCursors.delete(sessionId);
      renderMessages();
      updateTranscriptControl();
      return;
    }
    state.transcriptEnabled = true;
    updateTranscriptControl();
    await loadHistoryTranscript(sessionId);
    return;
  }
  if (!sessionId) {
    state.transcriptEnabled = !state.transcriptEnabled;
    updateTranscriptControl();
    return;
  }
  if (state.transcriptEnabled) {
    state.transcriptEnabled = false;
    cancelTranscriptRequest();
    state.transcripts.delete(sessionId);
    state.transcriptCursors.delete(sessionId);
    renderMessages();
    updateTranscriptControl();
    return;
  }
  state.transcriptEnabled = true;
  updateTranscriptControl();
  await refreshTranscript(true);
}

function renderModelList() {
  const list = $("#model-list");
  const empty = $("#model-empty");
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  const catalog = session ? state.models.get(session.sessionId) : null;
  const query = $("#model-search").value.trim().toLocaleLowerCase();
  const models = (catalog?.models || []).filter((model) => `${model.provider} ${model.id} ${model.name || ""}`.toLocaleLowerCase().includes(query));
  list.replaceChildren();
  empty.hidden = models.length > 0;
  empty.textContent = catalog?.error
    ? /unknown command|unsupported command|not implemented/i.test(catalog.error) ? "bridge更新が必要" : "取得できません"
    : state.loadingModels.has(session?.sessionId) ? "読込中" : "モデルなし";
  const current = session?.currentModel || catalog?.current;
  for (const model of models) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "model-option";
    button.setAttribute("role", "option");
    const selected = current?.provider === model.provider && current?.id === model.id;
    button.setAttribute("aria-selected", String(selected));
    const title = document.createElement("strong");
    title.textContent = text(model.name, model.id);
    const provider = document.createElement("span");
    provider.textContent = model.provider;
    button.append(title, provider);
    button.addEventListener("click", () => void switchModel(model));
    list.append(button);
  }
}

async function openModelDialog() {
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  if (!session?.connected || statusFor(session) === "working") return;
  await loadModels(session.sessionId);
  $("#model-search").value = "";
  renderModelList();
  $("#model-dialog").showModal();
  $("#model-search").focus();
}

async function switchModel(model) {
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  if (!session?.connected || state.modelSwitching) return;
  state.modelSwitching = true;
  updateHeader(session);
  try {
    const result = await api(`/api/sessions/${encodeURIComponent(session.sessionId)}/model`, {
      method: "POST", body: JSON.stringify({ provider: model.provider, modelId: model.id }),
    });
    session.currentModel = result.current;
    const catalog = state.models.get(session.sessionId) || { models: [] };
    state.models.set(session.sessionId, { ...catalog, current: result.current });
    $("#model-dialog").close();
  } catch (error) {
    showToast(error.message, "error");
  } finally {
    state.modelSwitching = false;
    updateHeader(state.sessions.find((item) => item.sessionId === session.sessionId));
  }
}

async function loadCodexUsage() {
  const session = state.sessions.find((item) => item.sessionId === state.selectedId);
  if (!session?.connected || !isCodexProvider(currentModelFor(session)?.provider) || state.loadingCodexUsage.has(session.sessionId)) return;
  state.loadingCodexUsage.add(session.sessionId);
  updateCodexUsage(session);
  try {
    const result = await api(`/api/sessions/${encodeURIComponent(session.sessionId)}/codex-usage`);
    if (!result || typeof result !== "object") throw new Error("Codex usage unavailable");
    state.codexUsage.set(session.sessionId, result);
  } catch (error) {
    showToast(error.message, "error");
  } finally {
    state.loadingCodexUsage.delete(session.sessionId);
    if (state.selectedId === session.sessionId) updateCodexUsage(state.sessions.find((item) => item.sessionId === session.sessionId));
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
    await api(`/api/sessions/${encodeURIComponent(session.sessionId)}/instruction`, {
      method: "POST", body: JSON.stringify({ text: value }),
    });
    const sent = state.sentMessages.get(session.sessionId) || [];
    sent.push({ id: `local-${Date.now()}`, at: new Date().toISOString(), text: value });
    state.sentMessages.set(session.sessionId, sent.slice(-100));
    state.drafts.delete(session.sessionId);
    composer.value = "";
    resizeComposer();
    renderMessages(true);
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
  const previousSession = state.sessions.find((item) => item.sessionId === previousSelection);
  const liveSessions = Array.isArray(payload.sessions) ? payload.sessions : [];
  const archive = state.historySelection?.sessionId === previousSelection ? state.historySelection : null;
  const archiveReplacedByLive = archive && liveSessions.some((session) => session.sessionId === archive.sessionId && session.connected === true);
  const keepArchive = archive && !archiveReplacedByLive;
  const nextSessions = keepArchive
    ? [...liveSessions.filter((session) => session.sessionId !== archive.sessionId), archive]
    : liveSessions;
  if (archiveReplacedByLive) state.historySelection = null;
  setConnection(payload.connected === true);
  if (payload.connected !== true && nextSessions.length === 0 && state.sessions.length > 0) {
    for (const session of state.sessions) session.connected = false;
    state.sessionSignature = JSON.stringify(state.sessions);
    renderSessionList();
    updateHeader(state.sessions.find((item) => item.sessionId === state.selectedId));
    return;
  }
  const signature = JSON.stringify(nextSessions);
  const nextSelection = !state.selectedId || !nextSessions.some((session) => session.sessionId === state.selectedId)
    ? liveSessions[0]?.sessionId || null : state.selectedId;
  if (previousSelection && previousSelection !== nextSelection) {
    state.drafts.set(previousSelection, composer.value);
    state.scrollPositions.set(previousSelection, messageList.scrollTop);
  }
  state.sessions = nextSessions;
  state.selectedId = nextSelection;
  if (signature !== state.sessionSignature) {
    state.sessionSignature = signature;
    renderSessionList();
  }
  const selected = state.sessions.find((session) => session.sessionId === state.selectedId);
  updateHeader(selected);
  if (previousSelection !== state.selectedId) {
    cancelDetailRefresh(previousSelection);
    cancelTranscriptRequest();
    state.transcripts.clear();
    state.transcriptCursors.clear();
    composer.value = state.drafts.get(state.selectedId) || "";
    resizeComposer();
    updateTranscriptControl();
    renderMessages(true);
    if (state.selectedId) restoreSessionScroll(state.selectedId);
    if (state.selectedId) {
      void refreshDetails(true);
      void loadModels(state.selectedId);
    }
  } else if (selected?.connected && previousSession?.historyOnly) {
    void refreshDetails(true);
    void loadModels(selected.sessionId);
  } else if (selected?.connected && previousSession && Number.isSafeInteger(selected.eventSeq) && selected.eventSeq !== previousSession.eventSeq) {
    void refreshDetails(false);
  }
}

function stopActiveConnection() {
  cancelAllDetailRefreshes();
  cancelTranscriptRequest();
  const stream = state.stream;
  state.liveConnected = false;
  state.detailRefreshAgain.clear();
  state.stream = null;
  stream?.close();
}

function startActiveConnection() {
  if (document.visibilityState === "hidden") return;
  resumeSelectedHistoryTranscript();
  if (!state.stream) {
    const stream = new EventSource("/api/live");
    state.stream = stream;
    stream.addEventListener("open", () => {
      if (state.stream !== stream) return;
      state.liveConnected = true;
      const session = state.sessions.find((item) => item.sessionId === state.selectedId);
      if (session?.connected) {
        void refreshDetails(false);
        void loadModels(session.sessionId);
      }
    });
    stream.addEventListener("message", (event) => {
      try { applySnapshot(JSON.parse(event.data)); } catch { return; }
    });
    stream.addEventListener("error", () => {
      if (state.stream !== stream) return;
      state.liveConnected = false;
      setConnection(false);
    });
  }
}

async function refreshSessions() {
  try {
    const sessions = await api("/api/sessions");
    applySnapshot({ connected: true, sessions });
  } catch {
    setConnection(false);
  }
}

function setSidebarMode(mode) {
  if (!["live", "folders", "herdr"].includes(mode)) return;
  state.sidebarMode = mode;
  const config = {
    live: ["#sidebar-mode-live", "#session-mode-panel", "セッション"],
    folders: ["#sidebar-mode-folders", "#folder-history-panel", "履歴"],
    herdr: ["#sidebar-mode-herdr", "#herdr-sidebar-panel", "Herdr"],
  };
  for (const [name, [buttonSelector, panelSelector]] of Object.entries(config)) {
    $(buttonSelector).setAttribute("aria-pressed", String(name === mode));
    $(panelSelector).hidden = name !== mode;
  }
  sidebarLabel.textContent = config[mode][2];
  $("#session-count").hidden = mode !== "live";
  if (mode === "folders" && !state.historyLoaded) void loadHistory();
  if (mode === "herdr" && !state.herdrLoaded) void loadHerdr();
}

function renderFolderHistory() {
  const list = $("#folder-history-list");
  list.replaceChildren();
  const folders = Array.isArray(state.historyFolders) ? state.historyFolders : [];
  const total = folders.reduce((sum, folder) => sum + (Array.isArray(folder.sessions) ? folder.sessions.length : 0), 0);
  $("#folder-history-empty").hidden = total > 0;
  for (const folder of folders) {
    if (typeof folder?.path !== "string") continue;
    const group = document.createElement("section");
    group.className = "folder-history-group";
    const heading = document.createElement("div");
    heading.className = "folder-history-heading";
    const name = document.createElement("strong");
    name.textContent = shortPath(folder.path);
    name.title = folder.path;
    const path = document.createElement("span");
    path.className = "folder-history-path";
    path.textContent = folder.path;
    heading.append(name, path);
    group.append(heading);
    for (const session of Array.isArray(folder.sessions) ? folder.sessions : []) {
      if (typeof session?.sessionId !== "string") continue;
      const button = document.createElement("button");
      button.type = "button";
      button.className = "folder-history-item";
      button.title = folder.path;
      button.setAttribute("aria-current", String(session.sessionId === state.selectedId));
      const title = document.createElement("span");
      title.textContent = text(session.name, formatTime(session.updatedAt) || session.sessionId);
      const status = document.createElement("small");
      status.textContent = session.connected ? statusLabel(statusFor(session)) : "履歴";
      button.append(title, status);
      button.addEventListener("click", () => void selectHistorySession({ ...session, cwd: folder.path }));
      group.append(button);
    }
    list.append(group);
  }
}

async function loadHistory() {
  try {
    const result = await api("/api/history");
    state.historyFolders = Array.isArray(result.folders) ? result.folders : [];
    state.historyLoaded = true;
    renderFolderHistory();
    resumeSelectedHistoryTranscript();
  } catch (error) {
    showToast(error.message, "error");
  }
}

function herdrLabel(value, fallback = "—") {
  return typeof value === "string" && value.trim() ? value : fallback;
}

function herdrItemButton(className, primary, secondary = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = className;
  const label = document.createElement("span");
  label.textContent = primary;
  button.append(label);
  if (secondary) {
    const detail = document.createElement("small");
    detail.textContent = secondary;
    button.append(detail);
  }
  return button;
}

function renderHerdr() {
  const snapshot = state.herdr;
  const workspacesNode = $("#herdr-workspace-list");
  const tabsNode = $("#herdr-tab-list");
  const panesNode = $("#herdr-pane-list");
  const agentsNode = $("#herdr-agent-list");
  for (const node of [workspacesNode, tabsNode, panesNode, agentsNode]) node.replaceChildren();
  const workspaces = Array.isArray(snapshot?.workspaces) ? snapshot.workspaces : [];
  const tabs = Array.isArray(snapshot?.tabs) ? snapshot.tabs : [];
  const panes = Array.isArray(snapshot?.panes) ? snapshot.panes : [];
  const agents = panes.filter((pane) => pane.canAct === true && typeof pane.agent === "string" && pane.agent);
  $("#herdr-agent-count").textContent = String(agents.length);
  $("#herdr-agent-empty").hidden = agents.length > 0;
  $("#herdr-empty-state").hidden = workspaces.length > 0;
  $("#herdr-empty-state").textContent = snapshot?.available ? "ワークスペースなし" : "Herdr 未接続";
  $("#herdr-hierarchy").hidden = workspaces.length === 0;
  $("#herdr-swarm-overview").hidden = workspaces.length === 0;
  if (!workspaces.length) {
    $("#herdr-tab-empty").hidden = false;
    $("#herdr-pane-empty").hidden = false;
    return;
  }

  let workspace = workspaces.find((item) => item.id === state.selectedHerdrWorkspaceId);
  if (!workspace) workspace = workspaces.find((item) => item.focused) || workspaces[0];
  state.selectedHerdrWorkspaceId = workspace.id;
  for (const item of workspaces) {
    const title = herdrLabel(item.repoName, herdrLabel(item.label, `Workspace ${item.number || ""}`));
    const button = herdrItemButton("herdr-workspace-item", title, herdrLabel(item.checkoutPath, item.agentStatus));
    button.title = herdrLabel(item.checkoutPath, item.label || title);
    button.setAttribute("aria-current", String(item.id === workspace.id));
    button.addEventListener("click", () => {
      state.selectedHerdrWorkspaceId = item.id;
      state.selectedHerdrTabId = null;
      state.selectedHerdrPaneId = null;
      renderHerdr();
    });
    workspacesNode.append(button);
  }

  const workspaceTabs = tabs.filter((item) => item.workspaceId === workspace.id);
  $("#herdr-tabs-title").textContent = `タブ · ${workspaceTabs.length}`;
  $("#herdr-tab-empty").hidden = workspaceTabs.length > 0;
  if (!workspaceTabs.length) {
    $("#herdr-pane-empty").hidden = false;
  }
  let tab = workspaceTabs.find((item) => item.id === state.selectedHerdrTabId);
  if (!tab) tab = workspaceTabs.find((item) => item.focused) || workspaceTabs.find((item) => item.id === workspace.activeTabId) || workspaceTabs[0];
  state.selectedHerdrTabId = tab?.id || null;
  for (const item of workspaceTabs) {
    const button = herdrItemButton("herdr-tab-item", item.label || `Tab ${item.number}`, item.agentStatus);
    button.setAttribute("role", "tab");
    button.setAttribute("aria-selected", String(item.id === tab?.id));
    button.addEventListener("click", () => {
      state.selectedHerdrTabId = item.id;
      state.selectedHerdrPaneId = null;
      renderHerdr();
    });
    tabsNode.append(button);
  }

  const tabPanes = tab ? panes.filter((item) => item.workspaceId === workspace.id && item.tabId === tab.id) : [];
  $("#herdr-panes-title").textContent = `ペイン · ${tabPanes.length}`;
  $("#herdr-pane-empty").hidden = tabPanes.length > 0;
  let selectedPane = tabPanes.find((item) => item.id === state.selectedHerdrPaneId);
  if (!selectedPane) selectedPane = tabPanes.find((item) => item.focused) || tabPanes[0];
  state.selectedHerdrPaneId = selectedPane?.id || null;
  for (const pane of tabPanes) {
    const primary = herdrLabel(pane.label, herdrLabel(pane.agent, herdrLabel(pane.title, pane.id)));
    const secondary = [pane.agent, pane.agentStatus].filter(Boolean).join(" · ");
    const button = herdrItemButton("herdr-pane-item", primary, secondary);
    button.setAttribute("aria-current", String(pane.id === selectedPane?.id));
    button.title = [pane.cwd, pane.foregroundCwd].filter(Boolean).join(" → ") || pane.id;
    button.addEventListener("click", () => void openHerdrPane(pane));
    panesNode.append(button);
  }

  for (const pane of agents) {
    const label = herdrLabel(pane.agent, pane.id);
    const button = herdrItemButton("herdr-agent-item", label, pane.agentStatus);
    button.addEventListener("click", () => {
      state.selectedHerdrWorkspaceId = pane.workspaceId;
      state.selectedHerdrTabId = pane.tabId;
      state.selectedHerdrPaneId = pane.id;
      renderHerdr();
      void openHerdrPane(pane);
    });
    agentsNode.append(button);
  }
}

async function loadHerdr() {
  try {
    state.herdr = await api("/api/herdr");
    state.herdrLoaded = true;
    if (state.herdr.available && !state.selectedHerdrWorkspaceId) {
      state.selectedHerdrWorkspaceId = state.herdr.focusedWorkspaceId || null;
      state.selectedHerdrTabId = state.herdr.focusedTabId || null;
      state.selectedHerdrPaneId = state.herdr.focusedPaneId || null;
    }
    renderHerdr();
    if ($("#herdr-pane-dialog").open) {
      const pane = state.herdr.panes.find((item) => item.id === state.selectedHerdrPaneId);
      if (pane) {
        $("#herdr-pane-status").textContent = herdrLabel(pane.agentStatus, "unknown");
        $("#herdr-pane-focus-state").textContent = pane.focused ? "選択中" : "未選択";
        $("#herdr-pane-focus-button").disabled = pane.canAct !== true;
        $("#herdr-pane-send-button").disabled = pane.canAct !== true || pane.agentStatus === "blocked";
      }
    }
  } catch (error) {
    state.herdr = { available: false, workspaces: [], tabs: [], panes: [] };
    state.herdrLoaded = true;
    renderHerdr();
    showToast(error.message, "error");
  }
}

async function openHerdrPane(pane) {
  if (!pane || !state.herdr) return;
  state.selectedHerdrWorkspaceId = pane.workspaceId;
  state.selectedHerdrTabId = pane.tabId;
  state.selectedHerdrPaneId = pane.id;
  const workspace = state.herdr.workspaces.find((item) => item.id === pane.workspaceId);
  const tab = state.herdr.tabs.find((item) => item.id === pane.tabId);
  $("#herdr-pane-title").textContent = herdrLabel(pane.label, herdrLabel(pane.agent, pane.id));
  $("#herdr-dialog-workspace").textContent = herdrLabel(workspace?.repoName, herdrLabel(workspace?.label));
  $("#herdr-dialog-tab").textContent = herdrLabel(tab?.label, `Tab ${tab?.number || ""}`);
  $("#herdr-dialog-pane").textContent = herdrLabel(pane.label, pane.id);
  $("#herdr-pane-agent").textContent = herdrLabel(pane.agent, "Pane");
  $("#herdr-pane-status").textContent = herdrLabel(pane.agentStatus, "unknown");
  $("#herdr-pane-output").textContent = "読込中";
  $("#herdr-pane-focus-state").textContent = pane.focused ? "選択中" : "未選択";
  $("#herdr-pane-focus-button").disabled = pane.canAct !== true;
  $("#herdr-pane-action-text").value = "";
  $("#herdr-pane-send-button").disabled = pane.canAct !== true || pane.agentStatus === "blocked";
  const linkButton = $("#herdr-linked-session-button");
  linkButton.hidden = !pane.sessionId;
  if (!$("#herdr-pane-dialog").open) $("#herdr-pane-dialog").showModal();
  state.herdrPaneRequest?.controller.abort();
  const request = { id: pane.id, controller: new AbortController() };
  state.herdrPaneRequest = request;
  try {
    const result = await api(`/api/herdr/panes/${encodeURIComponent(pane.id)}/output`, { signal: request.controller.signal });
    if (state.herdrPaneRequest !== request || state.selectedHerdrPaneId !== pane.id) return;
    const maxChars = 12_000;
    const output = typeof result.text === "string" ? result.text : "";
    $("#herdr-pane-output").textContent = output.length > maxChars ? `${output.slice(0, maxChars)}\n…（省略）` : output || "出力なし";
  } catch (error) {
    if (!request.controller.signal.aborted && state.herdrPaneRequest === request) $("#herdr-pane-output").textContent = error.message;
  }
  renderHerdr();
}

function closeHerdrPane() {
  state.herdrPaneRequest?.controller.abort();
  state.herdrPaneRequest = null;
  $("#herdr-pane-dialog").close();
}

async function focusHerdrPane() {
  const paneId = state.selectedHerdrPaneId;
  const button = $("#herdr-pane-focus-button");
  if (!paneId || button.disabled) return;
  button.disabled = true;
  try {
    await api(`/api/herdr/panes/${encodeURIComponent(paneId)}/focus`, { method: "POST", body: "{}" });
    $("#herdr-pane-focus-state").textContent = "フォーカス済み";
    await loadHerdr();
  } catch (error) {
    showToast(error.message, "error");
  } finally {
    const pane = state.herdr?.panes.find((item) => item.id === paneId);
    button.disabled = pane?.canAct !== true;
  }
}

async function sendHerdrInstruction(event) {
  event.preventDefault();
  const paneId = state.selectedHerdrPaneId;
  const button = $("#herdr-pane-send-button");
  const input = $("#herdr-pane-action-text");
  const value = input.value;
  if (!paneId || !value.trim() || button.disabled) return;
  button.disabled = true;
  try {
    await api(`/api/herdr/panes/${encodeURIComponent(paneId)}/instruction`, {
      method: "POST", body: JSON.stringify({ text: value }),
    });
    input.value = "";
    $("#herdr-pane-focus-state").textContent = "送信済み";
    await loadHerdr();
  } catch (error) {
    if (error.code === "herdr_instruction_uncertain" || error instanceof TypeError) {
      showToast("送信結果不明。Herdrで確認してから再送してください。", "error");
    } else {
      showToast(error.message, "error");
    }
  } finally {
    const pane = state.herdr?.panes.find((item) => item.id === paneId);
    button.disabled = pane?.canAct !== true || pane.agentStatus === "blocked";
  }
}

async function openLinkedPiSession() {
  const pane = state.herdr?.panes.find((item) => item.id === state.selectedHerdrPaneId);
  if (!pane?.sessionId) return;
  closeHerdrPane();
  const live = state.sessions.find((item) => item.sessionId === pane.sessionId && item.historyOnly !== true && item.connected);
  if (live) {
    setSidebarMode("live");
    await selectSession(live.sessionId);
    return;
  }
  let found = state.historyFolders.flatMap((folder) => (folder.sessions || []).map((session) => ({ ...session, cwd: folder.path })))
    .find((session) => session.sessionId === pane.sessionId);
  if (!found) {
    await loadHistory();
    found = state.historyFolders.flatMap((folder) => (folder.sessions || []).map((session) => ({ ...session, cwd: folder.path })))
      .find((session) => session.sessionId === pane.sessionId);
  }
  if (found) await selectHistorySession(found);
  else showToast("Session を開けません", "error");
}

function refreshSidebarMode() {
  if (state.sidebarMode === "folders") void loadHistory();
  else if (state.sidebarMode === "herdr") void loadHerdr();
  else void refreshSessions();
}

$("#sessions-toggle").addEventListener("click", () => toggleDrawer());
backdrop.addEventListener("click", () => toggleDrawer(false));
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") toggleDrawer(false);
});
$("#sidebar-mode-live").addEventListener("click", () => setSidebarMode("live"));
$("#sidebar-mode-folders").addEventListener("click", () => setSidebarMode("folders"));
$("#sidebar-mode-herdr").addEventListener("click", () => setSidebarMode("herdr"));
$("#refresh-button").addEventListener("click", refreshSidebarMode);
$("#session-info-button").addEventListener("click", () => sessionInfo.showModal());
$("#close-info").addEventListener("click", () => sessionInfo.close());
$("#model-button").addEventListener("click", () => void openModelDialog());
$("#close-model").addEventListener("click", () => $("#model-dialog").close());
$("#model-search").addEventListener("input", renderModelList);
$("#privacy-info-button").addEventListener("click", () => $("#privacy-info").showModal());
$("#close-privacy").addEventListener("click", () => $("#privacy-info").close());
$("#model-dialog").addEventListener("click", (event) => {
  if (event.target === $("#model-dialog")) $("#model-dialog").close();
});
$("#privacy-info").addEventListener("click", (event) => {
  if (event.target === $("#privacy-info")) $("#privacy-info").close();
});
sessionInfo.addEventListener("click", (event) => {
  if (event.target === sessionInfo) sessionInfo.close();
});
$("#herdr-pane-dialog").addEventListener("click", (event) => {
  if (event.target === $("#herdr-pane-dialog")) closeHerdrPane();
});
$("#diff-button").addEventListener("click", loadDiff);
$("#close-herdr-pane").addEventListener("click", closeHerdrPane);
$("#herdr-pane-focus-button").addEventListener("click", () => void focusHerdrPane());
$("#herdr-pane-action-form").addEventListener("submit", sendHerdrInstruction);
$("#herdr-linked-session-button").addEventListener("click", () => void openLinkedPiSession());
$("#codex-usage-refresh").addEventListener("click", loadCodexUsage);
$("#transcript-toggle").addEventListener("click", toggleTranscript);
$("#jump-latest").addEventListener("click", () => {
  messageList.scrollTop = messageList.scrollHeight;
  updateJumpButton();
});
messageList.addEventListener("scroll", () => {
  if (state.selectedId) state.scrollPositions.set(state.selectedId, messageList.scrollTop);
  updateJumpButton();
}, { passive: true });
$("#instruction-form").addEventListener("submit", sendInstruction);
composer.addEventListener("input", () => {
  resizeComposer();
  if (state.selectedId) state.drafts.set(state.selectedId, composer.value);
});
composer.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    $("#instruction-form").requestSubmit();
  }
});
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") stopActiveConnection();
  else startActiveConnection();
});
window.addEventListener("pagehide", stopActiveConnection);
window.addEventListener("pageshow", startActiveConnection);
function syncVisualViewport() {
  const viewport = window.visualViewport;
  if (!viewport) return;
  document.documentElement.style.setProperty("--viewport-height", `${Math.round(viewport.height)}px`);
  document.documentElement.style.setProperty("--viewport-top", `${Math.round(viewport.offsetTop)}px`);
}
syncVisualViewport();
window.addEventListener("resize", syncVisualViewport, { passive: true });
window.visualViewport?.addEventListener("resize", syncVisualViewport, { passive: true });
window.visualViewport?.addEventListener("scroll", syncVisualViewport, { passive: true });
startActiveConnection();
if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => navigator.serviceWorker.register("/sw.js").catch(() => {}));
}
