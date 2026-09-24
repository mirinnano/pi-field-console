# Pi Field Console (experimental)

A small Go HTTP/SSE gateway and installable mobile-first PWA for the **already-running Pi sessions registered with pi-harness**. It does not start a second Pi runtime and does not expose the bridge Unix socket to the browser.

The UI syncs the active, compaction-aware conversation by default, including user/assistant text, tool calls and arguments, tool outputs, bash commands and statuses, visible extension messages, and supported images. System prompts, hidden thinking, and compaction/branch summaries are excluded. Transcript frames are size-bounded and fetched incrementally; they are not written to bridge snapshots or event logs. The browser keeps at most 200 entries / 6 MiB in memory, does not cache API responses, and drops the oldest entries at that display limit. Use **隠す** to stop sync, abort an in-flight request, and clear the displayed transcript.

The HTTP listener stays available; bridge status polling runs only while an SSE client is connected. Status snapshots are checked every 3 seconds. When a session's event sequence changes, the browser fetches its event and transcript deltas; hidden pages close SSE and abort reads. There is no fixed detail-refresh timer.

The console can send a follow-up, switch the active session's model while Pi is idle, and explicitly fetch Codex 5-hour / 7-day quota while using an `openai-codex` model.
Model changes do not alter Pi's default.
Context tokens are Pi's current context estimate; the session token total adds main-model, subagent, and external-model input/output where reported.
Codex quota is a separate account-level value.
Quota lookup is user-triggered, uses Pi's in-process auth, and returns only usage percentages/reset times to the browser.
It follows the request pattern in [`pi-chatgpt-limit`](https://github.com/patlux/pi-chatgpt-limit); that extension has no stable API, so this app does not install it or depend on private module exports.
The fixed `chatgpt.com/backend-api/wham/usage` endpoint is undocumented and may change.
No shell, approval, pause, or arbitrary command endpoint is exposed.

## UI reference

The conversation layout follows patterns visible in the public Codex CLI/TUI source: a scrollable transcript above one persistent composer, distinct user and agent entries, and a compact input footer. The reference files are [`chatwidget.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/chatwidget.rs), [`history_cell/messages.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/history_cell/messages.rs), and [`bottom_pane/chat_composer.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/bottom_pane/chat_composer.rs). This app adapts those interaction patterns for a Japanese mobile web UI; it does not copy Codex branding or source code.

The public repository describes `codex app` as the desktop experience, but its desktop UI implementation is not included in the open-source CLI repository. This implementation therefore references the published TUI source, not private desktop code.

The mobile drawer, transcript-follow behavior, and composer also borrow small interaction patterns from [agegr/pi-web](https://github.com/agegr/pi-web) and [khangkontum/pi-web](https://github.com/khangkontum/pi-web): visual-viewport sizing for the keyboard, per-session in-memory drafts/scroll positions, and a quiet jump-to-latest control.
No source files are copied.
UI text uses Noto Sans CJK when installed; conversation text uses Noto Serif CJK; code/model labels prefer Nerd Fonts.
All have system fallbacks and are not bundled.

## Enable the web console in pi-harness

The companion patch updates the pi-harness bridge and extension for transcript sync, model listing/switching, context telemetry, action-sequence refreshes, and on-demand Codex quota. Model metadata is allowlisted; API URLs and headers are not exposed. Switching validates an exact available-model pair and is session-local. Codex usage uses Pi's active Codex credential only inside the extension process; the browser receives 5-hour / 7-day percentages and reset times, never the token, email, or plan. It targets pi-harness commit `ea1b2f32eda2fcba326717b2f2e9fb08b8be9ee7` and changes only the files included in [`integrations/pi-harness-console.patch`](integrations/pi-harness-console.patch).

```sh
cd /path/to/pi-harness
git apply --unidiff-zero /path/to/pi-field-console/integrations/pi-harness-console.patch
npm run check
npm test
```

Restart the pi-harness bridge daemon and Pi sessions after applying the patch. Older bridge versions support basic status, events, and follow-up instructions, but not transcript sync, model switching, or Codex usage.

## Run locally

Requirements: Go 1.24+ and the updated pi-harness bridge and Pi extension running.

```sh
go run .
```

Open <http://127.0.0.1:8765>. The PWA can be installed from a compatible browser; service-worker caching is limited to the static app shell. API responses and session content are never cached.

Environment:

| Variable | Default | Purpose |
| --- | --- | --- |
| `PI_HARNESS_SOCKET` | `/tmp/pi-harness-<uid>/bridge.sock` | Existing local pi-harness UDS path |
| `PI_REMOTE_HOST` | `127.0.0.1` | Must be `localhost` or a loopback IP; public/interface binds are rejected |
| `PI_REMOTE_PORT` | `8765` | Local HTTP port |
| `PI_REMOTE_ALLOWED_HOSTS` | loopback hosts only | Comma-separated exact DNS hosts permitted behind an explicit reverse proxy |
| `PI_REMOTE_ALLOWED_ORIGINS` | same-origin only | Optional exact `https://…` origins for a trusted reverse proxy |

## Remote access boundary

The gateway has **no user login of its own**, so keep it loopback-only. Conversation text, tool arguments and outputs, and images are sent to browsers admitted by the proxy; any admitted user can also send follow-up instructions. Use a trusted HTTPS/VPN reverse proxy with strict identity/ACL controls (for example, a Tailscale Serve endpoint restricted by tailnet ACLs). Do not bind Go to a LAN/public address or forward port 8765 directly. If the proxy preserves the remote hostname, allow only that exact host and origin, e.g.:

```sh
PI_REMOTE_ALLOWED_HOSTS=pi-host.example-tailnet.ts.net \
PI_REMOTE_ALLOWED_ORIGINS=https://pi-host.example-tailnet.ts.net \
go run .
```

Configure the proxy to forward HTTPS traffic to `http://127.0.0.1:8765`; verify its ACL and host/origin forwarding before enabling it. The PWA is a control surface for loaded Pi sessions, so anyone admitted by that proxy can send follow-ups and switch the active model.

## Checks

```sh
go test ./...
go vet ./...
```

The Go service uses only the standard library. The UI is plain HTML/CSS/JavaScript and can share the same JSON API with a future Android client.

## License

MIT. See [LICENSE](LICENSE).
