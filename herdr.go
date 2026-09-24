package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxHerdrSnapshotBytes = 4 * 1024 * 1024
	maxHerdrOutputBytes   = 128 * 1024
	maxHerdrReadLines     = 100
)

var (
	errHerdrInstructionOutcomeUnknown = errors.New("Herdr instruction outcome unknown")
	herdrIDPattern                    = regexp.MustCompile(`^[A-Za-z0-9_-]+(?::[A-Za-z0-9_-]+)*$`)
	herdrEscapePattern                = regexp.MustCompile(`\x1b(\][^\x07]*(\x07|\x1b\\)|\[[0-?]*[ -/]*[@-~]|[P^_][\s\S]*?(\x1b\\|\x07))`)
	herdrEnvLinePattern               = regexp.MustCompile(`^\s*((export\s+|declare\s+-x\s+)?[A-Za-z_][A-Za-z0-9_]{0,127}=)`)
	herdrEnvKeyPattern                = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	herdrControlPattern               = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2060}\x{2066}-\x{2069}\x{feff}]`)
)

type herdrClient struct {
	binary    string
	enabled   bool
	ownPaneID string
}

type herdrEnvelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type herdrSnapshotEnvelope struct {
	Type     string           `json:"type"`
	Snapshot herdrRawSnapshot `json:"snapshot"`
}

type herdrRawSnapshot struct {
	Version            string              `json:"version"`
	Protocol           int                 `json:"protocol"`
	FocusedWorkspaceID *string             `json:"focused_workspace_id"`
	FocusedTabID       *string             `json:"focused_tab_id"`
	FocusedPaneID      *string             `json:"focused_pane_id"`
	Workspaces         []herdrRawWorkspace `json:"workspaces"`
	Tabs               []herdrRawTab       `json:"tabs"`
	Panes              []herdrRawPane      `json:"panes"`
	Agents             []herdrRawAgent     `json:"agents"`
}

type herdrRawWorkspace struct {
	WorkspaceID string            `json:"workspace_id"`
	Label       string            `json:"label"`
	Number      int               `json:"number"`
	Focused     bool              `json:"focused"`
	ActiveTabID string            `json:"active_tab_id"`
	AgentStatus string            `json:"agent_status"`
	Worktree    *herdrRawWorktree `json:"worktree"`
	Tokens      map[string]string `json:"tokens"`
}

type herdrRawWorktree struct {
	RepoName         string `json:"repo_name"`
	RepoRoot         string `json:"repo_root"`
	CheckoutPath     string `json:"checkout_path"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}

type herdrRawTab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus string `json:"agent_status"`
}

type herdrRawPane struct {
	PaneID        string           `json:"pane_id"`
	WorkspaceID   string           `json:"workspace_id"`
	TabID         string           `json:"tab_id"`
	Focused       bool             `json:"focused"`
	AgentStatus   string           `json:"agent_status"`
	Agent         *string          `json:"agent"`
	DisplayAgent  *string          `json:"display_agent"`
	CWD           *string          `json:"cwd"`
	ForegroundCWD *string          `json:"foreground_cwd"`
	Label         *string          `json:"label"`
	Title         *string          `json:"title"`
	AgentSession  *herdrSessionRef `json:"agent_session"`
}

type herdrRawAgent struct {
	PaneID       string           `json:"pane_id"`
	WorkspaceID  string           `json:"workspace_id"`
	TabID        string           `json:"tab_id"`
	Agent        *string          `json:"agent"`
	DisplayAgent *string          `json:"display_agent"`
	AgentStatus  string           `json:"agent_status"`
	Name         *string          `json:"name"`
	CWD          *string          `json:"cwd"`
	Focused      bool             `json:"focused"`
	AgentSession *herdrSessionRef `json:"agent_session"`
}

type herdrSessionRef struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Value  string `json:"value"`
}

type herdrOverview struct {
	Available          bool             `json:"available"`
	Version            string           `json:"version,omitempty"`
	Protocol           int              `json:"protocol,omitempty"`
	FocusedWorkspaceID *string          `json:"focusedWorkspaceId,omitempty"`
	FocusedTabID       *string          `json:"focusedTabId,omitempty"`
	FocusedPaneID      *string          `json:"focusedPaneId,omitempty"`
	Workspaces         []herdrWorkspace `json:"workspaces"`
	Tabs               []herdrTab       `json:"tabs"`
	Panes              []herdrPane      `json:"panes"`
}

type herdrWorkspace struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Number       int    `json:"number"`
	Focused      bool   `json:"focused"`
	ActiveTabID  string `json:"activeTabId"`
	AgentStatus  string `json:"agentStatus"`
	RepoName     string `json:"repoName,omitempty"`
	RepoRoot     string `json:"repoRoot,omitempty"`
	CheckoutPath string `json:"checkoutPath,omitempty"`
	Linked       bool   `json:"linkedWorktree,omitempty"`
}

type herdrTab struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"paneCount"`
	AgentStatus string `json:"agentStatus"`
}

type herdrPane struct {
	ID            string `json:"id"`
	WorkspaceID   string `json:"workspaceId"`
	TabID         string `json:"tabId"`
	Focused       bool   `json:"focused"`
	AgentStatus   string `json:"agentStatus"`
	Agent         string `json:"agent,omitempty"`
	CanAct        bool   `json:"canAct"`
	Label         string `json:"label,omitempty"`
	Title         string `json:"title,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	ForegroundCWD string `json:"foregroundCwd,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
}

func newHerdrClient() *herdrClient {
	binary := strings.TrimSpace(os.Getenv("HERDR_BIN_PATH"))
	if binary == "" {
		binary = "herdr"
	}
	ownPaneID := strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
	if len(ownPaneID) > 256 || !herdrIDPattern.MatchString(ownPaneID) {
		ownPaneID = ""
	}
	return &herdrClient{
		binary: binary, enabled: os.Getenv("HERDR_ENV") == "1" && strings.TrimSpace(os.Getenv("HERDR_SOCKET_PATH")) != "",
		ownPaneID: ownPaneID,
	}
}

func (c *herdrClient) available() bool {
	return c != nil && c.enabled && c.binary != ""
}

func (c *herdrClient) run(ctx context.Context, maxBytes int, args ...string) ([]byte, error) {
	if !c.available() {
		return nil, errors.New("Herdr unavailable")
	}
	if maxBytes <= 0 {
		maxBytes = maxHerdrOutputBytes
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.binary, args...)
	var stdout limitedBuffer
	stdout.limit = maxBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &limitedBuffer{limit: 8 * 1024}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Herdr request timed out")
		}
		return nil, errors.New("Herdr request failed")
	}
	return stdout.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.Len()+len(p) > b.limit {
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func parseHerdrSnapshot(data []byte) (herdrRawSnapshot, error) {
	var envelope herdrEnvelope
	if len(data) == 0 || len(data) > maxHerdrSnapshotBytes || json.Unmarshal(data, &envelope) != nil || len(envelope.Result) == 0 || !isJSONNull(envelope.Error) {
		return herdrRawSnapshot{}, errors.New("invalid Herdr snapshot")
	}
	var result herdrSnapshotEnvelope
	if json.Unmarshal(envelope.Result, &result) != nil || result.Type != "session_snapshot" {
		return herdrRawSnapshot{}, errors.New("invalid Herdr snapshot")
	}
	return result.Snapshot, nil
}

func (c *herdrClient) snapshot(ctx context.Context) (herdrRawSnapshot, error) {
	data, err := c.run(ctx, maxHerdrSnapshotBytes, "api", "snapshot")
	if err != nil {
		return herdrRawSnapshot{}, err
	}
	return parseHerdrSnapshot(data)
}

func herdrText(value *string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return ""
	}
	return strings.TrimSpace(*value)
}

func normalizeSessionPath(value string) string {
	if !filepath.IsAbs(value) {
		return ""
	}
	return filepath.Clean(value)
}

func projectHerdrSnapshot(raw herdrRawSnapshot, sessionFiles map[string]string, ownPaneID string) herdrOverview {
	result := herdrOverview{
		Available:          true,
		Version:            raw.Version,
		Protocol:           raw.Protocol,
		FocusedWorkspaceID: raw.FocusedWorkspaceID,
		FocusedTabID:       raw.FocusedTabID,
		FocusedPaneID:      raw.FocusedPaneID,
		Workspaces:         make([]herdrWorkspace, 0, len(raw.Workspaces)),
		Tabs:               make([]herdrTab, 0, len(raw.Tabs)),
		Panes:              make([]herdrPane, 0, len(raw.Panes)),
	}
	for _, workspace := range raw.Workspaces {
		if !herdrIDPattern.MatchString(workspace.WorkspaceID) {
			continue
		}
		item := herdrWorkspace{ID: workspace.WorkspaceID, Label: workspace.Label, Number: workspace.Number, Focused: workspace.Focused, ActiveTabID: workspace.ActiveTabID, AgentStatus: workspace.AgentStatus}
		if workspace.Worktree != nil {
			item.RepoName = workspace.Worktree.RepoName
			item.RepoRoot = workspace.Worktree.RepoRoot
			item.CheckoutPath = workspace.Worktree.CheckoutPath
			item.Linked = workspace.Worktree.IsLinkedWorktree
		}
		result.Workspaces = append(result.Workspaces, item)
	}
	for _, tab := range raw.Tabs {
		if !herdrIDPattern.MatchString(tab.TabID) || !herdrIDPattern.MatchString(tab.WorkspaceID) {
			continue
		}
		result.Tabs = append(result.Tabs, herdrTab{ID: tab.TabID, WorkspaceID: tab.WorkspaceID, Number: tab.Number, Label: tab.Label, Focused: tab.Focused, PaneCount: tab.PaneCount, AgentStatus: tab.AgentStatus})
	}

	agents := make(map[string]herdrRawAgent, len(raw.Agents))
	for _, agent := range raw.Agents {
		if herdrIDPattern.MatchString(agent.PaneID) {
			agents[agent.PaneID] = agent
		}
	}
	for _, pane := range raw.Panes {
		if !herdrIDPattern.MatchString(pane.PaneID) || !herdrIDPattern.MatchString(pane.WorkspaceID) || !herdrIDPattern.MatchString(pane.TabID) {
			continue
		}
		agent := herdrText(pane.Agent)
		if agent == "" {
			if detail, ok := agents[pane.PaneID]; ok {
				agent = herdrText(detail.Agent)
				if agent == "" {
					agent = herdrText(detail.DisplayAgent)
				}
			}
		}
		if agent == "" {
			agent = herdrText(pane.DisplayAgent)
		}
		item := herdrPane{
			ID:            pane.PaneID,
			WorkspaceID:   pane.WorkspaceID,
			TabID:         pane.TabID,
			Focused:       pane.Focused,
			AgentStatus:   pane.AgentStatus,
			Agent:         agent,
			CanAct:        pane.PaneID != ownPaneID && findHerdrAgent(raw, pane.PaneID) != nil,
			Label:         herdrText(pane.Label),
			Title:         herdrText(pane.Title),
			CWD:           herdrText(pane.CWD),
			ForegroundCWD: herdrText(pane.ForegroundCWD),
		}
		if pane.AgentSession != nil {
			if sessionID := sessionFiles[normalizeSessionPath(pane.AgentSession.Value)]; sessionID != "" {
				item.SessionID = sessionID
			}
		}
		result.Panes = append(result.Panes, item)
	}
	return result
}

func (c *herdrClient) readPane(ctx context.Context, paneID string) (string, error) {
	if !herdrIDPattern.MatchString(paneID) {
		return "", errors.New("invalid pane id")
	}
	snapshot, err := c.snapshot(ctx)
	if err != nil {
		return "", err
	}
	if findHerdrPane(snapshot, paneID) == nil {
		return "", errors.New("pane unavailable")
	}
	data, err := c.run(ctx, maxHerdrOutputBytes, "pane", "read", paneID, "--source", "recent-unwrapped", "--lines", fmt.Sprint(maxHerdrReadLines), "--format", "text")
	if err != nil {
		return "", err
	}
	return sanitizeHerdrPaneOutput(string(data), 12_000), nil
}

func (c *herdrClient) focusAgent(ctx context.Context, snapshot herdrRawSnapshot, paneID string) error {
	if !herdrIDPattern.MatchString(paneID) || findHerdrAgent(snapshot, paneID) == nil {
		return errors.New("agent unavailable")
	}
	if c.ownPaneID != "" && paneID == c.ownPaneID {
		return errors.New("cannot target current Pi pane")
	}
	_, err := c.run(ctx, maxHerdrOutputBytes, "agent", "focus", paneID)
	return err
}

func (c *herdrClient) promptAgent(ctx context.Context, snapshot herdrRawSnapshot, paneID, text string) error {
	if !herdrIDPattern.MatchString(paneID) || findHerdrAgent(snapshot, paneID) == nil {
		return errors.New("agent unavailable")
	}
	if c.ownPaneID != "" && paneID == c.ownPaneID {
		return errors.New("cannot target current Pi pane")
	}
	if strings.TrimSpace(text) == "" || len(text) > maxInstruction {
		return errors.New("instruction must be 1–8000 bytes")
	}
	if findHerdrAgent(snapshot, paneID).AgentStatus == "blocked" {
		return errors.New("agent blocked; resolve it in Herdr first")
	}
	if _, err := c.run(ctx, maxHerdrOutputBytes, "agent", "prompt", paneID, "--", text); err != nil {
		return errHerdrInstructionOutcomeUnknown
	}
	return nil
}

func isJSONNull(value json.RawMessage) bool {
	return len(value) == 0 || string(bytes.TrimSpace(value)) == "null"
}

func findHerdrPane(snapshot herdrRawSnapshot, paneID string) *herdrRawPane {
	for index := range snapshot.Panes {
		if snapshot.Panes[index].PaneID == paneID {
			return &snapshot.Panes[index]
		}
	}
	return nil
}

func findHerdrAgent(snapshot herdrRawSnapshot, paneID string) *herdrRawAgent {
	for index := range snapshot.Agents {
		if snapshot.Agents[index].PaneID == paneID && herdrText(snapshot.Agents[index].Agent) != "" {
			return &snapshot.Agents[index]
		}
	}
	for index := range snapshot.Panes {
		if snapshot.Panes[index].PaneID == paneID && herdrText(snapshot.Panes[index].Agent) != "" {
			return &herdrRawAgent{PaneID: paneID, Agent: snapshot.Panes[index].Agent, AgentStatus: snapshot.Panes[index].AgentStatus}
		}
	}
	return nil
}

func sanitizeHerdrPaneOutput(value string, maxChars int) string {
	value = herdrEscapePattern.ReplaceAllString(value, "")
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	value = herdrControlPattern.ReplaceAllString(value, "")
	if looksLikeHerdrEnvironment(value) {
		return "[pane output suppressed: environment data]"
	}
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		if herdrEnvLinePattern.MatchString(line) {
			lines[index] = "[environment variable redacted]"
		}
	}
	value = strings.Join(lines, "\n")
	if maxChars > 0 && utf8.RuneCountInString(value) > maxChars {
		runes := []rune(value)
		value = string(runes[:maxChars]) + "\n…（省略）"
	}
	return value
}

func looksLikeHerdrEnvironment(value string) bool {
	if len(value) > 24_000 || !strings.HasPrefix(strings.TrimSpace(value), "{") {
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &object) != nil || len(object) < 3 {
		return false
	}
	for key, raw := range object {
		if !herdrEnvKeyPattern.MatchString(key) {
			return false
		}
		var scalar any
		if json.Unmarshal(raw, &scalar) != nil {
			return false
		}
		switch scalar.(type) {
		case nil, bool, float64, string:
		default:
			return false
		}
	}
	return true
}

func limitUTF8Text(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "�")
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "\n…（省略）"
}
