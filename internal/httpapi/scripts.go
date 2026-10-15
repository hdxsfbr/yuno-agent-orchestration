package httpapi

import (
	"fmt"
	"net/http"
	"regexp"

	"yuno/internal/model"
	"yuno/internal/runtoken"
)

const maxScriptBody = 4 << 20

// scriptIDPattern is ScriptRegistration.script_id in api/openapi.yaml.
const scriptIDPattern = `^[a-zA-Z0-9._-]{1,80}$`

var scriptIDRe = regexp.MustCompile(scriptIDPattern)

// validScriptID accepts ids that match the contract and that the scripted provider can
// read back from a task message ("script_id: <id>"); an id ending in "." or "-" would
// be read back shorter.
func validScriptID(id string) bool {
	return scriptIDRe.MatchString(id) && runtoken.FindScriptID("script_id: "+id+" ") == id
}

type scriptRegistration struct {
	ScriptID string             `json:"script_id"`
	Steps    []model.ScriptStep `json:"steps"`
}

// registerScript stores (or replaces) a script for the scripted provider. The store drops
// it once its last step was served or model.ScriptTTL after registration.
func (s *Server) registerScript(w http.ResponseWriter, r *http.Request) {
	var in scriptRegistration
	if !decodeBody(w, r, &in, maxScriptBody) {
		return
	}
	var fe fieldErrors
	if in.ScriptID == "" {
		fe.add("script_id", "is required")
	} else if !validScriptID(in.ScriptID) {
		fe.add("script_id", "must match %s and not end in '.' or '-'", scriptIDPattern)
	}
	if len(in.Steps) == 0 {
		fe.add("steps", "must contain at least one step")
	}
	for i, st := range in.Steps {
		if err := st.Validate(); err != nil {
			fe.add(fmt.Sprintf("steps[%d]", i), "%s", err.Error())
		}
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid script", fe)
		return
	}
	s.scripts.Register(in.ScriptID, in.Steps)
	s.log.Info("script registered", "script_id", in.ScriptID, "steps", len(in.Steps))
	w.WriteHeader(http.StatusNoContent)
}
