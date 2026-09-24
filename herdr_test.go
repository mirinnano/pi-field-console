package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseHerdrSnapshotAcceptsNullError(t *testing.T) {
	data := []byte(`{"id":"req-1","result":{"type":"session_snapshot","snapshot":{"version":"1","protocol":1,"workspaces":[],"tabs":[],"panes":[],"agents":[]}},"error":null}`)
	snapshot, err := parseHerdrSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != "1" || snapshot.Protocol != 1 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestParseHerdrSnapshotRejectsErrorsAndOversizedData(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"id":"req-1","result":null,"error":{"message":"failed"}}`),
		[]byte(`{"id":"req-1","result":{"type":"wrong"},"error":null}`),
		make([]byte, maxHerdrSnapshotBytes+1),
	} {
		if _, err := parseHerdrSnapshot(data); err == nil {
			t.Fatal("expected invalid snapshot to be rejected")
		}
	}
}

func TestProjectHerdrSnapshotLinksExactSessionFileWithoutLeakingIt(t *testing.T) {
	const sessionFile = "/private/workspace/.pi/agent/sessions/session.jsonl"
	cwd := "/private/workspace"
	sessionID := "session_123"
	raw := herdrRawSnapshot{
		FocusedWorkspaceID: stringPointer("w1"),
		FocusedTabID:       stringPointer("t1"),
		FocusedPaneID:      stringPointer("p1"),
		Workspaces: []herdrRawWorkspace{{
			WorkspaceID: "w1", Label: "project", Number: 1, Focused: true,
			ActiveTabID: "t1", AgentStatus: "working",
			Worktree: &herdrRawWorktree{RepoName: "project", RepoRoot: cwd, CheckoutPath: cwd, IsLinkedWorktree: true},
			Tokens:   map[string]string{"access_token": "must-not-escape"},
		}},
		Tabs: []herdrRawTab{{TabID: "t1", WorkspaceID: "w1", Number: 1, Label: "main", Focused: true, PaneCount: 1, AgentStatus: "working"}},
		Panes: []herdrRawPane{
			{PaneID: "p1", WorkspaceID: "w1", TabID: "t1", Focused: true, AgentStatus: "working", Agent: stringPointer("pi"), CWD: &cwd, AgentSession: &herdrSessionRef{Value: sessionFile}},
			{PaneID: "../bad", WorkspaceID: "w1", TabID: "t1"},
			{PaneID: "p2", WorkspaceID: "w1", TabID: "t1", DisplayAgent: stringPointer("preview"), AgentSession: &herdrSessionRef{Value: sessionFile + ".other"}},
		},
		Agents: []herdrRawAgent{{PaneID: "p1", Agent: stringPointer("pi"), AgentStatus: "working"}},
	}
	overview := projectHerdrSnapshot(raw, map[string]string{normalizeSessionPath(sessionFile): sessionID}, "")
	if len(overview.Workspaces) != 1 || len(overview.Tabs) != 1 || len(overview.Panes) != 2 {
		t.Fatalf("invalid Herdr IDs were not filtered: %+v", overview)
	}
	if overview.Panes[0].SessionID != sessionID || overview.Panes[1].SessionID != "" {
		t.Fatalf("pane matching was not exact: %+v", overview.Panes)
	}
	if !overview.Panes[0].CanAct || overview.Panes[1].CanAct || overview.Panes[1].Agent != "preview" {
		t.Fatalf("display-only pane got mutation capability: %+v", overview.Panes)
	}
	selfOverview := projectHerdrSnapshot(raw, nil, "p1")
	if selfOverview.Panes[0].CanAct {
		t.Fatal("calling Pi pane remained actionable")
	}
	encoded, err := json.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{sessionFile, "must-not-escape"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private snapshot value leaked in projection: %q", secret)
		}
	}
}

func TestHerdrMutationsRejectTheCallingPane(t *testing.T) {
	snapshot := herdrRawSnapshot{
		Panes:  []herdrRawPane{{PaneID: "p1", Agent: stringPointer("pi"), AgentStatus: "working"}},
		Agents: []herdrRawAgent{{PaneID: "p1", Agent: stringPointer("pi"), AgentStatus: "working"}},
	}
	client := &herdrClient{ownPaneID: "p1"}
	if err := client.focusAgent(context.Background(), snapshot, "p1"); err == nil || err.Error() != "cannot target current Pi pane" {
		t.Fatalf("focus self was not rejected: %v", err)
	}
	if err := client.promptAgent(context.Background(), snapshot, "p1", "hello"); err == nil || err.Error() != "cannot target current Pi pane" {
		t.Fatalf("prompt self was not rejected: %v", err)
	}
}

func TestHerdrPromptCommandFailureIsExplicitlyUncertain(t *testing.T) {
	falseCommand, err := exec.LookPath("false")
	if err != nil {
		t.Skip("false command unavailable")
	}
	snapshot := herdrRawSnapshot{
		Panes:  []herdrRawPane{{PaneID: "p1", Agent: stringPointer("pi"), AgentStatus: "working"}},
		Agents: []herdrRawAgent{{PaneID: "p1", Agent: stringPointer("pi"), AgentStatus: "working"}},
	}
	client := &herdrClient{binary: falseCommand, enabled: true}
	if err := client.promptAgent(context.Background(), snapshot, "p1", "hello"); !errors.Is(err, errHerdrInstructionOutcomeUnknown) {
		t.Fatalf("prompt failure was not marked uncertain: %v", err)
	}
	response := httptest.NewRecorder()
	writeHerdrError(response, errHerdrInstructionOutcomeUnknown)
	if response.Code != 502 {
		t.Fatalf("unexpected status for uncertain prompt: %d", response.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "herdr_instruction_uncertain" || !strings.Contains(body["error"], "verify before retrying") {
		t.Fatalf("uncertainty warning missing: %#v", body)
	}
}

func TestHerdrPaneOutputRedactsTerminalControlsAndEnvironment(t *testing.T) {
	output := sanitizeHerdrPaneOutput("hello\x1b[31m red\x1b[0m\nACCESS_TOKEN=secret\nnormal", 100)
	if strings.Contains(output, "secret") || strings.ContainsRune(output, '\x1b') || !strings.Contains(output, "hello red") || !strings.Contains(output, "[environment variable redacted]") {
		t.Fatalf("pane output was not sanitized: %q", output)
	}
	jsonEnvironment := `{"PATH":"/private","HOME":"/private/home","HERDR_ENV":"1"}`
	if got := sanitizeHerdrPaneOutput(jsonEnvironment, 100); !strings.Contains(got, "suppressed") || strings.Contains(got, "private") {
		t.Fatalf("environment object was not suppressed: %q", got)
	}
	if got := sanitizeHerdrPaneOutput("日本"+strings.Repeat("x", 20), 4); utf8.RuneCountInString(got) >= 20 || !strings.HasSuffix(got, "…（省略）") {
		t.Fatalf("pane output was not bounded: %q", got)
	}
}

func TestLimitUTF8TextTruncatesAtRuneBoundary(t *testing.T) {
	got := limitUTF8Text("ab日本語", 5)
	if !strings.HasPrefix(got, "ab日") || !strings.HasSuffix(got, "…（省略）") {
		t.Fatalf("unexpected truncated text: %q", got)
	}
}

func stringPointer(value string) *string { return &value }
