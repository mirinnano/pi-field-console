package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectSessionsRemovesPrivatePathsAndHerdrIdentity(t *testing.T) {
	input := []byte(`[{"sessionId":"sess_1","cwd":"/repo","name":"work","connected":true,"status":"working","eventSeq":7,"updatedAt":"2026-09-24T12:00:00Z","sessionFile":"/private/session.jsonl","sessionDir":"/private/sessions","herdrWorkspaceId":"w1","herdrTabId":"t1","herdrPaneId":"p1","tokens":{"secret":"no"},"telemetry":{"contextTokens":4}}]`)
	public, links, err := projectSessions(input)
	if err != nil {
		t.Fatal(err)
	}
	if links["/private/session.jsonl"] != "sess_1" {
		t.Fatalf("internal exact session link missing: %#v", links)
	}
	var sessions []map[string]json.RawMessage
	if err := json.Unmarshal(public, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || string(sessions[0]["sessionId"]) != `"sess_1"` || string(sessions[0]["cwd"]) != `"/repo"` || string(sessions[0]["eventSeq"]) != `7` {
		t.Fatalf("unexpected projected session: %s", public)
	}
	for _, secret := range []string{"sessionFile", "sessionDir", "herdrWorkspaceId", "herdrTabId", "herdrPaneId", "tokens", "/private/session.jsonl"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("private field leaked to public sessions payload: %q", secret)
		}
	}
}
