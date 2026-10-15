package model

import (
	"encoding/json"
	"sync"
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

// ScriptStore hands the scripted provider the next step for a run token. ok=false
// means unknown token or exhausted script (the provider answers 500 so the turn ends
// with an error the eval records as "script exhausted").
type ScriptStore interface {
	Next(runToken string) (step ScriptStep, ok bool)
	Register(runToken string, steps []ScriptStep)
	Remove(runToken string)
}

// MemoryScriptStore is the in-process ScriptStore used by tests and the eval runner.
type MemoryScriptStore struct {
	mu      sync.Mutex
	scripts map[string][]ScriptStep
	index   map[string]int
}

func NewMemoryScriptStore() *MemoryScriptStore {
	return &MemoryScriptStore{scripts: map[string][]ScriptStep{}, index: map[string]int{}}
}

func (s *MemoryScriptStore) Register(runToken string, steps []ScriptStep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[runToken] = steps
	s.index[runToken] = 0
}

func (s *MemoryScriptStore) Remove(runToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.scripts, runToken)
	delete(s.index, runToken)
}

func (s *MemoryScriptStore) Next(runToken string) (ScriptStep, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps, ok := s.scripts[runToken]
	if !ok {
		return ScriptStep{}, false
	}
	i := s.index[runToken]
	if i >= len(steps) {
		return ScriptStep{}, false
	}
	s.index[runToken] = i + 1
	return steps[i], true
}
