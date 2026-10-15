package httpapi

import (
	"encoding/json"
	"net/http"

	"yuno/internal/tools"
)

// toolView is the Tool schema: a registry entry plus where it runs.
type toolView struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	InputSchema     json.RawMessage `json:"input_schema,omitempty"`
	Risk            tools.Risk      `json:"risk"`
	Hits            tools.Hits      `json:"hits"`
	TimeoutSeconds  int             `json:"timeout_seconds"`
	MaxAttempts     int             `json:"max_attempts,omitempty"`
	UntrustedOutput bool            `json:"untrusted_output"`
	Source          tools.Source    `json:"source"`
}

func newToolView(t *tools.Tool, src tools.Source) toolView {
	return toolView{
		Name:            t.Name,
		Description:     t.Description,
		InputSchema:     t.InputSchema,
		Risk:            t.Risk,
		Hits:            t.Hits,
		TimeoutSeconds:  t.TimeoutS,
		MaxAttempts:     t.MaxAttempts,
		UntrustedOutput: t.Untrusted,
		Source:          src,
	}
}

// listTools returns the platform tools served over MCP, then the OpenClaw built-ins
// the templates use. Built-ins report timeout_seconds 0: OpenClaw applies its own.
func (s *Server) listTools(w http.ResponseWriter, _ *http.Request) {
	out := []toolView{}
	for _, t := range s.tools.List() {
		out = append(out, newToolView(t, tools.SourcePlatform))
	}
	for _, t := range tools.OpenClawBuiltins() {
		out = append(out, newToolView(&t, tools.SourceOpenClaw))
	}
	writeJSON(w, http.StatusOK, out)
}
