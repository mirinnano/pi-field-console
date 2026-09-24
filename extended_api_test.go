package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
