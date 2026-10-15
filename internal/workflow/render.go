package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"text/template/parse"
)

// printFunc ends every printing action of a task or message template: a missing key or
// a JSON null prints as nothing. text/template prints those as "<no value>", and its
// missingkey=zero option is no substitute over decoded JSON (map[string]any): a missing
// leaf still prints "<no value>", and {{.state.coder.summary}} before the coder ran
// fails with "nil pointer evaluating interface {}.summary".
const printFunc = "_yuno_text"

var renderFuncs = template.FuncMap{printFunc: func(v any) any {
	if v == nil {
		return ""
	}
	return v
}}

// legacyQuickRunTask is the quick-run task of wave 1 snapshots, which predates task
// templates; it means {{.input.task}}.
const legacyQuickRunTask = "{{input.task}}"

// Render executes a task or approval message template over {input, state} (DESIGN 5.6
// rule 5): {{.input.task}}, {{with .state.reviewer}}{{.feedback}}{{end}}. Missing keys
// render empty. Errors name the template position.
func Render(text string, input, state json.RawMessage) (string, error) {
	tmpl, err := ParseTemplate(text)
	if err != nil {
		return "", err
	}
	data := map[string]any{"input": decodeOrEmpty(input), "state": decodeOrEmpty(state)}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ParseTemplate compiles a task or message template with the empty-for-missing
// printing rule.
func ParseTemplate(text string) (*template.Template, error) {
	text = strings.ReplaceAll(text, legacyQuickRunTask, "{{.input.task}}")
	tmpl, err := template.New("task").Funcs(renderFuncs).Parse(text)
	if err != nil {
		return nil, err
	}
	for _, t := range tmpl.Templates() {
		if t.Tree != nil {
			emptyForMissing(t.Tree, t.Tree.Root)
		}
	}
	return tmpl, nil
}

// emptyForMissing appends printFunc to every action that prints.
func emptyForMissing(tree *parse.Tree, n parse.Node) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			emptyForMissing(tree, c)
		}
	case *parse.ActionNode:
		if len(n.Pipe.Decl) == 0 {
			id := parse.NewIdentifier(printFunc).SetTree(tree).SetPos(n.Pos)
			n.Pipe.Cmds = append(n.Pipe.Cmds, &parse.CommandNode{NodeType: parse.NodeCommand, Pos: n.Pos, Args: []parse.Node{id}})
		}
	case *parse.IfNode:
		emptyForMissing(tree, n.List)
		emptyForMissing(tree, n.ElseList)
	case *parse.RangeNode:
		emptyForMissing(tree, n.List)
		emptyForMissing(tree, n.ElseList)
	case *parse.WithNode:
		emptyForMissing(tree, n.List)
		emptyForMissing(tree, n.ElseList)
	}
}

func decodeOrEmpty(raw json.RawMessage) any {
	var v any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			return map[string]any{}
		}
	}
	if v == nil {
		return map[string]any{}
	}
	return v
}

// ValidateTemplates renders every agent task and approval message against the given
// input and an empty state, so a template error is reported before the run (DESIGN 5.6
// rule 5). Messages are path-qualified like ValidateStructure's.
func (d *Definition) ValidateTemplates(input json.RawMessage) []string {
	var msgs []string
	for i, n := range d.Nodes {
		switch n.Kind {
		case KindAgent:
			if _, err := Render(n.Task, input, nil); err != nil {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].task: %v", i, err))
			}
		case KindApproval:
			if _, err := Render(n.Message, input, nil); err != nil {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].message: %v", i, err))
			}
		}
	}
	return msgs
}
