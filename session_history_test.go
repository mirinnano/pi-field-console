package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const historyTestTime = "2025-01-02T03:04:05.000Z"

func writeHistorySession(t *testing.T, dir, filename, id, cwd string, version int, records ...map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filename)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	write := func(value any) {
		t.Helper()
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, writeErr := file.Write(append(encoded, '\n')); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	write(map[string]any{"type": "session", "version": version, "id": id, "timestamp": historyTestTime, "cwd": cwd})
	for _, record := range records {
		write(record)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func historyEntry(id, parent, kind, timestamp string, extra map[string]any) map[string]any {
	entry := map[string]any{"type": kind, "id": id, "parentId": parent, "timestamp": timestamp}
	for key, value := range extra {
		entry[key] = value
	}
	return entry
}

func makeHistoryMessage(role string, content any, timestamp any) map[string]any {
	return map[string]any{"role": role, "content": content, "timestamp": timestamp}
}

func transcriptText(message historyMessage) string {
	var out strings.Builder
	for _, block := range message.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

func TestHistoryListUsesHeaderCWDAndDoesNotExposeMessageBodies(t *testing.T) {
	root := t.TempDir()
	firstDir, secondDir := filepath.Join(root, "--first-slug--"), filepath.Join(root, "--second-slug--")
	writeHistorySession(t, firstDir, "first.jsonl", "session-one", "/work/alpha/project", 3,
		historyEntry("m1", "", "message", "2025-01-02T03:05:00Z", map[string]any{"message": makeHistoryMessage("user", "body-secret-do-not-index", 1735787100000)}),
		historyEntry("info1", "m1", "session_info", "2025-01-02T03:06:00Z", map[string]any{"name": "Alpha work"}),
	)
	second := writeHistorySession(t, secondDir, "second.jsonl", "session-two", "/mnt/beta/project", 3,
		historyEntry("m2", "", "message", "2025-01-02T03:07:00Z", map[string]any{"message": makeHistoryMessage("user", "another-body-secret", 1735787220000)}),
	)
	privatePath := filepath.Join(firstDir, "private-history-file-token.jsonl")
	if err := os.Rename(filepath.Join(firstDir, "first.jsonl"), privatePath); err != nil {
		t.Fatal(err)
	}

	index := newSessionHistory([]string{root})
	listed := index.List()
	links := index.SessionFiles()
	if links[privatePath] != "session-one" || links[second] != "session-two" || len(links) != 2 {
		t.Fatalf("SessionFiles() = %#v", links)
	}
	if len(listed) != 2 {
		t.Fatalf("List() returned %d sessions, want 2", len(listed))
	}
	byID := map[string]historySession{}
	for _, item := range listed {
		byID[item.SessionID] = item
	}
	if byID["session-one"].CWD != "/work/alpha/project" || byID["session-two"].CWD != "/mnt/beta/project" {
		t.Fatalf("cwd must come from headers, not folder slugs: %#v", byID)
	}
	if filepath.Base(byID["session-one"].CWD) != filepath.Base(byID["session-two"].CWD) || byID["session-one"].CWD == byID["session-two"].CWD {
		t.Fatal("test requires distinct cwd values with identical basenames")
	}
	if byID["session-one"].Name != "Alpha work" || byID["session-one"].CreatedAt != "2025-01-02T03:04:05Z" || byID["session-one"].UpdatedAt != "2025-01-02T03:06:00Z" {
		t.Errorf("unexpected path-free metadata: %#v", byID["session-one"])
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"body-secret-do-not-index", "another-body-secret", "private-history-file-token", firstDir, secondDir} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("List() exposed %q", secret)
		}
	}
	index.SetRoots([]string{firstDir})
	listed = index.List()
	if len(listed) != 1 || listed[0].SessionID != "session-one" {
		t.Fatalf("SetRoots did not restrict index: %#v", listed)
	}
}

func TestDiscoveredRootsAreIndexedOnTheNextSingleRefresh(t *testing.T) {
	firstRoot, secondRoot := t.TempDir(), t.TempDir()
	firstPath := writeHistorySession(t, firstRoot, "first.jsonl", "first-session", "/first", 3)
	writeHistorySession(t, secondRoot, "second.jsonl", "second-session", "/second", 3)
	index := newSessionHistory([]string{firstRoot})
	if len(index.List()) != 1 {
		t.Fatal("initial root was not indexed")
	}

	index.mu.Lock()
	cached := index.metadataCache[firstPath]
	cached.metadata.Name = "cached metadata"
	index.metadataCache[firstPath] = cached
	index.mu.Unlock()

	index.setRootsForNextRefresh([]string{firstRoot, secondRoot})
	if len(index.sessions) != 1 {
		t.Fatal("discovered roots were scanned eagerly before the request's index read")
	}
	listed := index.List()
	if len(listed) != 2 {
		t.Fatalf("next index refresh returned %d sessions, want both roots: %#v", len(listed), listed)
	}
	for _, item := range listed {
		if item.SessionID == "first-session" && item.Name != "cached metadata" {
			t.Fatalf("unchanged-root metadata cache was discarded: %#v", item)
		}
	}
	if len(index.SessionFiles()) != 2 {
		t.Fatalf("SessionFiles failed to reflect both roots: %#v", index.SessionFiles())
	}
}

func TestHistoryMetadataCacheAvoidsUnchangedTailReads(t *testing.T) {
	root := t.TempDir()
	path := writeHistorySession(t, root, "cached.jsonl", "cached-session", "/cached", 3,
		historyEntry("name1", "", "session_info", historyTestTime, map[string]any{"name": "initial"}),
	)
	index := newSessionHistory([]string{root})
	index.mu.Lock()
	cached := index.metadataCache[path]
	cached.metadata.Name = "cache-hit"
	index.metadataCache[path] = cached
	index.mu.Unlock()
	if got := index.List()[0].Name; got != "cache-hit" {
		t.Fatalf("unchanged metadata was reread: %q", got)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(historyEntry("name2", "name1", "session_info", "2025-01-02T03:08:00Z", map[string]any{"name": "updated"}))
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := index.List()[0].Name; got != "updated" {
		t.Fatalf("changed metadata was not refreshed: %q", got)
	}
}

func TestHistorySkipsMalformedAndReadsLegacyLinearSessionsSafely(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "broken.jsonl"), []byte("not json\n{\"type\":\"message\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad-header.jsonl"), []byte("{\"type\":\"message\",\"id\":\"wrong\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeHistorySession(t, root, "legacy.jsonl", "legacy-session", "/legacy", 1)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{
		{"type": "message", "parentId": nil, "timestamp": "2025-01-02T03:05:00Z", "message": makeHistoryMessage("user", "legacy question", float64(1e100))},
		{"type": "message", "parentId": nil, "timestamp": "invalid", "message": makeHistoryMessage("assistant", []any{map[string]any{"type": "text", "text": "legacy answer"}}, 1735787101000)},
	}
	for _, row := range rows {
		encoded, _ := json.Marshal(row)
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	index := newSessionHistory([]string{root})
	if got := len(index.List()); got != 1 {
		t.Fatalf("malformed files were indexed; sessions=%d", got)
	}
	page, err := index.Transcript("legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].ID != "legacy_2" || page.Messages[1].ID != "legacy_3" {
		t.Fatalf("legacy linear entries not reconstructed: %#v", page.Messages)
	}
	if page.Messages[0].At != "2025-01-02T03:05:00Z" || page.Messages[1].At != "2025-01-02T03:05:01.000Z" {
		t.Fatalf("unsafe timestamps were not safely replaced: %#v", page.Messages)
	}
}

func TestHistoryTranscriptFollowsBranchAndNewestCompactionAndHidesPrivateEntries(t *testing.T) {
	root := t.TempDir()
	timestamp := historyTestTime
	rows := []map[string]any{
		historyEntry("prefix", "", "message", timestamp, map[string]any{"message": makeHistoryMessage("user", "summarized prefix secret", 1735787045000)}),
		historyEntry("abandoned", "prefix", "message", timestamp, map[string]any{"message": makeHistoryMessage("assistant", "abandoned branch secret", 1735787046000)}),
		historyEntry("kept_user", "prefix", "message", timestamp, map[string]any{"message": makeHistoryMessage("user", "kept user message", 1735787047000)}),
		historyEntry("system", "kept_user", "message", timestamp, map[string]any{"message": makeHistoryMessage("system", "private system prompt", 1735787048000)}),
		historyEntry("thinking", "system", "message", timestamp, map[string]any{"message": makeHistoryMessage("thinking", "private thinking", 1735787049000)}),
		historyEntry("hidden_custom", "thinking", "message", timestamp, map[string]any{"message": map[string]any{"role": "custom", "display": false, "content": "hidden custom role", "timestamp": 1735787050000}}),
		historyEntry("hidden_custom_message", "hidden_custom", "custom_message", timestamp, map[string]any{"customType": "hidden", "display": false, "content": "hidden extension"}),
		historyEntry("compact", "hidden_custom_message", "compaction", timestamp, map[string]any{"firstKeptEntryId": "kept_user", "summary": "private compaction summary"}),
		historyEntry("branch_summary", "compact", "branch_summary", timestamp, map[string]any{"summary": "private branch summary", "fromId": "abandoned"}),
		historyEntry("visible_extension", "branch_summary", "custom_message", timestamp, map[string]any{"customType": "notice", "display": true, "content": "visible extension"}),
		historyEntry("visible_custom", "visible_extension", "message", timestamp, map[string]any{"message": map[string]any{"role": "custom", "customType": "visible role", "display": true, "content": "visible custom", "timestamp": 1735787051000}}),
		historyEntry("assistant", "visible_custom", "message", timestamp, map[string]any{"message": map[string]any{
			"role": "assistant", "provider": "openai", "model": "model-x", "timestamp": 1735787052000,
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "hidden thinking block"},
				map[string]any{"type": "text", "text": "Calling a tool."},
				map[string]any{"type": "toolCall", "id": "call-1", "name": "bash", "arguments": map[string]any{"command": "pwd"}},
			},
		}}),
		historyEntry("tool_result", "assistant", "message", timestamp, map[string]any{"message": map[string]any{"role": "toolResult", "toolName": "bash", "isError": false, "content": []any{map[string]any{"type": "text", "text": "tool output"}}, "timestamp": 1735787053000}}),
		historyEntry("bash", "tool_result", "message", timestamp, map[string]any{"message": map[string]any{"role": "bashExecution", "command": "pwd", "output": "/work/project", "exitCode": 0, "cancelled": false, "truncated": false, "fullOutputPath": "/private/full-output.log", "timestamp": 1735787054000}}),
		historyEntry("state", "bash", "custom", timestamp, map[string]any{"data": map[string]any{"secret": "extension state"}}),
	}
	writeHistorySession(t, root, "slug-is-irrelevant.jsonl", "branch-session", "/repo", 3, rows...)
	page, err := newSessionHistory([]string{root}).Transcript("branch-session")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user", "custom", "custom", "assistant", "toolResult", "bashExecution"}
	if len(page.Messages) != len(want) {
		t.Fatalf("visible messages = %#v", page.Messages)
	}
	for i, role := range want {
		if page.Messages[i].Role != role {
			t.Errorf("role[%d]=%q, want %q", i, page.Messages[i].Role, role)
		}
	}
	if page.Messages[0].ID != "kept_user" || transcriptText(page.Messages[0]) != "kept user message" {
		t.Errorf("compaction did not retain correct range: %#v", page.Messages[0])
	}
	if page.Messages[1].Label != "notice" || page.Messages[2].Label != "visible role" {
		t.Errorf("visible custom metadata missing: %#v", page.Messages)
	}
	assistant := page.Messages[3]
	if assistant.Model == nil || assistant.Model.Provider != "openai" || assistant.Model.ID != "model-x" {
		t.Errorf("model ref missing: %#v", assistant.Model)
	}
	toolCall := false
	for _, block := range assistant.Content {
		if block.Type == "toolCall" {
			toolCall = true
			if block.Name != "bash" || block.Arguments != `{"command":"pwd"}` {
				t.Errorf("bad tool call: %#v", block)
			}
		}
	}
	if !toolCall || transcriptText(page.Messages[4]) != "tool output" || transcriptText(page.Messages[5]) != "/work/project" {
		t.Fatal("tool call, result, or bash content missing")
	}
	encoded, _ := json.Marshal(page)
	for _, secret := range []string{"summarized prefix secret", "abandoned branch secret", "private system prompt", "private thinking", "hidden custom", "hidden extension", "private compaction summary", "private branch summary", "hidden thinking block", "extension state", "/private/full-output.log"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("hidden content leaked: %q", secret)
		}
	}
	if page.Source != "pi-session" || page.Cursor != "state" || !page.Reset {
		t.Errorf("bad transcript envelope: %#v", page)
	}
}

func TestHistorySkipsSymlinksAndRejectsInvalidOrStaleIDs(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	writeHistorySession(t, inside, "in.jsonl", "inside-id", "/inside", 3)
	outside := t.TempDir()
	outsidePath := writeHistorySession(t, outside, "out.jsonl", "outside-id", "/outside", 3)
	if err := os.Symlink(outsidePath, filepath.Join(inside, "linked.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(inside, "linked-dir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	index := newSessionHistory([]string{inside})
	if got := index.List(); len(got) != 1 || got[0].SessionID != "inside-id" {
		t.Fatalf("symlink target was indexed: %#v", got)
	}
	if _, err := index.Transcript("outside-id"); err == nil {
		t.Fatal("session outside explicit root resolved")
	}
	if _, err := index.Transcript("../../outside-id"); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if err := os.Remove(filepath.Join(inside, "in.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Transcript("inside-id"); err == nil {
		t.Fatal("stale indexed file resolved")
	}
}

func TestHistoryTranscriptBoundsCountContentLinesAndFileSize(t *testing.T) {
	root := t.TempDir()
	rows := make([]map[string]any, 85)
	parent := ""
	for i := range rows {
		id := fmt.Sprintf("message_%03d", i)
		rows[i] = historyEntry(id, parent, "message", historyTestTime, map[string]any{"message": makeHistoryMessage("user", fmt.Sprintf("message %03d", i), 1735787045000+i)})
		parent = id
	}
	writeHistorySession(t, root, "many.jsonl", "many-session", "/many", 3, rows...)
	page, err := newSessionHistory([]string{root}).Transcript("many-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != historyMaxTranscriptRows || page.Messages[0].ID != "message_005" || page.Messages[79].ID != "message_084" || !page.Truncated || page.More {
		t.Fatalf("snapshot bounds not applied: %#v", page)
	}
	encoded, _ := json.Marshal(page)
	if len(encoded) > historyMaxTranscriptSize {
		t.Fatalf("response size %d exceeds 6 MiB", len(encoded))
	}

	largeRoot := t.TempDir()
	writeHistorySession(t, largeRoot, "large.jsonl", "large-session", "/large", 3,
		historyEntry("large_message", "", "message", historyTestTime, map[string]any{"message": makeHistoryMessage("assistant", []any{map[string]any{"type": "text", "text": strings.Repeat("x", 900<<10)}}, 1735787045000)}),
	)
	large, err := newSessionHistory([]string{largeRoot}).Transcript("large-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(large.Messages) != 1 || !large.Truncated || len(transcriptText(large.Messages[0])) > historyMaxTextSize {
		t.Fatalf("per-entry text bound failed: %#v", large)
	}

	lineRoot := t.TempDir()
	linePath := writeHistorySession(t, lineRoot, "line.jsonl", "line-session", "/line", 3)
	file, err := os.OpenFile(linePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oversized := map[string]any{"type": "message", "id": "too-long", "parentId": nil, "timestamp": historyTestTime, "message": makeHistoryMessage("user", strings.Repeat("q", historyMaxLineBytes+32), 1735787045000)}
	longLine, _ := json.Marshal(oversized)
	valid, _ := json.Marshal(historyEntry("after-long-line", "", "message", historyTestTime, map[string]any{"message": makeHistoryMessage("user", "valid line", 1735787046000)}))
	for _, line := range [][]byte{longLine, valid} {
		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	linePage, err := newSessionHistory([]string{lineRoot}).Transcript("line-session")
	if err != nil {
		t.Fatal(err)
	}
	if !linePage.Truncated || len(linePage.Messages) != 1 || linePage.Messages[0].ID != "after-long-line" {
		t.Fatalf("oversized line handling failed: %#v", linePage)
	}

	fileRoot := t.TempDir()
	filePath := writeHistorySession(t, fileRoot, "large-file.jsonl", "large-file", "/large-file", 3)
	if err := os.Truncate(filePath, historyMaxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := newSessionHistory([]string{fileRoot}).Transcript("large-file"); err == nil {
		t.Fatal("file over parser limit accepted")
	}
}

func TestHistoryImagesUseAllowlistAndTimestampsAreSafe(t *testing.T) {
	root := t.TempDir()
	image := strings.Repeat("AQID", 24)
	writeHistorySession(t, root, "image.jsonl", "image-session", "/images", 3,
		historyEntry("user-image", "", "message", historyTestTime, map[string]any{"message": makeHistoryMessage("user", []any{
			map[string]any{"type": "image", "mimeType": "image/png", "data": image},
			map[string]any{"type": "image", "mimeType": "image/svg+xml", "data": image},
		}, "not-a-number")}))
	page, err := newSessionHistory([]string{root}).Transcript("image-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].At != "2025-01-02T03:04:05Z" {
		t.Fatalf("safe timestamp fallback failed: %#v", page.Messages)
	}
	blocks := page.Messages[0].Content
	if len(blocks) != 2 || blocks[0].Type != "image" || blocks[1].Type != "text" || !page.Truncated {
		t.Fatalf("image allowlist/bounds failed: %#v", blocks)
	}
}
