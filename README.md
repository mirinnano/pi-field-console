# Pi Field Console (experimental)

A small Go HTTP/SSE gateway and installable mobile-first PWA for the **already-running Pi sessions registered with pi-harness**. It does not start a second Pi runtime and does not expose the bridge Unix socket to the browser.

The UI syncs the active, compaction-aware conversation by default, including user/assistant text, tool calls and arguments, tool outputs, bash commands and statuses, visible extension messages, and supported images. System prompts, hidden thinking, and compaction/branch summaries are excluded. Transcript frames are size-bounded and fetched incrementally; they are not written to bridge snapshots or event logs. The browser keeps at most 200 entries / 6 MiB in memory, does not cache API responses, and drops the oldest entries at that display limit. Use **会話を隠す** to stop sync, abort an in-flight request, and clear the displayed transcript.

The lightweight HTTP listener stays available so a new browser can wake the console. Background bridge status polling runs only while at least one live browser connection is open, and stops when the last connection closes. The browser closes its live stream and periodic detail refresh while hidden; explicit requests still run on demand. Thus the Go process remains idle-ready, but does not continuously poll the Pi bridge without an active page.

The only write operation is `send_instruction`, delivered to Pi as a follow-up user message. There is no shell, approval, pause, or arbitrary command endpoint.

## UI reference

The conversation layout follows patterns visible in the public Codex CLI/TUI source: a scrollable transcript above one persistent composer, distinct user and agent entries, and a compact input footer. The reference files are [`chatwidget.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/chatwidget.rs), [`history_cell/messages.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/history_cell/messages.rs), and [`bottom_pane/chat_composer.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/bottom_pane/chat_composer.rs). This app adapts those interaction patterns for a Japanese mobile web UI; it does not copy Codex branding or source code.

The public repository describes `codex app` as the desktop experience, but its desktop UI implementation is not included in the open-source CLI repository. This implementation therefore references the published TUI source, not private desktop code.

## Enable transcript support in pi-harness

The companion patch updates the pi-harness bridge protocol and extension to supply transcript snapshots. It targets pi-harness commit `ea1b2f32eda2fcba326717b2f2e9fb08b8be9ee7` and changes only the files included in [`integrations/pi-harness-transcript.patch`](integrations/pi-harness-transcript.patch).

```sh
cd /path/to/pi-harness
git apply /path/to/pi-field-console/integrations/pi-harness-transcript.patch
npm run check
npm test
```

Restart the pi-harness bridge daemon and Pi sessions after applying the patch. Older bridge versions continue to support status, events, and follow-up instructions, but not transcript sync.

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

Configure the proxy to forward HTTPS traffic to `http://127.0.0.1:8765`; verify its ACL and host/origin forwarding before enabling it. The PWA is a control surface for the currently loaded Pi session, so anyone admitted by that proxy can send follow-up instructions.

## Checks

```sh
go test ./...
go vet ./...
```

The Go service uses only the standard library. The UI is plain HTML/CSS/JavaScript and can share the same JSON API with a future Android client.

## License

MIT. See [LICENSE](LICENSE).
