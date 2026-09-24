package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeCall struct {
	Command   string `json:"command"`
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	After     int64  `json:"after"`
	ID        string `json:"id"`
}

func startFakeBridge(t *testing.T) (string, <-chan fakeCall) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ph-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := dir + "/bridge.sock"
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan fakeCall, 32)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var request fakeCall
				var frame struct {
					V       int             `json:"v"`
					Op      string          `json:"op"`
					ID      string          `json:"id"`
					Command string          `json:"command"`
					Session string          `json:"sessionId"`
					Text    string          `json:"text"`
					After   int64           `json:"after"`
					Fields  json.RawMessage `json:"fields"`
				}
				if json.Unmarshal(line, &frame) != nil || frame.V != bridgeVersion || frame.Op != "request" {
					return
				}
				request = fakeCall{Command: frame.Command, SessionID: frame.Session, Text: frame.Text, After: frame.After, ID: frame.ID}
				select {
				case calls <- request:
				default:
				}
				var data any
				switch frame.Command {
				case "request_status":
					data = []map[string]any{{"sessionId": "sess_1", "cwd": "/tmp/demo", "connected": true, "status": "working"}}
				case "request_events":
					data = map[string]any{"events": []map[string]any{{"seq": 4, "at": "2026-09-24T12:00:00Z", "type": "ToolStarted", "summary": "read"}}, "cursor": 4, "dropped": false}
				case "request_task_state":
					data = map[string]any{"source": "stepstone", "snapshotFound": true, "total": 1, "tasks": []map[string]any{{"id": "task_1", "title": "Inspect the change", "status": "doing"}}}
				case "request_diff":
					data = map[string]any{"summary": "1 file changed, 2 insertions(+)"}
				case "send_instruction":
					data = map[string]any{"accepted": true}
				default:
					_, _ = fmt.Fprintf(conn, `{"v":1,"op":"response","id":%q,"ok":false,"error":"unknown command"}`+"\n", frame.ID)
					return
				}
				encoded, _ := json.Marshal(data)
				_, _ = fmt.Fprintf(conn, `{"v":1,"op":"response","id":%q,"ok":true,"data":%s}`+"\n", frame.ID, encoded)
			}()
		}
	}()
	return socketPath, calls
}

func testHandler(t *testing.T, socketPath string) http.Handler {
	t.Helper()
	return newService(socketPath).handler(config{allowedHosts: map[string]struct{}{}, allowedOrigin: map[string]struct{}{}}, webAssets)
}

func TestAPIUsesOnlyScopedBridgeCommands(t *testing.T) {
	socketPath, calls := startFakeBridge(t)
	server := httptest.NewServer(testHandler(t, socketPath))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	var sessions []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || len(sessions) != 1 || sessions[0]["sessionId"] != "sess_1" {
		t.Fatalf("unexpected session response: status=%d body=%v", response.StatusCode, sessions)
	}
	if call := <-calls; call.Command != "request_status" || call.SessionID != "" {
		t.Fatalf("unexpected status bridge call: %+v", call)
	}

	response, err = http.Get(server.URL + "/api/sessions/sess_1/events?after=3")
	if err != nil {
		t.Fatal(err)
	}
	var events map[string]any
	if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || events["cursor"] != float64(4) {
		t.Fatalf("unexpected events response: status=%d body=%v", response.StatusCode, events)
	}
	if call := <-calls; call.Command != "request_events" || call.SessionID != "sess_1" || call.After != 3 {
		t.Fatalf("unexpected events bridge call: %+v", call)
	}

	response, err = http.Get(server.URL + "/api/sessions/sess_1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	var tasks map[string]any
	if err := json.NewDecoder(response.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || tasks["snapshotFound"] != true {
		t.Fatalf("unexpected tasks response: status=%d body=%v", response.StatusCode, tasks)
	}
	if call := <-calls; call.Command != "request_task_state" || call.SessionID != "sess_1" {
		t.Fatalf("unexpected task bridge call: %+v", call)
	}

	response, err = http.Get(server.URL + "/api/sessions/sess_1/diff")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("diff request returned %d", response.StatusCode)
	}
	if call := <-calls; call.Command != "request_diff" || call.SessionID != "sess_1" {
		t.Fatalf("unexpected diff bridge call: %+v", call)
	}

	body := strings.NewReader(`{"text":"Please inspect the failure"}`)
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/sess_1/instruction", body)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var accepted map[string]any
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || accepted["accepted"] != true {
		t.Fatalf("unexpected instruction response: status=%d body=%v", response.StatusCode, accepted)
	}
	if call := <-calls; call.Command != "send_instruction" || call.SessionID != "sess_1" || call.Text != "Please inspect the failure" {
		t.Fatalf("unexpected instruction bridge call: %+v", call)
	}

	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/sessions/sess_1/instruction", strings.NewReader(`{"text":" "}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty instruction was not rejected: %d", response.StatusCode)
	}
	select {
	case call := <-calls:
		t.Fatalf("invalid instruction reached bridge: %+v", call)
	default:
	}

	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/sessions/sess_1/pause", strings.NewReader(`{}`))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatalf("reserved pause command unexpectedly exposed: %d", response.StatusCode)
	}
}

func TestHostAndOriginGuards(t *testing.T) {
	server := httptest.NewServer(testHandler(t, "/missing.sock"))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/sessions", nil)
	request.Host = "attacker.example"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected Host accepted: %d", response.StatusCode)
	}

	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/sessions/sess_1/instruction", strings.NewReader(`{"text":"test"}`))
	request.Header.Set("Origin", "https://attacker.example")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin request accepted: %d", response.StatusCode)
	}
}

func TestListenAddressCannotBindPublicInterface(t *testing.T) {
	t.Setenv("PI_REMOTE_HOST", "0.0.0.0")
	t.Setenv("PI_REMOTE_PORT", "8765")
	if _, err := listenAddress(); err == nil {
		t.Fatal("public bind address was accepted")
	}
	t.Setenv("PI_REMOTE_HOST", "127.0.0.1")
	address, err := listenAddress()
	if err != nil || address != "127.0.0.1:8765" {
		t.Fatalf("loopback listener rejected: address=%q err=%v", address, err)
	}
}

func TestLiveSSESnapshot(t *testing.T) {
	server := httptest.NewServer(testHandler(t, "/missing.sock"))
	defer server.Close()
	response, err := http.Get(server.URL + "/api/live")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected live stream response: status=%d headers=%v", response.StatusCode, response.Header)
	}
	reader := bufio.NewReader(response.Body)
	var payload string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			break
		}
	}
	var update map[string]any
	if err := json.Unmarshal([]byte(payload), &update); err != nil {
		t.Fatal(err)
	}
	if update["type"] != "snapshot" || update["connected"] != false {
		t.Fatalf("unexpected live snapshot: %v", update)
	}
}

func TestEmbeddedPWAAssets(t *testing.T) {
	for _, name := range []string{"static/index.html", "static/app.js", "static/app.css", "static/manifest.webmanifest", "static/sw.js", "static/icon.svg"} {
		file, err := webAssets.Open(name)
		if err != nil {
			t.Fatalf("missing embedded asset %s: %v", name, err)
		}
		_ = file.Close()
	}
}

func TestParseAllowListsAndSessionIDs(t *testing.T) {
	hosts := parseHostAllowlist(" remote.example:443,localhost ")
	if _, ok := hosts["remote.example"]; !ok {
		t.Fatalf("host allowlist was not normalized: %v", hosts)
	}
	origins := parseOriginAllowlist("https://remote.example, javascript:alert(1), https://bad.example/path")
	if len(origins) != 1 {
		t.Fatalf("invalid origins were retained: %v", origins)
	}
	if !validSessionID("session_1-abc") || validSessionID("../other") || validSessionID(strings.Repeat("x", 129)) {
		t.Fatal("session id validation failed")
	}
}

func TestDefaultBridgeSocketEnvironment(t *testing.T) {
	t.Setenv("PI_HARNESS_SOCKET", "/tmp/custom/bridge.sock")
	if got := defaultBridgePath(); got != "/tmp/custom/bridge.sock" {
		t.Fatalf("unexpected bridge socket path: %s", got)
	}
}
