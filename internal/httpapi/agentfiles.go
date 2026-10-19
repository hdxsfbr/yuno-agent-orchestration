package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Limits of the agent file API (DESIGN 5.27 rules 1 to 3).
const (
	agentFileReadMax = 256 << 10 // content returned for one file; more is cut with truncated
	agentFileEditMax = 64 << 10  // an addition or an override
	// agentFileBodyMax bounds the request body: a 64 KiB text can take six bytes per byte
	// as JSON escapes.
	agentFileBodyMax = 6*agentFileEditMax + 4096
)

// agentFile is one workspace file of an agent in the list (DESIGN 5.27 rule 1). Addition
// is present for a generated file and Override for a runtime file; null when none.
type agentFile struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Exists    bool   `json:"exists"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	// Redacted is true when redaction changed the content: the editor must not seed an
	// override from it.
	Redacted  bool            `json:"redacted"`
	Addition  json.RawMessage `json:"addition,omitempty"`
	Override  json.RawMessage `json:"override,omitempty"`
	UpdatedAt *time.Time      `json:"updated_at,omitempty"`
}

func nullable(s *string) json.RawMessage {
	if s == nil {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(*s)
	return b
}

// readWorkspaceFile reads one named file from the workspace through an os.Root, so a
// symlink the agent planted cannot lead outside it. The entry is checked with Lstat before
// it is opened, so a named pipe or device the agent made is never read (opening a FIFO
// blocks): a missing, unreadable or non-regular file is exists=false. The content is
// redacted (the agent's chat token too) before it is cut at agentFileReadMax. When the read
// hit its limit, the tail of the raw buffer is dropped first (back to a line end when there
// is one) so a secret straddling the limit is never half shown. redacted reports that
// redaction changed the content.
func readWorkspaceFile(root *os.Root, name, chatToken string) (content string, exists, truncated, redacted bool) {
	if fi, err := root.Lstat(name); err != nil || !fi.Mode().IsRegular() {
		return "", false, false, false
	}
	f, err := root.Open(name)
	if err != nil {
		return "", false, false, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", false, false, false
	}
	const limit = agentFileReadMax + 1 + 1024
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", false, false, false
	}
	if len(b) >= limit {
		cut := len(b) - 1025
		if i := bytes.LastIndexByte(b[:cut], '\n'); i >= 0 && cut-i <= 1024 {
			cut = i + 1
		}
		b = b[:cut]
		truncated = true
	}
	plain := strings.ToValidUTF8(string(b), "\uFFFD")
	s := plain
	if chatToken != "" {
		s = strings.ReplaceAll(s, chatToken, obs.Redacted)
	}
	s = obs.Redact(s)
	redacted = s != plain
	if len(s) > agentFileReadMax {
		s = s[:agentFileReadMax]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		truncated = true
	}
	return s, true, truncated, redacted
}

// agentFileView builds the list entry for one name.
func agentFileView(root *os.Root, a *store.Agent, name string, edits map[string]store.AgentFileEdit) agentFile {
	v := agentFile{Name: name, Kind: openclaw.FileKind(name)}
	if root != nil {
		v.Content, v.Exists, v.Truncated, v.Redacted = readWorkspaceFile(root, name, a.ChatToken)
	}
	e, has := edits[name]
	if v.Kind == openclaw.FileKindGenerated {
		v.Addition = nullable(e.Addition)
	} else {
		v.Override = nullable(e.Override)
	}
	if has {
		t := e.UpdatedAt.UTC()
		v.UpdatedAt = &t
	}
	return v
}

// openAgentWorkspace opens the agent's workspace; a nil root with no error means the
// directory does not exist yet.
func (s *Server) openAgentWorkspace(a *store.Agent) (*os.Root, error) {
	if a.OpenClawAgentID == nil || *a.OpenClawAgentID == "" {
		return nil, nil
	}
	root, err := os.OpenRoot(s.workspace(*a.OpenClawAgentID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return root, err
}

func notMaterialized(a *store.Agent) bool {
	return a.OpenClawAgentID == nil || *a.OpenClawAgentID == ""
}

func (s *Server) listAgentFiles(w http.ResponseWriter, r *http.Request) {
	a, err := store.GetAgent(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	if notMaterialized(a) {
		writeError(w, http.StatusConflict, "not_materialized", "the agent has no workspace yet", map[string]string{"id": a.ID})
		return
	}
	edits, err := store.GetAgentFileEdits(r.Context(), s.pool, a.ID)
	if err != nil {
		s.internalError(w, r, "agent file edits", err)
		return
	}
	root, err := s.openAgentWorkspace(a)
	if err != nil {
		s.internalError(w, r, "open workspace", err)
		return
	}
	if root != nil {
		defer root.Close()
	}
	out := []agentFile{}
	for _, name := range openclaw.AgentFileNames() {
		out = append(out, agentFileView(root, a, name, edits))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putAgentFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	kind := openclaw.FileKind(name)
	if kind == "" {
		writeError(w, http.StatusBadRequest, "unknown_file", "unknown file "+name, nil)
		return
	}
	a, err := store.GetAgent(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, agentFileBodyMax))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the body is over the 64 KiB limit", nil)
		return
	}
	if !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid_encoding", "the body is not valid UTF-8", nil)
		return
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil || in == nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "the body must be a JSON object", nil)
		return
	}
	want := "addition"
	if kind == openclaw.FileKindRuntime {
		want = "override"
	}
	if len(in) != 1 || in[want] == nil {
		writeError(w, http.StatusBadRequest, "invalid_body", name+" takes exactly {\""+want+"\": ...}", nil)
		return
	}
	var text *string
	if string(in[want]) != "null" {
		var v string
		if err := json.Unmarshal(in[want], &v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", want+" must be a string", nil)
			return
		}
		text = &v
	}
	if want == "addition" && text == nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "addition must be a string (empty removes it)", nil)
		return
	}
	if text != nil && len(*text) > agentFileEditMax {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", want+" is over the 64 KiB limit", nil)
		return
	}
	if notMaterialized(a) {
		writeError(w, http.StatusConflict, "not_materialized", "the agent has no workspace yet", map[string]string{"id": a.ID})
		return
	}
	if want == "addition" {
		err = store.UpsertAgentFileAddition(r.Context(), s.pool, a.ID, name, *text)
	} else {
		err = store.UpsertAgentFileOverride(r.Context(), s.pool, a.ID, name, text)
	}
	if err != nil {
		s.internalError(w, r, "store agent file edit", err)
		return
	}
	// Re-materialise like an agent update; the edit stays stored when this fails and the
	// background sync applies it later (DESIGN 5.27 failure handling).
	saved, ok := s.materializeSaved(w, r, a.ID)
	if !ok {
		return
	}
	edits, err := store.GetAgentFileEdits(r.Context(), s.pool, a.ID)
	if err != nil {
		s.internalError(w, r, "agent file edits", err)
		return
	}
	root, err := s.openAgentWorkspace(saved)
	if err != nil {
		s.internalError(w, r, "open workspace", err)
		return
	}
	if root != nil {
		defer root.Close()
	}
	writeJSON(w, http.StatusOK, agentFileView(root, saved, name, edits))
}
