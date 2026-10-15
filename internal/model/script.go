package model

import (
	"encoding/json"
	"sync"
	"time"
)

// ScriptStep is one scripted model response (DESIGN 5.14 rule 9): either text or a
// tool call, with the usage the fake provider reports.
type ScriptStep struct {
	Text  string          `json:"text,omitempty" yaml:"text,omitempty"`
	Tool  *ScriptToolCall `json:"tool,omitempty" yaml:"tool,omitempty"`
	Usage *ScriptUsage    `json:"usage,omitempty" yaml:"usage,omitempty"`
}

type ScriptToolCall struct {
	Name string          `json:"name" yaml:"name"`
	Args json.RawMessage `json:"args" yaml:"args"`
}

type ScriptUsage struct {
	Prompt     int `json:"prompt" yaml:"prompt"`
	Completion int `json:"completion" yaml:"completion"`
}

// ScriptStore hands the scripted provider the next step for a script id (the
// "script_id: <id>" marker in the task message). ok=false means unknown id or
// exhausted script (the provider answers 500 so the turn ends with an error the eval
// records as "script exhausted"). Scripts are registered over the API by the eval
// runner (POST /api/evals/scripts) and by tests directly.
type ScriptStore interface {
	Next(scriptID string) (step ScriptStep, ok bool)
	Register(scriptID string, steps []ScriptStep)
	Remove(scriptID string)
}

// ScriptTTL bounds how long a registered script is kept when it is never used up (an
// eval run that was cancelled or failed before its last step).
const ScriptTTL = time.Hour

// MemoryScriptStore is the in-process ScriptStore used by tests and the eval runner. A
// script is dropped once its last step was served and, at the latest, ScriptTTL after
// it was registered, so repeated eval runs do not grow the api's memory.
type MemoryScriptStore struct {
	mu      sync.Mutex
	scripts map[string]*storedScript
	now     func() time.Time
}

type storedScript struct {
	steps   []ScriptStep
	next    int
	expires time.Time
}

func NewMemoryScriptStore() *MemoryScriptStore {
	return &MemoryScriptStore{scripts: map[string]*storedScript{}, now: time.Now}
}

func (s *MemoryScriptStore) Register(scriptID string, steps []ScriptStep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, sc := range s.scripts {
		if !now.Before(sc.expires) {
			delete(s.scripts, id)
		}
	}
	s.scripts[scriptID] = &storedScript{steps: steps, expires: now.Add(ScriptTTL)}
}

func (s *MemoryScriptStore) Remove(scriptID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.scripts, scriptID)
}

func (s *MemoryScriptStore) Next(scriptID string) (ScriptStep, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.scripts[scriptID]
	if !ok {
		return ScriptStep{}, false
	}
	if !s.now().Before(sc.expires) || sc.next >= len(sc.steps) {
		delete(s.scripts, scriptID)
		return ScriptStep{}, false
	}
	step := sc.steps[sc.next]
	sc.next++
	if sc.next == len(sc.steps) {
		delete(s.scripts, scriptID) // used up: the next request is answered as exhausted
	}
	return step, true
}
