package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	historyMaxRoots          = 128
	historyMaxWalkEntries    = 40000
	historyMaxFiles          = 1000
	historyMaxSessions       = 500
	historyHeaderLineBytes   = 64 << 10
	historyTailBytes         = 64 << 10
	historyMaxFileBytes      = 32 << 20
	historyMaxLineBytes      = 2 << 20
	historyMaxTreeEntries    = 20000
	historyMaxTranscriptRows = 80
	historyMaxTranscriptSize = 6 << 20
	historyMaxEntrySize      = 2 << 20
	historyMaxContentSize    = (3 << 20) / 2
	historyMaxTextSize       = 512 << 10
	historyMaxToolArgsSize   = 64 << 10
	historyMaxImageSize      = 2 << 20
)

var historyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var historyImageMIMEs = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/gif":  {},
}

// historySession is the public, path-free metadata returned by List.
type historySession struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// historyTranscript is a bounded snapshot or delta in the format consumed by app.js.
type historyTranscript struct {
	Source    string           `json:"source"`
	Messages  []historyMessage `json:"messages"`
	Cursor    string           `json:"cursor"`
	Reset     bool             `json:"reset"`
	More      bool             `json:"more"`
	Truncated bool             `json:"truncated"`
}

type historyMessage struct {
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	At        string         `json:"at"`
	Content   []historyBlock `json:"content"`
	Model     *historyModel  `json:"model,omitempty"`
	Label     string         `json:"label,omitempty"`
	ToolName  string         `json:"toolName,omitempty"`
	IsError   *bool          `json:"isError,omitempty"`
	Command   string         `json:"command,omitempty"`
	ExitCode  *int64         `json:"exitCode,omitempty"`
	Cancelled *bool          `json:"cancelled,omitempty"`
	Truncated *bool          `json:"truncated,omitempty"`
}

type historyModel struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

type historyBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MIMEType  string `json:"mimeType,omitempty"`
	Data      string `json:"data,omitempty"`
	Name      string `json:"name,omitempty"`
	ID        string `json:"id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type historyFile struct {
	path string
	info os.FileInfo
}

type cachedHistoryMetadata struct {
	info     os.FileInfo
	size     int64
	modTime  time.Time
	metadata historySession
}

type sessionHistory struct {
	mu            sync.RWMutex
	roots         []string
	sessions      []historySession
	files         map[string]historyFile
	sessionPaths  map[string]string
	metadataCache map[string]cachedHistoryMetadata
}

// newSessionHistory builds a path-private index over only the supplied roots.
func newSessionHistory(roots []string) *sessionHistory {
	h := &sessionHistory{}
	h.SetRoots(roots)
	return h
}

// SetRoots replaces the complete search scope and rebuilds the index.
func (h *sessionHistory) SetRoots(roots []string) {
	cleaned := make([]string, 0, min(len(roots), historyMaxRoots))
	for _, root := range roots {
		if len(cleaned) == historyMaxRoots {
			break
		}
		if strings.TrimSpace(root) == "" {
			continue
		}
		cleaned = append(cleaned, filepath.Clean(root))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.roots = cleaned
	h.metadataCache = make(map[string]cachedHistoryMetadata)
	h.rebuildLocked()
}

// List returns a refreshed, bounded metadata index. Message bodies are not read here.
func (h *sessionHistory) List() []historySession {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rebuildLocked()
	return append([]historySession(nil), h.sessions...)
}

// SessionFiles returns a copy of the private file-path-to-session-ID index for
// internal linking. Paths are deliberately absent from historySession and are
// never included by List or Transcript.
func (h *sessionHistory) SessionFiles() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rebuildLocked()
	result := make(map[string]string, len(h.sessionPaths))
	for path, id := range h.sessionPaths {
		result[path] = id
	}
	return result
}

func (h *sessionHistory) rebuildLocked() {
	if h.metadataCache == nil {
		h.metadataCache = make(map[string]cachedHistoryMetadata)
	}
	found := make([]historySession, 0)
	files := make(map[string]historyFile)
	sessionPaths := make(map[string]string)
	cachePaths := make(map[string]struct{})
	seen := make(map[string]struct{})
	walked, inspected := 0, 0
	stopWalk := false

	for _, root := range h.roots {
		if stopWalk || inspected >= historyMaxFiles {
			break
		}
		rootInfo, err := os.Lstat(root)
		if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		visit := func(path string, entry fs.DirEntry, walkErr error) error {
			walked++
			if walked > historyMaxWalkEntries {
				stopWalk = true
				return errHistoryWalkLimit
			}
			if walkErr != nil || entry == nil || entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if filepath.Ext(entry.Name()) != ".jsonl" || inspected >= historyMaxFiles {
				return nil
			}
			inspected++
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			cachePaths[path] = struct{}{}
			meta, ok := h.metadataForLocked(path, info)
			if !ok {
				return nil
			}
			sessionPaths[path] = meta.SessionID
			if _, duplicate := seen[meta.SessionID]; duplicate {
				return nil
			}
			seen[meta.SessionID] = struct{}{}
			found = append(found, meta)
			files[meta.SessionID] = historyFile{path: path, info: info}
			sessionPaths[path] = meta.SessionID
			return nil
		}

		if rootInfo.Mode().IsRegular() {
			entry := rootFileEntry{path: root, info: rootInfo}
			if filepath.Ext(root) == ".jsonl" && inspected < historyMaxFiles {
				inspected++
				cachePaths[root] = struct{}{}
				if meta, ok := h.metadataForLocked(entry.path, entry.info); ok {
					sessionPaths[root] = meta.SessionID
					if _, duplicate := seen[meta.SessionID]; !duplicate {
						seen[meta.SessionID] = struct{}{}
						found = append(found, meta)
						files[meta.SessionID] = historyFile{path: root, info: rootInfo}
						sessionPaths[root] = meta.SessionID
					}
				}
			}
			continue
		}

		err = filepath.WalkDir(root, visit)
		if errors.Is(err, errHistoryWalkLimit) {
			stopWalk = true
		}
	}
	for path := range h.metadataCache {
		if _, stillInScope := cachePaths[path]; !stillInScope {
			delete(h.metadataCache, path)
		}
	}

	sort.Slice(found, func(i, j int) bool {
		updatedI, _ := time.Parse(time.RFC3339Nano, found[i].UpdatedAt)
		updatedJ, _ := time.Parse(time.RFC3339Nano, found[j].UpdatedAt)
		if !updatedI.Equal(updatedJ) {
			return updatedI.After(updatedJ)
		}
		createdI, _ := time.Parse(time.RFC3339Nano, found[i].CreatedAt)
		createdJ, _ := time.Parse(time.RFC3339Nano, found[j].CreatedAt)
		if !createdI.Equal(createdJ) {
			return createdI.After(createdJ)
		}
		return found[i].SessionID < found[j].SessionID
	})
	if len(found) > historyMaxSessions {
		found = found[:historyMaxSessions]
	}
	h.sessions = found
	h.files = files
	h.sessionPaths = sessionPaths
}

func (h *sessionHistory) metadataForLocked(path string, info os.FileInfo) (historySession, bool) {
	if cached, ok := h.metadataCache[path]; ok && cached.info != nil && os.SameFile(info, cached.info) && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		return cached.metadata, true
	}
	metadata, ok := readHistoryMetadata(path, info)
	if !ok {
		delete(h.metadataCache, path)
		return historySession{}, false
	}
	h.metadataCache[path] = cachedHistoryMetadata{info: info, size: info.Size(), modTime: info.ModTime(), metadata: metadata}
	return metadata, true
}

type rootFileEntry struct {
	path string
	info os.FileInfo
}

var errHistoryWalkLimit = errors.New("history walk limit")

type sessionHeader struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Version   int    `json:"version"`
}

type historyTailRecord struct {
	Type      string  `json:"type"`
	Timestamp string  `json:"timestamp"`
	Name      *string `json:"name"`
}

func readHistoryMetadata(path string, walkedInfo os.FileInfo) (historySession, bool) {
	file, info, err := openIndexedFile(path, walkedInfo)
	if err != nil {
		return historySession{}, false
	}
	defer file.Close()

	first := make([]byte, historyHeaderLineBytes+1)
	n, readErr := file.ReadAt(first, 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return historySession{}, false
	}
	first = first[:n]
	if newline := bytes.IndexByte(first, '\n'); newline >= 0 {
		first = first[:newline]
	} else if len(first) > historyHeaderLineBytes || info.Size() > int64(historyHeaderLineBytes) {
		return historySession{}, false
	}
	first = bytes.TrimSuffix(first, []byte{'\r'})
	var header sessionHeader
	if len(first) == 0 || json.Unmarshal(first, &header) != nil || header.Type != "session" || !historyIDPattern.MatchString(header.ID) {
		return historySession{}, false
	}

	created := safeISOTime(header.Timestamp)
	if created == "" {
		created = safeISOTimeFromTime(info.ModTime())
	}
	updated := created
	name := ""
	if info.Size() > 0 {
		tailLen := min(info.Size(), int64(historyTailBytes))
		tail := make([]byte, int(tailLen))
		if _, err := file.ReadAt(tail, info.Size()-tailLen); err == nil || errors.Is(err, io.EOF) {
			if info.Size() > tailLen {
				if newline := bytes.IndexByte(tail, '\n'); newline >= 0 {
					tail = tail[newline+1:]
				} else {
					tail = nil // The entire tail is part of one oversized record.
				}
			}
			for _, line := range bytes.Split(tail, []byte{'\n'}) {
				line = bytes.TrimSpace(line)
				if len(line) == 0 || len(line) > historyHeaderLineBytes {
					continue
				}
				var record historyTailRecord
				if json.Unmarshal(line, &record) != nil {
					continue
				}
				if at := safeISOTime(record.Timestamp); at != "" {
					updated = at
				}
				if record.Type == "session_info" && record.Name != nil {
					name = cleanHistoryString(*record.Name, 512)
				}
			}
		}
	}
	cwd := cleanHistoryString(header.CWD, 4096)
	return historySession{SessionID: header.ID, CWD: cwd, Name: name, CreatedAt: created, UpdatedAt: updated}, true
}

func openIndexedFile(path string, expected os.FileInfo) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("not a regular indexed session")
	}
	if expected != nil && !os.SameFile(before, expected) {
		return nil, nil, errors.New("indexed session changed")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, nil, errors.New("indexed session changed")
	}
	return file, opened, nil
}

// Transcript reads and projects only the currently indexed file for id.
func (h *sessionHistory) Transcript(id string) (historyTranscript, error) {
	if !historyIDPattern.MatchString(id) {
		return historyTranscript{}, errors.New("invalid session id")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	indexed, ok := h.files[id]
	if !ok {
		return historyTranscript{}, errors.New("session not found")
	}
	file, info, err := openIndexedFile(indexed.path, indexed.info)
	if err != nil || info.Size() > historyMaxFileBytes {
		return historyTranscript{}, errors.New("session unavailable")
	}
	defer file.Close()

	entries, parseTruncated, err := readHistoryTree(file, info.Size())
	if err != nil || len(entries) == 0 || entries[0].header.ID != id {
		return historyTranscript{}, errors.New("session unavailable")
	}
	active := activeHistoryPath(entries)
	selected := compactedHistoryEntries(active)
	return renderHistoryTranscript(selected, parseTruncated), nil
}

type parsedHistoryEntry struct {
	header    sessionHeader
	id        string
	parentID  string
	timestamp string
	kind      string
	raw       json.RawMessage
	firstKept string
	sequence  int
}

type sessionEntryFields struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	ParentID         json.RawMessage `json:"parentId"`
	Timestamp        string          `json:"timestamp"`
	Message          json.RawMessage `json:"message"`
	FirstKeptEntryID string          `json:"firstKeptEntryId"`
}

func readHistoryTree(file *os.File, size int64) ([]parsedHistoryEntry, bool, error) {
	if size <= 0 || size > historyMaxFileBytes {
		return nil, size > historyMaxFileBytes, errors.New("session size limit")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, historyMaxFileBytes), 64<<10)
	entries := make([]parsedHistoryEntry, 0, 512)
	ids := make(map[string]int)
	lineNumber := 0
	truncated := false
	headerRead := false
	version := 1
	previousID := ""
	for lineNumber < historyMaxTreeEntries+1 {
		line, tooLong, eof, err := readBoundedHistoryLine(reader, historyMaxLineBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			return entries, truncated, err
		}
		if len(line) == 0 && eof {
			break
		}
		lineNumber++
		if tooLong {
			truncated = true
			if eof {
				break
			}
			continue
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if eof {
				break
			}
			continue
		}
		if !headerRead {
			var header sessionHeader
			if json.Unmarshal(line, &header) != nil || header.Type != "session" || !historyIDPattern.MatchString(header.ID) {
				return nil, truncated, errors.New("invalid session header")
			}
			headerRead = true
			if header.Version > 0 {
				version = header.Version
			}
			entries = append(entries, parsedHistoryEntry{header: header})
			if eof {
				break
			}
			continue
		}

		var fields sessionEntryFields
		if json.Unmarshal(line, &fields) == nil && fields.Type != "" && fields.Type != "session" {
			id := fields.ID
			if id == "" && version <= 1 {
				id = "legacy_" + strconv.Itoa(lineNumber)
			}
			if historyIDPattern.MatchString(id) {
				if _, duplicate := ids[id]; !duplicate {
					parent := ""
					if version <= 1 {
						parent = previousID
					} else if len(fields.ParentID) > 0 && !bytes.Equal(bytes.TrimSpace(fields.ParentID), []byte("null")) {
						_ = json.Unmarshal(fields.ParentID, &parent)
					}
					entry := parsedHistoryEntry{
						id: id, parentID: parent, timestamp: fields.Timestamp,
						kind: fields.Type, raw: append(json.RawMessage(nil), line...),
						firstKept: fields.FirstKeptEntryID, sequence: lineNumber,
					}
					ids[id] = len(entries)
					entries = append(entries, entry)
					previousID = id
				}
			}
		}
		if eof {
			break
		}
	}
	if lineNumber > historyMaxTreeEntries {
		truncated = true
	}
	return entries, truncated, nil
}

func readBoundedHistoryLine(reader *bufio.Reader, maxBytes int) ([]byte, bool, bool, error) {
	line := make([]byte, 0, min(maxBytes, 64<<10))
	tooLong := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(fragment) > maxBytes {
				tooLong = true
				line = nil
			} else {
				line = append(line, fragment...)
			}
		}
		if err == nil {
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return line, tooLong, false, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return line, tooLong, true, nil
		}
		return nil, tooLong, false, err
	}
}

func activeHistoryPath(entries []parsedHistoryEntry) []parsedHistoryEntry {
	if len(entries) < 2 {
		return nil
	}
	byID := make(map[string]parsedHistoryEntry, len(entries)-1)
	for _, entry := range entries[1:] {
		byID[entry.id] = entry
	}
	leaf := entries[len(entries)-1]
	path := make([]parsedHistoryEntry, 0, len(entries)-1)
	visited := make(map[string]struct{})
	for leaf.id != "" {
		if _, exists := visited[leaf.id]; exists {
			break
		}
		visited[leaf.id] = struct{}{}
		path = append(path, leaf)
		parent, ok := byID[leaf.parentID]
		if !ok || leaf.parentID == "" {
			break
		}
		leaf = parent
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path
}

func compactedHistoryEntries(path []parsedHistoryEntry) []parsedHistoryEntry {
	compactAt := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].kind == "compaction" {
			compactAt = i
			break
		}
	}
	if compactAt < 0 {
		return append([]parsedHistoryEntry(nil), path...)
	}
	compact := path[compactAt]
	selected := make([]parsedHistoryEntry, 0, len(path)-compactAt+8)
	// Pi 0.87.1 builds context with the latest compaction checkpoint first.
	selected = append(selected, compact)
	keptAt := -1
	if compact.firstKept != "" && compact.firstKept != compact.id {
		for i := 0; i < compactAt; i++ {
			if path[i].id == compact.firstKept {
				keptAt = i
				break
			}
		}
	}
	if keptAt >= 0 {
		for _, entry := range path[keptAt:compactAt] {
			// buildContextEntries retains the range but folds system prompts into the checkpoint.
			if entry.kind == "message" && messageRole(entry.raw) == "system" {
				continue
			}
			selected = append(selected, entry)
		}
	}
	selected = append(selected, path[compactAt+1:]...)
	return selected
}

func renderHistoryTranscript(entries []parsedHistoryEntry, parseTruncated bool) historyTranscript {
	// Each call is a fresh bounded snapshot. Cursor support is represented in the
	// response for clients that retain the active leaf between refreshes.
	page, _ := renderHistoryTranscriptFrom(entries, "", parseTruncated)
	return page
}

func renderHistoryTranscriptFrom(entries []parsedHistoryEntry, after string, parseTruncated bool) (historyTranscript, bool) {
	cursorIndex := -1
	if after != "" {
		for i := range entries {
			if entries[i].id == after {
				cursorIndex = i
				break
			}
		}
	}
	reset := after == "" || cursorIndex < 0
	result := historyTranscript{Source: "pi-session", Messages: make([]historyMessage, 0), Reset: reset, Truncated: parseTruncated}
	if len(entries) > 0 {
		result.Cursor = entries[len(entries)-1].id
	}
	if reset {
		used := 0
		for i := len(entries) - 1; i >= 0; i-- {
			message, ok, wasTruncated := transcriptHistoryMessage(entries[i])
			if !ok {
				continue
			}
			encoded, _ := json.Marshal(message)
			if len(result.Messages) >= historyMaxTranscriptRows || used+len(encoded) > historyMaxTranscriptSize-(64<<10) {
				result.Truncated = true
				break
			}
			used += len(encoded)
			result.Messages = append(result.Messages, message)
			if wasTruncated {
				result.Truncated = true
			}
		}
		for left, right := 0, len(result.Messages)-1; left < right; left, right = left+1, right-1 {
			result.Messages[left], result.Messages[right] = result.Messages[right], result.Messages[left]
		}
		return result, result.Truncated
	}

	result.Cursor = after
	used := 0
	for i := cursorIndex + 1; i < len(entries); i++ {
		message, ok, wasTruncated := transcriptHistoryMessage(entries[i])
		if ok {
			encoded, _ := json.Marshal(message)
			if len(result.Messages) >= historyMaxTranscriptRows || used+len(encoded) > historyMaxTranscriptSize-(64<<10) {
				result.More = true
				break
			}
			used += len(encoded)
			result.Messages = append(result.Messages, message)
			if wasTruncated {
				result.Truncated = true
			}
		}
		result.Cursor = entries[i].id
	}
	return result, result.Truncated
}

func messageRole(raw json.RawMessage) string {
	var message struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(extractRawField(raw, "message"), &message) != nil {
		return ""
	}
	return message.Role
}

func transcriptHistoryMessage(entry parsedHistoryEntry) (historyMessage, bool, bool) {
	if entry.kind == "custom_message" {
		var custom struct {
			Display    *bool           `json:"display"`
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
		}
		if json.Unmarshal(entry.raw, &custom) != nil || custom.Display == nil || !*custom.Display {
			return historyMessage{}, false, false
		}
		blocks, truncated := normalizeHistoryContent(custom.Content, true)
		message := historyMessage{ID: entry.id, Role: "custom", At: entryTimestamp(entry), Content: blocks}
		message.Label = cleanHistoryString(custom.CustomType, 120)
		return fitHistoryMessage(message, truncated)
	}
	if entry.kind != "message" {
		return historyMessage{}, false, false
	}
	var stored struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Timestamp  json.RawMessage `json:"timestamp"`
		Provider   string          `json:"provider"`
		Model      string          `json:"model"`
		ToolName   string          `json:"toolName"`
		IsError    *bool           `json:"isError"`
		Command    string          `json:"command"`
		Output     string          `json:"output"`
		ExitCode   *int64          `json:"exitCode"`
		Cancelled  *bool           `json:"cancelled"`
		Truncated  *bool           `json:"truncated"`
		Display    *bool           `json:"display"`
		CustomType string          `json:"customType"`
	}
	if json.Unmarshal(extractRawField(entry.raw, "message"), &stored) != nil {
		return historyMessage{}, false, false
	}
	role := stored.Role
	if role == "hookMessage" { // Legacy v1 name, renamed to custom in v3.
		role = "custom"
	}
	if role == "custom" && (stored.Display == nil || !*stored.Display) {
		return historyMessage{}, false, false
	}
	if role != "user" && role != "assistant" && role != "toolResult" && role != "bashExecution" && role != "custom" {
		return historyMessage{}, false, false
	}
	content := stored.Content
	message := historyMessage{ID: entry.id, Role: role, At: entryTimestamp(entry)}
	if role == "bashExecution" {
		content, _ = json.Marshal([]map[string]string{{"type": "text", "text": stored.Output}})
		message.Command = cleanHistoryString(stored.Command, historyMaxToolArgsSize)
		message.ExitCode = stored.ExitCode
		message.Cancelled = stored.Cancelled
		message.Truncated = stored.Truncated
	} else {
		message.ToolName = cleanHistoryString(stored.ToolName, 120)
		message.IsError = stored.IsError
	}
	if role == "assistant" && stored.Provider != "" && stored.Model != "" {
		provider := cleanHistoryString(stored.Provider, 120)
		model := cleanHistoryString(stored.Model, 120)
		if provider != "" && model != "" {
			message.Model = &historyModel{Provider: provider, ID: model}
		}
	}
	if role == "custom" {
		message.Label = cleanHistoryString(stored.CustomType, 120)
	}
	blocks, contentTruncated := normalizeHistoryContent(content, role == "assistant")
	message.Content = blocks
	wasTruncated := contentTruncated || (stored.Truncated != nil && *stored.Truncated)
	if wasTruncated {
		flag := true
		message.Truncated = &flag
	}
	return fitHistoryMessage(message, wasTruncated)
}

func fitHistoryMessage(message historyMessage, truncated bool) (historyMessage, bool, bool) {
	// normalizeHistoryContent budgets blocks; this final check also covers JSON escaping and metadata.
	encoded, err := json.Marshal(message)
	if err == nil && len(encoded) <= historyMaxEntrySize {
		return message, true, truncated
	}
	truncated = true
	message.Content = []historyBlock{{Type: "text", Text: "[Transcript entry omitted at the size limit]"}}
	flag := true
	message.Truncated = &flag
	encoded, _ = json.Marshal(message)
	if len(encoded) > historyMaxEntrySize {
		return historyMessage{}, false, true
	}
	return message, true, true
}

func normalizeHistoryContent(raw json.RawMessage, allowTools bool) ([]historyBlock, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return make([]historyBlock, 0), false
	}
	var stringContent string
	if json.Unmarshal(raw, &stringContent) == nil {
		block, truncated := boundedTextBlock(stringContent, historyMaxTextSize)
		return []historyBlock{block}, truncated
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return make([]historyBlock, 0), false
	}
	blocks := make([]historyBlock, 0, min(len(values), 64))
	used := 2
	textUsed := 0
	truncated := false
	for _, value := range values {
		if len(blocks) >= 128 {
			truncated = true
			break
		}
		var item struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Data      string          `json:"data"`
			MIMEType  string          `json:"mimeType"`
			Name      string          `json:"name"`
			ID        string          `json:"id"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(value, &item) != nil {
			continue
		}
		var block historyBlock
		switch item.Type {
		case "text":
			remaining := historyMaxTextSize - textUsed
			if remaining <= 0 {
				truncated = true
				continue
			}
			var textTruncated bool
			block, textTruncated = boundedTextBlock(item.Text, remaining)
			truncated = truncated || textTruncated
			textUsed += len(block.Text)
		case "image":
			mimeType := strings.ToLower(item.MIMEType)
			_, mimeOK := historyImageMIMEs[mimeType]
			data := item.Data
			if !mimeOK || len(data) == 0 || len(data) > historyMaxImageSize || len(data)%4 != 0 || !validHistoryBase64(data) {
				block = historyBlock{Type: "text", Text: "[Image omitted: unsupported or too large]"}
				truncated = true
			} else {
				block = historyBlock{Type: "image", MIMEType: mimeType, Data: data}
			}
		case "toolCall":
			if !allowTools || strings.TrimSpace(item.Name) == "" {
				continue
			}
			name := cleanHistoryString(item.Name, 120)
			id := cleanHistoryString(item.ID, 128)
			args := compactHistoryArguments(item.Arguments)
			var argsTruncated bool
			args, argsTruncated = truncateHistoryString(args, historyMaxToolArgsSize)
			block = historyBlock{Type: "toolCall", Name: name, ID: id, Arguments: args}
			truncated = truncated || argsTruncated
		default:
			// Thinking signatures and unknown provider blocks are intentionally not exported.
			continue
		}
		encoded, _ := json.Marshal(block)
		separator := 0
		if len(blocks) > 0 {
			separator = 1
		}
		if used+separator+len(encoded) > historyMaxContentSize {
			truncated = true
			break
		}
		used += separator + len(encoded)
		blocks = append(blocks, block)
	}
	return blocks, truncated
}

func boundedTextBlock(text string, limit int) (historyBlock, bool) {
	cleaned := cleanHistoryString(text, max(limit*2, limit))
	limited, truncated := truncateHistoryString(cleaned, limit)
	return historyBlock{Type: "text", Text: limited}, truncated
}

func validHistoryBase64(value string) bool {
	if len(value)%4 != 0 {
		return false
	}
	_, err := base64.StdEncoding.Strict().DecodeString(value)
	return err == nil
}

func compactHistoryArguments(raw json.RawMessage) string {
	if len(raw) == 0 || !json.Valid(raw) {
		return "{}"
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return "{}"
	}
	return compact.String()
}

func extractRawField(raw json.RawMessage, field string) json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object[field]
}

func entryTimestamp(entry parsedHistoryEntry) string {
	var stored struct {
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if json.Unmarshal(extractRawField(entry.raw, "message"), &stored) == nil {
		if at := safeUnixMillis(stored.Timestamp); at != "" {
			return at
		}
	}
	if at := safeISOTime(entry.timestamp); at != "" {
		return at
	}
	return safeISOTime(entry.header.Timestamp)
}

func safeUnixMillis(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || value != value || value > 8.64e15 || value < -8.64e15 {
		return ""
	}
	millis := int64(value)
	instant := time.UnixMilli(millis).UTC()
	if instant.Year() < 1 || instant.Year() > 9999 {
		return ""
	}
	return instant.Format("2006-01-02T15:04:05.000Z")
}

func safeISOTime(raw string) string {
	if raw == "" || len(raw) > 128 {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.Year() < 1 || t.Year() > 9999 {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func safeISOTimeFromTime(value time.Time) string {
	if value.IsZero() || value.Year() < 1 || value.Year() > 9999 {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func cleanHistoryString(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	var cleaned strings.Builder
	cleaned.Grow(min(len(value), maxBytes))
	for _, r := range value {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
			r = ' '
		}
		if cleaned.Len()+utf8.RuneLen(r) > maxBytes {
			break
		}
		cleaned.WriteRune(r)
	}
	return cleaned.String()
}

func truncateHistoryString(value string, maxBytes int) (string, bool) {
	value = cleanHistoryString(value, maxBytes+4)
	if len(value) <= maxBytes {
		return value, false
	}
	const suffix = "…"
	budget := maxBytes - len(suffix)
	if budget < 0 {
		budget = maxBytes
	}
	var out strings.Builder
	for _, r := range value {
		if out.Len()+utf8.RuneLen(r) > budget {
			break
		}
		out.WriteRune(r)
	}
	if maxBytes >= len(suffix) {
		out.WriteString(suffix)
	}
	return out.String(), true
}
