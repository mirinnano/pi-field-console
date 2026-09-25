package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryAPIListsFoldersAndExplicitlyLoadsOfflineTranscript(t *testing.T) {
	root := t.TempDir()
	file := writeHistorySession(t, filepath.Join(root, "--encoded--"), "history-file.jsonl", "history_session_1", "/work/project", 3,
		historyEntry("user_1", "", "message", historyTestTime, map[string]any{
			"message": makeHistoryMessage("user", "selected offline transcript", float64(1735787045000)),
		}),
		historyEntry("info_1", "user_1", "session_info", historyTestTime, map[string]any{"name": "Past work"}),
	)
	app := newService(filepath.Join(root, "missing.sock"))
	app.history = newSessionHistory([]string{root})
	app.historyRoots = []string{root}
	app.herdr = &herdrClient{enabled: false}
	server := httptest.NewServer(app.handler(config{}, webAssets))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/history")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Folders []struct {
			Path     string               `json:"path"`
			Sessions []historySessionView `json:"sessions"`
		} `json:"folders"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || len(payload.Folders) != 1 {
		t.Fatalf("unexpected history listing: status=%d payload=%+v", response.StatusCode, payload)
	}
	folder := payload.Folders[0]
	if folder.Path != "/work/project" || len(folder.Sessions) != 1 || folder.Sessions[0].SessionID != "history_session_1" || folder.Sessions[0].Connected || folder.Sessions[0].Status != "offline" || folder.Sessions[0].Name != "Past work" {
		t.Fatalf("unexpected grouped history: %+v", folder)
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), "selected offline transcript") || strings.Contains(string(encoded), file) {
		t.Fatal("history index leaked transcript body or backing file path")
	}

	response, err = http.Get(server.URL + "/api/history/history_session_1/transcript")
	if err != nil {
		t.Fatal(err)
	}
	var transcript historyTranscript
	if err := json.NewDecoder(response.Body).Decode(&transcript); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || len(transcript.Messages) != 1 || transcript.Messages[0].Content[0].Text != "selected offline transcript" {
		t.Fatalf("unexpected offline transcript: status=%d transcript=%+v", response.StatusCode, transcript)
	}
}

func TestHerdrHandlerLinksHistoryAndTargetsExactPane(t *testing.T) {
	root := t.TempDir()
	sessionFile := writeHistorySession(t, filepath.Join(root, "--private-folder-token--"), "private-session-file-token.jsonl", "linked_session_1", "/work/project", 3,
		historyEntry("user_1", "", "message", historyTestTime, map[string]any{
			"message": makeHistoryMessage("user", "private-transcript-body-token", float64(1735787045000)),
		}),
	)
	const linkedPaneID = "pane-linked"
	fixture, err := json.Marshal(map[string]any{
		"id": "snapshot-request",
		"result": map[string]any{
			"type": "session_snapshot",
			"snapshot": map[string]any{
				"version": "test", "protocol": 1,
				"focused_workspace_id": "workspace-1", "focused_tab_id": "tab-1", "focused_pane_id": linkedPaneID,
				"workspaces": []map[string]any{{"workspace_id": "workspace-1", "label": "project", "number": 1}},
				"tabs":       []map[string]any{{"tab_id": "tab-1", "workspace_id": "workspace-1", "number": 1}},
				"panes": []map[string]any{
					{"pane_id": linkedPaneID, "workspace_id": "workspace-1", "tab_id": "tab-1", "agent": "pi", "agent_status": "working", "cwd": "/work/project", "agent_session": map[string]any{"agent": "pi", "kind": "session_file", "source": "fixture", "value": sessionFile}},
					{"pane_id": "pane-near-match", "workspace_id": "workspace-1", "tab_id": "tab-1", "agent": "pi", "agent_status": "working", "agent_session": map[string]any{"agent": "pi", "kind": "session_file", "source": "fixture", "value": sessionFile + ".backup"}},
				},
				"agents": []map[string]any{
					{"pane_id": linkedPaneID, "agent": "pi", "agent_status": "working"},
					{"pane_id": "pane-near-match", "agent": "pi", "agent_status": "working"},
				},
			},
		},
		"error": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	fakeHerdr := filepath.Join(root, "fake-herdr")
	script := `#!/bin/sh
if [ "$1" = "api" ] && [ "$2" = "snapshot" ]; then
  printf '%s\n' "$HERDR_TEST_SNAPSHOT"
  exit 0
fi
if [ "$1" = "agent" ] && [ "$2" = "prompt" ]; then
  printf '%s\n' "$@" >> "$HERDR_TEST_CALLS"
  printf '{}\n'
  exit 0
fi
exit 64
`
	if err := os.WriteFile(fakeHerdr, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(root, "herdr-call-log")
	t.Setenv("HERDR_TEST_SNAPSHOT", string(fixture))
	t.Setenv("HERDR_TEST_CALLS", callLog)

	app := newService(filepath.Join(root, "missing-bridge.sock"))
	app.history = newSessionHistory(nil)
	app.historyRoots = nil
	app.herdr = &herdrClient{binary: fakeHerdr, enabled: true}
	server := httptest.NewServer(app.handler(config{}, webAssets))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/herdr")
	if err != nil {
		t.Fatal(err)
	}
	herdrBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var overview herdrOverview
	if err := json.Unmarshal(herdrBody, &overview); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !overview.Available || len(overview.Panes) != 2 {
		t.Fatalf("unexpected Herdr overview: status=%d overview=%+v", response.StatusCode, overview)
	}
	var linkedPane, nearMatchPane *herdrPane
	for i := range overview.Panes {
		switch overview.Panes[i].ID {
		case linkedPaneID:
			linkedPane = &overview.Panes[i]
		case "pane-near-match":
			nearMatchPane = &overview.Panes[i]
		}
	}
	if linkedPane == nil || linkedPane.SessionID != "linked_session_1" || nearMatchPane == nil || nearMatchPane.SessionID != "" {
		t.Fatalf("Herdr panes were not linked to the exact history file: %+v", overview.Panes)
	}

	response, err = http.Get(server.URL + "/api/history")
	if err != nil {
		t.Fatal(err)
	}
	historyBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var history struct {
		Folders []historyFolderView `json:"folders"`
	}
	if err := json.Unmarshal(historyBody, &history); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(history.Folders) != 1 || history.Folders[0].Path != "/work/project" || len(history.Folders[0].Sessions) != 1 || history.Folders[0].Sessions[0].SessionID != "linked_session_1" {
		t.Fatalf("history handler did not index the pane's session file: status=%d history=%+v", response.StatusCode, history)
	}
	for _, body := range []string{string(herdrBody), string(historyBody)} {
		for _, secret := range []string{root, sessionFile, filepath.Base(sessionFile), "private-transcript-body-token"} {
			if strings.Contains(body, secret) {
				t.Errorf("handler response exposed private history data %q", secret)
			}
		}
	}

	response, err = http.Post(server.URL+"/api/herdr/panes/"+linkedPane.ID+"/instruction", "application/json", strings.NewReader(`{"text":"verify linked pane"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("pane instruction returned %d", response.StatusCode)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	wantCall := []string{"agent", "prompt", linkedPaneID, "--", "verify linked pane"}
	if got := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(got) != len(wantCall) {
		t.Fatalf("fake Herdr recorded unexpected action arguments: %q", calls)
	} else {
		for i := range wantCall {
			if got[i] != wantCall[i] {
				t.Fatalf("fake Herdr action args = %q, want %q", got, wantCall)
			}
		}
	}
}

func TestHerdrAPIIsOptionalAndRejectsInvalidPaneBeforeExecuting(t *testing.T) {
	app := newService("/missing.sock")
	app.history = newSessionHistory(nil)
	app.historyRoots = nil
	app.herdr = &herdrClient{binary: "/must-not-run", enabled: false}
	server := httptest.NewServer(app.handler(config{}, webAssets))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/herdr")
	if err != nil {
		t.Fatal(err)
	}
	var overview herdrOverview
	if err := json.NewDecoder(response.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || overview.Available || len(overview.Workspaces) != 0 {
		t.Fatalf("unexpected unavailable Herdr response: status=%d overview=%+v", response.StatusCode, overview)
	}

	response, err = http.Get(server.URL + "/api/herdr/panes/bad%24id/output")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid pane id was not rejected before command execution: %d", response.StatusCode)
	}

	response, err = http.Post(server.URL+"/api/herdr/panes/p1/instruction", "application/json", strings.NewReader(`{"text":"do work","unexpected":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("disabled Herdr instruction route returned %d", response.StatusCode)
	}
}
