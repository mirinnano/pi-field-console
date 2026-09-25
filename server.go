package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	bridgeVersion           = 1
	maxBridgeLine           = 64 * 1024
	maxModelBridgeLine      = 512 * 1024
	maxTranscriptBridgeLine = 8 * 1024 * 1024
	maxInstruction          = 8000
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type config struct {
	allowedHosts  map[string]struct{}
	allowedOrigin map[string]struct{}
}

type service struct {
	bridgePath    string
	hub           *liveHub
	herdr         *herdrClient
	history       *sessionHistory
	historyRoots  []string
	historyRootMu sync.Mutex
}

type bridgeReply struct {
	V    int             `json:"v"`
	Op   string          `json:"op"`
	ID   string          `json:"id"`
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data"`
	Err  string          `json:"error"`
}

func newService(bridgePath string) *service {
	roots := sessionHistoryRoots()
	history := &sessionHistory{roots: append([]string(nil), roots...), metadataCache: make(map[string]cachedHistoryMetadata)}
	return &service{
		bridgePath: bridgePath, hub: newLiveHub(), herdr: newHerdrClient(),
		history: history, historyRoots: roots,
	}
}

func sessionHistoryRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil
	}
	agentDir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR"))
	if !filepath.IsAbs(agentDir) {
		agentDir = filepath.Join(home, ".pi", "agent")
	}
	roots := make([]string, 0, 8)
	seen := make(map[string]struct{})
	add := func(value string) {
		if !filepath.IsAbs(value) || len(value) > 4096 {
			return
		}
		root := filepath.Clean(value)
		if _, exists := seen[root]; exists {
			return
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	add(filepath.Join(agentDir, "sessions"))
	add(os.Getenv("PI_CODING_AGENT_SESSION_DIR"))
	settingsPath := filepath.Join(agentDir, "settings.json")
	if info, statErr := os.Stat(settingsPath); statErr == nil && info.Mode().IsRegular() && info.Size() <= 1<<20 {
		if data, readErr := os.ReadFile(settingsPath); readErr == nil {
			var settings struct {
				SessionDir string `json:"sessionDir"`
			}
			if json.Unmarshal(data, &settings) == nil {
				add(settings.SessionDir)
			}
		}
	}
	for _, value := range strings.Split(os.Getenv("PI_REMOTE_SESSION_DIRS"), ",") {
		add(strings.TrimSpace(value))
	}
	return roots
}

func (s *service) handler(cfg config, assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/history/{sessionID}/transcript", s.handleHistoryTranscript)
	mux.HandleFunc("GET /api/sessions/{sessionID}/events", s.handleEvents)
	mux.HandleFunc("GET /api/sessions/{sessionID}/transcript", s.handleTranscript)
	mux.HandleFunc("GET /api/sessions/{sessionID}/tasks", s.handleTasks)
	mux.HandleFunc("GET /api/sessions/{sessionID}/diff", s.handleDiff)
	mux.HandleFunc("GET /api/sessions/{sessionID}/models", s.handleModels)
	mux.HandleFunc("GET /api/sessions/{sessionID}/codex-usage", s.handleCodexUsage)
	mux.HandleFunc("POST /api/sessions/{sessionID}/model", s.handleModel)
	mux.HandleFunc("POST /api/sessions/{sessionID}/instruction", s.handleInstruction)
	mux.HandleFunc("GET /api/herdr", s.handleHerdr)
	mux.HandleFunc("GET /api/herdr/panes/{paneID}/output", s.handleHerdrPaneOutput)
	mux.HandleFunc("POST /api/herdr/panes/{paneID}/focus", s.handleHerdrPaneFocus)
	mux.HandleFunc("POST /api/herdr/panes/{paneID}/instruction", s.handleHerdrPaneInstruction)
	mux.HandleFunc("GET /api/live", s.handleLive)

	static, err := fs.Sub(assets, "static")
	if err == nil {
		mux.Handle("GET /", http.FileServer(http.FS(static)))
	}
	return s.securityHeaders(s.validateRequest(cfg, mux))
}

func (s *service) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *service) validateRequest(cfg config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedRequestHost(r.Host, cfg.allowedHosts) {
			writeError(w, http.StatusForbidden, "host not allowed")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !allowedRequestOrigin(origin, r.Host, cfg.allowedOrigin) {
			writeError(w, http.StatusForbidden, "origin not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedRequestHost(requestHost string, configured map[string]struct{}) bool {
	host := normalizeHost(requestHost)
	if host == "" {
		return false
	}
	if _, ok := configured[host]; ok {
		return true
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizeHost(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(value, "[]")
}

func allowedRequestOrigin(origin, requestHost string, configured map[string]struct{}) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if _, ok := configured[origin]; ok {
		return true
	}
	return strings.EqualFold(parsed.Host, requestHost)
}

func (s *service) handleHealth(w http.ResponseWriter, r *http.Request) {
	data, err := s.bridgeRequest(r.Context(), "request_status", "", nil)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "bridge unavailable")
		return
	}
	var sessions []json.RawMessage
	if err := json.Unmarshal(data, &sessions); err != nil {
		writeError(w, http.StatusBadGateway, "invalid bridge response")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bridge": true, "sessions": len(sessions)})
}

func (s *service) handleSessions(w http.ResponseWriter, r *http.Request) {
	data, err := s.bridgeRequest(r.Context(), "request_status", "", nil)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	public, _, err := projectSessions(data)
	if err != nil {
		writeError(w, http.StatusBadGateway, "invalid bridge response")
		return
	}
	writeRawJSON(w, http.StatusOK, public)
}

func projectSessions(data []byte) (json.RawMessage, map[string]string, error) {
	var sessions []map[string]json.RawMessage
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, nil, err
	}
	public := make([]map[string]json.RawMessage, 0, len(sessions))
	links := make(map[string]string)
	allowed := map[string]bool{
		"sessionId": true, "cwd": true, "name": true, "connected": true, "status": true, "eventSeq": true,
		"updatedAt": true, "startedAt": true, "currentModel": true, "telemetry": true,
	}
	for _, session := range sessions {
		item := make(map[string]json.RawMessage)
		for key, value := range session {
			if allowed[key] {
				item[key] = value
			}
		}
		var sessionID, sessionFile string
		_ = json.Unmarshal(session["sessionId"], &sessionID)
		_ = json.Unmarshal(session["sessionFile"], &sessionFile)
		if validSessionID(sessionID) {
			if normalized := normalizeSessionPath(sessionFile); normalized != "" {
				links[normalized] = sessionID
			}
		}
		public = append(public, item)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		return nil, nil, err
	}
	return encoded, links, nil
}

type historySessionView struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Connected bool   `json:"connected"`
	Status    string `json:"status"`
}

type historyFolderView struct {
	Path     string               `json:"path"`
	Sessions []historySessionView `json:"sessions"`
}

func (s *service) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeJSON(w, http.StatusOK, map[string]any{"folders": []historyFolderView{}})
		return
	}
	var liveByID map[string]json.RawMessage
	if data, err := s.bridgeRequest(r.Context(), "request_status", "", nil); err == nil {
		s.addRegisteredHistoryRoots(data)
		var live []map[string]json.RawMessage
		if json.Unmarshal(data, &live) == nil {
			liveByID = make(map[string]json.RawMessage, len(live))
			for _, item := range live {
				var id string
				_ = json.Unmarshal(item["sessionId"], &id)
				if validSessionID(id) {
					encoded, _ := json.Marshal(item)
					liveByID[id] = encoded
				}
			}
		}
	}
	folders := make([]historyFolderView, 0)
	folderIndex := make(map[string]int)
	for _, item := range s.history.List() {
		path := item.CWD
		index, ok := folderIndex[path]
		if !ok {
			index = len(folders)
			folderIndex[path] = index
			folders = append(folders, historyFolderView{Path: path, Sessions: []historySessionView{}})
		}
		row := historySessionView{SessionID: item.SessionID, CWD: item.CWD, Name: item.Name, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, Status: "offline"}
		if encoded, exists := liveByID[item.SessionID]; exists {
			var live struct {
				Connected bool   `json:"connected"`
				Status    string `json:"status"`
				Name      string `json:"name"`
				UpdatedAt string `json:"updatedAt"`
			}
			if json.Unmarshal(encoded, &live) == nil {
				row.Connected = live.Connected
				if live.Connected && live.Status != "" {
					row.Status = live.Status
				}
				if live.Name != "" {
					row.Name = live.Name
				}
				if live.UpdatedAt != "" {
					row.UpdatedAt = live.UpdatedAt
				}
			}
		}
		folders[index].Sessions = append(folders[index].Sessions, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

func (s *service) handleHistoryTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sessionID")
	if !validSessionID(id) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if s.history == nil {
		writeError(w, http.StatusServiceUnavailable, "session history unavailable")
		return
	}
	s.history.List()
	transcript, err := s.history.Transcript(id)
	if err != nil {
		if err.Error() == "session not found" {
			writeError(w, http.StatusNotFound, "session not found")
		} else {
			writeError(w, http.StatusConflict, "session unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, transcript)
}

func (s *service) addRegisteredHistoryRoots(data []byte) {
	var sessions []struct {
		SessionFile string `json:"sessionFile"`
		SessionDir  string `json:"sessionDir"`
	}
	if json.Unmarshal(data, &sessions) != nil {
		return
	}
	roots := make([]string, 0, len(sessions)*2)
	for _, session := range sessions {
		if root := normalizeSessionPath(session.SessionDir); root != "" {
			roots = append(roots, root)
		} else if file := normalizeSessionPath(session.SessionFile); file != "" {
			roots = append(roots, file)
		}
	}
	s.addHistoryRoots(roots)
}

func (s *service) addHistoryRoots(extra []string) {
	if s.history == nil || len(extra) == 0 {
		return
	}
	s.historyRootMu.Lock()
	defer s.historyRootMu.Unlock()
	known := make(map[string]struct{}, len(s.historyRoots))
	for _, root := range s.historyRoots {
		known[root] = struct{}{}
	}
	changed := false
	for _, value := range extra {
		root := normalizeSessionPath(value)
		if root == "" {
			continue
		}
		if _, exists := known[root]; exists {
			continue
		}
		if len(s.historyRoots) >= historyMaxRoots {
			break
		}
		s.historyRoots = append(s.historyRoots, root)
		known[root] = struct{}{}
		changed = true
	}
	if changed {
		s.history.setRootsForNextRefresh(s.historyRoots)
	}
}

func (s *service) handleHerdr(w http.ResponseWriter, r *http.Request) {
	if s.herdr == nil || !s.herdr.available() {
		writeJSON(w, http.StatusOK, emptyHerdrOverview())
		return
	}
	raw, err := s.herdr.snapshot(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Herdr unavailable")
		return
	}
	links := map[string]string{}
	extraRoots := make([]string, 0, len(raw.Panes))
	if data, bridgeErr := s.bridgeRequest(r.Context(), "request_status", "", nil); bridgeErr == nil {
		s.addRegisteredHistoryRoots(data)
		_, links, _ = projectSessions(data)
	}
	for _, pane := range raw.Panes {
		if pane.AgentSession != nil {
			if sessionFile := normalizeSessionPath(pane.AgentSession.Value); sessionFile != "" {
				extraRoots = append(extraRoots, sessionFile)
			}
		}
	}
	s.addHistoryRoots(extraRoots)
	if s.history != nil {
		for sessionFile, sessionID := range s.history.SessionFiles() {
			links[sessionFile] = sessionID
		}
	}
	writeJSON(w, http.StatusOK, projectHerdrSnapshot(raw, links, s.herdr.ownPaneID))
}

func (s *service) handleHerdrPaneOutput(w http.ResponseWriter, r *http.Request) {
	paneID := r.PathValue("paneID")
	if !herdrIDPattern.MatchString(paneID) {
		writeError(w, http.StatusBadRequest, "invalid pane id")
		return
	}
	if s.herdr == nil || !s.herdr.available() {
		writeError(w, http.StatusServiceUnavailable, "Herdr unavailable")
		return
	}
	output, err := s.herdr.readPane(r.Context(), paneID)
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"paneId": paneID, "text": output})
}

func (s *service) handleHerdrPaneFocus(w http.ResponseWriter, r *http.Request) {
	paneID := r.PathValue("paneID")
	if !herdrIDPattern.MatchString(paneID) {
		writeError(w, http.StatusBadRequest, "invalid pane id")
		return
	}
	if s.herdr == nil || !s.herdr.available() {
		writeError(w, http.StatusServiceUnavailable, "Herdr unavailable")
		return
	}
	snapshot, err := s.herdr.snapshot(r.Context())
	if err == nil {
		err = s.herdr.focusAgent(r.Context(), snapshot, paneID)
	}
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *service) handleHerdrPaneInstruction(w http.ResponseWriter, r *http.Request) {
	paneID := r.PathValue("paneID")
	if !herdrIDPattern.MatchString(paneID) {
		writeError(w, http.StatusBadRequest, "invalid pane id")
		return
	}
	if s.herdr == nil || !s.herdr.available() {
		writeError(w, http.StatusServiceUnavailable, "Herdr unavailable")
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		Text string `json:"text"`
	}
	if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.Text) == "" || len(input.Text) > maxInstruction {
		writeError(w, http.StatusBadRequest, "invalid instruction")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid instruction")
		return
	}
	snapshot, err := s.herdr.snapshot(r.Context())
	if err == nil {
		err = s.herdr.promptAgent(r.Context(), snapshot, paneID, input.Text)
	}
	if err != nil {
		writeHerdrError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *service) sessionFiles(ctx context.Context) map[string]string {
	data, err := s.bridgeRequest(ctx, "request_status", "", nil)
	if err != nil {
		return map[string]string{}
	}
	_, links, err := projectSessions(data)
	if err != nil {
		return map[string]string{}
	}
	return links
}

func emptyHerdrOverview() herdrOverview {
	return herdrOverview{Available: false, Workspaces: []herdrWorkspace{}, Tabs: []herdrTab{}, Panes: []herdrPane{}}
}

func writeHerdrError(w http.ResponseWriter, err error) {
	if errors.Is(err, errHerdrInstructionOutcomeUnknown) {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "Herdr instruction outcome unknown; verify before retrying.",
			"code":  "herdr_instruction_uncertain",
		})
		return
	}
	status := http.StatusBadGateway
	message := "Herdr request failed"
	switch err.Error() {
	case "Herdr unavailable":
		status, message = http.StatusServiceUnavailable, "Herdr unavailable"
	case "Herdr request timed out":
		status, message = http.StatusGatewayTimeout, "Herdr request timed out"
	case "invalid pane id":
		status, message = http.StatusBadRequest, "invalid pane id"
	case "pane unavailable", "agent unavailable", "agent blocked; resolve it in Herdr first", "cannot target current Pi pane":
		status, message = http.StatusConflict, err.Error()
	case "instruction must be 1–8000 bytes":
		status, message = http.StatusBadRequest, err.Error()
	}
	writeError(w, status, message)
}

func (s *service) handleEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if !validSessionID(sessionID) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid event cursor")
			return
		}
		after = parsed
	}
	data, err := s.bridgeRequest(r.Context(), "request_events", sessionID, map[string]any{"after": after})
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeRawJSON(w, http.StatusOK, data)
}

func (s *service) handleTranscript(w http.ResponseWriter, r *http.Request) {
	fields := map[string]any{}
	if afterEntryID := r.URL.Query().Get("afterEntryId"); afterEntryID != "" {
		if !validSessionID(afterEntryID) {
			writeError(w, http.StatusBadRequest, "invalid transcript cursor")
			return
		}
		fields["afterEntryId"] = afterEntryID
	}
	s.handleSessionCommand(w, r, "request_transcript", fields)
}

func (s *service) handleTasks(w http.ResponseWriter, r *http.Request) {
	s.handleSessionCommand(w, r, "request_task_state", nil)
}

func (s *service) handleDiff(w http.ResponseWriter, r *http.Request) {
	s.handleSessionCommand(w, r, "request_diff", nil)
}

func (s *service) handleModels(w http.ResponseWriter, r *http.Request) {
	s.handleSessionCommand(w, r, "request_models", nil)
}

func (s *service) handleCodexUsage(w http.ResponseWriter, r *http.Request) {
	s.handleSessionCommand(w, r, "request_codex_usage", nil)
}

func (s *service) handleModel(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if !validSessionID(sessionID) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		Provider string `json:"provider"`
		ModelID  string `json:"modelId"`
	}
	if err := decoder.Decode(&input); err != nil || !validModelRefPart(input.Provider) || !validModelRefPart(input.ModelID) {
		writeError(w, http.StatusBadRequest, "invalid model")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid model")
		return
	}
	data, err := s.bridgeRequest(r.Context(), "switch_model", sessionID, map[string]any{"provider": input.Provider, "modelId": input.ModelID})
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeRawJSON(w, http.StatusAccepted, data)
}

func validModelRefPart(value string) bool {
	if value == "" || len(value) > 120 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func (s *service) handleSessionCommand(w http.ResponseWriter, r *http.Request, command string, fields map[string]any) {
	sessionID := r.PathValue("sessionID")
	if !validSessionID(sessionID) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	data, err := s.bridgeRequest(r.Context(), command, sessionID, fields)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeRawJSON(w, http.StatusOK, data)
}

func (s *service) handleInstruction(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if !validSessionID(sessionID) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		Text string `json:"text"`
	}
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid instruction")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid instruction")
		return
	}
	if strings.TrimSpace(input.Text) == "" || len(input.Text) > maxInstruction {
		writeError(w, http.StatusBadRequest, "instruction must be 1–8000 bytes")
		return
	}
	data, err := s.bridgeRequest(r.Context(), "send_instruction", sessionID, map[string]any{"text": input.Text})
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeRawJSON(w, http.StatusAccepted, data)
}

func (s *service) bridgeRequest(ctx context.Context, command, sessionID string, fields map[string]any) (json.RawMessage, error) {
	allowed := map[string]bool{
		"request_status": true, "request_events": true, "request_task_state": true,
		"request_transcript": true, "request_diff": true, "request_models": true,
		"request_codex_usage": true, "switch_model": true, "send_instruction": true,
	}
	if !allowed[command] {
		return nil, errors.New("command not allowed")
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	frame := map[string]any{"v": bridgeVersion, "op": "request", "id": id, "command": command}
	if sessionID != "" {
		frame["sessionId"] = sessionID
	}
	for key, value := range fields {
		frame[key] = value
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if len(encoded)+1 > maxBridgeLine {
		return nil, errors.New("bridge request too large")
	}

	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", s.bridgePath)
	if err != nil {
		return nil, fmt.Errorf("bridge unavailable: %w", err)
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(12 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return nil, fmt.Errorf("bridge unavailable: %w", err)
	}
	responseLimit := maxBridgeLine
	if command == "request_transcript" {
		responseLimit = maxTranscriptBridgeLine
	} else if command == "request_models" {
		responseLimit = maxModelBridgeLine
	}
	reader := bufio.NewReaderSize(conn, responseLimit)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("bridge unavailable: %w", err)
	}
	if len(line) > responseLimit {
		return nil, errors.New("bridge response too large")
	}
	var reply bridgeReply
	if err := json.Unmarshal(bytesTrimNewline(line), &reply); err != nil || reply.V != bridgeVersion || reply.Op != "response" || subtle.ConstantTimeCompare([]byte(reply.ID), []byte(id)) != 1 {
		return nil, errors.New("invalid bridge response")
	}
	if !reply.OK {
		message := reply.Err
		if message == "" {
			message = "bridge rejected request"
		}
		return nil, bridgeError{message: message}
	}
	if len(reply.Data) == 0 {
		return json.RawMessage("null"), nil
	}
	return reply.Data, nil
}

func bytesTrimNewline(value []byte) []byte {
	return []byte(strings.TrimSuffix(string(value), "\n"))
}

type bridgeError struct{ message string }

func (e bridgeError) Error() string { return e.message }

func writeBridgeError(w http.ResponseWriter, err error) {
	var upstream bridgeError
	if errors.As(err, &upstream) {
		status := http.StatusBadGateway
		if upstream.message == "unknown session" {
			status = http.StatusNotFound
		} else if upstream.message == "session offline" || upstream.message == "session busy" {
			status = http.StatusConflict
		} else if upstream.message == "invalid instruction" || upstream.message == "invalid model" || upstream.message == "model unavailable" {
			status = http.StatusBadRequest
		} else if upstream.message == "unknown command" || upstream.message == "unsupported command" || upstream.message == "not implemented in this milestone" {
			status = http.StatusNotImplemented
		}
		writeError(w, status, upstream.message)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "bridge unavailable")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "response encoding failed")
		return
	}
	writeRawJSON(w, status, encoded)
}

func writeRawJSON(w http.ResponseWriter, status int, value []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func validSessionID(value string) bool { return sessionIDPattern.MatchString(value) }

// liveHub broadcasts only bridge snapshots; each subscriber has a single-slot
// queue so a slow browser cannot backpressure bridge polling.
type liveHub struct {
	mu            sync.Mutex
	subscribers   map[chan []byte]struct{}
	current       []byte
	activity      chan struct{}
	pollingCancel context.CancelFunc
}

func newLiveHub() *liveHub {
	return &liveHub{
		current:     []byte(`{"type":"snapshot","connected":false,"sessions":[]}`),
		subscribers: make(map[chan []byte]struct{}),
		activity:    make(chan struct{}, 1),
	}
}

func (h *liveHub) publish(connected bool, sessions json.RawMessage) {
	if !json.Valid(sessions) {
		sessions = json.RawMessage("[]")
	}
	message, err := json.Marshal(struct {
		Type      string          `json:"type"`
		Connected bool            `json:"connected"`
		Sessions  json.RawMessage `json:"sessions"`
		At        time.Time       `json:"at"`
	}{"snapshot", connected, sessions, time.Now().UTC()})
	if err != nil {
		return
	}
	h.mu.Lock()
	h.current = message
	for subscriber := range h.subscribers {
		select {
		case subscriber <- message:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- message:
			default:
			}
		}
	}
	h.mu.Unlock()
}

func (h *liveHub) notifyActivityLocked() {
	select {
	case h.activity <- struct{}{}:
	default:
	}
}

func (h *liveHub) hasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers) > 0
}

func (h *liveHub) waitForSubscribers(ctx context.Context) bool {
	for {
		if h.hasSubscribers() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-h.activity:
		}
	}
}

func (h *liveHub) setPollingCancel(cancel context.CancelFunc) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscribers) == 0 {
		return false
	}
	h.pollingCancel = cancel
	return true
}

func (h *liveHub) clearPollingCancel() {
	h.mu.Lock()
	h.pollingCancel = nil
	h.mu.Unlock()
}

func (h *liveHub) subscribe() (chan []byte, func()) {
	channel := make(chan []byte, 1)
	h.mu.Lock()
	wasIdle := len(h.subscribers) == 0
	h.subscribers[channel] = struct{}{}
	channel <- append([]byte(nil), h.current...)
	if wasIdle {
		h.notifyActivityLocked()
	}
	h.mu.Unlock()
	return channel, func() {
		var cancel context.CancelFunc
		h.mu.Lock()
		if _, ok := h.subscribers[channel]; ok {
			delete(h.subscribers, channel)
			if len(h.subscribers) == 0 {
				cancel = h.pollingCancel
				h.pollingCancel = nil
				h.notifyActivityLocked()
			}
		}
		h.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

func (s *service) pollBridge(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	for {
		if !s.hub.waitForSubscribers(ctx) {
			return
		}
		pollCtx, cancel := context.WithCancel(ctx)
		if !s.hub.setPollingCancel(cancel) {
			cancel()
			continue
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.pollBridgeWhileActive(pollCtx, interval)
		}()

	active:
		for {
			select {
			case <-ctx.Done():
				cancel()
				<-done
				s.hub.clearPollingCancel()
				return
			case <-s.hub.activity:
				if !s.hub.hasSubscribers() || pollCtx.Err() != nil {
					cancel()
					<-done
					s.hub.clearPollingCancel()
					break active
				}
			}
		}
	}
}

func (s *service) pollBridgeWhileActive(ctx context.Context, interval time.Duration) {
	poll := func() {
		if ctx.Err() != nil || !s.hub.hasSubscribers() {
			return
		}
		data, err := s.bridgeRequest(ctx, "request_status", "", nil)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.hub.publish(false, json.RawMessage("[]"))
			return
		}
		public, _, err := projectSessions(data)
		if err != nil {
			s.hub.publish(false, json.RawMessage("[]"))
			return
		}
		s.hub.publish(true, public)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

func (s *service) handleLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	updates, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case message := <-updates:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", message); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func defaultBridgePath() string {
	if configured := os.Getenv("PI_HARNESS_SOCKET"); configured != "" {
		return configured
	}
	uid := strconv.Itoa(os.Getuid())
	return path.Join("/tmp", "pi-harness-"+uid, "bridge.sock")
}

func parseHostAllowlist(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		host := normalizeHost(item)
		if host != "" {
			result[host] = struct{}{}
		}
	}
	return result
}

func parseOriginAllowlist(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		origin := strings.TrimSpace(item)
		parsed, err := url.Parse(origin)
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.User == nil {
			result[origin] = struct{}{}
		}
	}
	return result
}

func listenAddress() (string, error) {
	host := strings.TrimSpace(os.Getenv("PI_REMOTE_HOST"))
	if host == "" {
		host = "127.0.0.1"
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("PI_REMOTE_HOST must be localhost or a loopback IP; use a trusted local HTTPS/VPN proxy for remote access")
		}
	}
	port := strings.TrimSpace(os.Getenv("PI_REMOTE_PORT"))
	if port == "" {
		port = "8765"
	}
	parsed, err := strconv.Atoi(port)
	if err != nil || parsed < 1 || parsed > 65535 {
		return "", errors.New("PI_REMOTE_PORT must be between 1 and 65535")
	}
	return net.JoinHostPort(host, port), nil
}

func envConfig() config {
	return config{
		allowedHosts:  parseHostAllowlist(os.Getenv("PI_REMOTE_ALLOWED_HOSTS")),
		allowedOrigin: parseOriginAllowlist(os.Getenv("PI_REMOTE_ALLOWED_ORIGINS")),
	}
}
