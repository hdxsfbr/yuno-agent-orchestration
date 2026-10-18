package seed

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// The sentences of DESIGN 5.24 rules 9 and 12 and 5.25 rule 8, verbatim.
const (
	askOperatorSentence = `Ask the operator with ask_operator only when a missing fact decides the work; otherwise decide and state the assumption.`
	writerOutSentence   = `Put the finished report under runs/<run id>/out/ so the operator receives it.`
	chiefAnswerSentence = `When a platform message says a run asks a question, put the question and its options to the person in your own ` +
		`words; when they answer, call answer_question with the question id from that message and their answer as they wrote it, ` +
		`then confirm. Never answer a run's question yourself.`
	templateOutSentence = `Put the finished report under runs/$RUN_ID/out/ so the operator receives it.`
)

// T5.24.14 and T5.25.8 (seed): the Coder and the Researcher hold ask_operator and end their
// prompts with the sentence about it; the Writer's prompt ends with the sentence about its
// report; the Chief answers a run's question with answer_question and never asks one; the
// other agents hold and say nothing of the new tools.
func TestShippedPromptsAndToolsForQuestionsAndDeliverables(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, _, err := Seed(ctx, pool)
	must(t, err)

	endings := map[string]string{"Coder": askOperatorSentence, "Researcher": askOperatorSentence, "Writer": writerOutSentence}
	asks := map[string]bool{"Coder": true, "Researcher": true}
	check := func(when string) {
		t.Helper()
		for _, tpl := range Agents {
			a, err := findAgent(ctx, pool, tpl.Name)
			must(t, err)
			if end, ok := endings[tpl.Name]; ok && !strings.HasSuffix(a.SystemPrompt, end) {
				t.Errorf("%s: the %s prompt does not end with %q:\n%s", when, tpl.Name, end, a.SystemPrompt)
			}
			if has := slices.Contains(a.ToolAllowlist, tools.AskOperatorName); has != asks[tpl.Name] {
				t.Errorf("%s: %s holds ask_operator = %v, want %v (%v)", when, tpl.Name, has, asks[tpl.Name], a.ToolAllowlist)
			}
			if slices.Contains(a.ToolAllowlist, tools.AnswerQuestionName) != (tpl.Name == ChiefName) {
				t.Errorf("%s: %s holds answer_question wrongly (%v)", when, tpl.Name, a.ToolAllowlist)
			}
			if _, ends := endings[tpl.Name]; !ends && tpl.Name != ChiefName &&
				(strings.Contains(a.SystemPrompt, "ask_operator") || strings.Contains(a.SystemPrompt, "/out/")) {
				t.Errorf("%s: the %s prompt names the question or deliverable sentences:\n%s", when, tpl.Name, a.SystemPrompt)
			}
			if tpl.Name == ChiefName && (!strings.Contains(a.SystemPrompt, chiefAnswerSentence) || !slices.Contains(a.ToolAllowlist, tools.AnswerQuestionName) ||
				strings.Contains(a.SystemPrompt, "ask_operator")) {
				t.Errorf("%s: the Chief's prompt or tools are wrong:\n%s\n%v", when, a.SystemPrompt, a.ToolAllowlist)
			}
		}
		// Every holder of ask_operator is a run agent; the chat tools are the Chief's alone.
		chief, err := findAgent(ctx, pool, ChiefName)
		must(t, err)
		if want := slices.Sorted(slices.Values(tools.ChatToolNames)); !slices.Equal(slices.Sorted(slices.Values(chief.ToolAllowlist)), want) {
			t.Errorf("%s: the Chief's tools are %v, want the chat tools %v", when, chief.ToolAllowlist, want)
		}
	}
	check("seeded")

	// The shipped values themselves (what a fresh install gets) end the same way.
	for name, end := range endings {
		var shipped *store.Agent
		for i := range Agents {
			if Agents[i].Name == name {
				shipped = &Agents[i]
			}
		}
		if shipped == nil || !strings.HasSuffix(shipped.SystemPrompt, end) {
			t.Errorf("the shipped %s prompt does not end with %q", name, end)
		}
	}

	// The install from before this wave: the old prompts and allowlists, which
	// `make seed-reset` brings up to date.
	for _, name := range []string{"Coder", "Researcher", "Writer", ChiefName} {
		a, err := findAgent(ctx, pool, name)
		must(t, err)
		a.SystemPrompt = strings.NewReplacer(" "+askOperatorSentence, "", " "+writerOutSentence, "", " "+chiefAnswerSentence, "").Replace(a.SystemPrompt)
		a.ToolAllowlist = slices.DeleteFunc(slices.Clone(a.ToolAllowlist), func(s string) bool {
			return s == tools.AskOperatorName || s == tools.AnswerQuestionName
		})
		must(t, store.UpdateAgent(ctx, pool, a))
		if strings.Contains(a.SystemPrompt, askOperatorSentence) || strings.Contains(a.SystemPrompt, writerOutSentence) || strings.Contains(a.SystemPrompt, chiefAnswerSentence) {
			t.Fatalf("the test did not strip %s's prompt: %s", name, a.SystemPrompt)
		}
	}
	agents, _, err := ResetShipped(ctx, pool, nil)
	must(t, err)
	if !slices.Equal(agents, []string{"Coder", "Researcher", "Writer", ChiefName}) {
		t.Fatalf("reset %v", agents)
	}
	check("after the reset")
	agents, _, err = ResetShipped(ctx, pool, nil)
	must(t, err)
	if len(agents) != 0 {
		t.Fatalf("a second reset changed %v", agents)
	}
}

// T5.25.8 (template): the research pipeline's writer is told where the report goes, with
// the sentence of the Writer's prompt; the other nodes are not.
func TestResearchPipelineWriterPutsTheReportInOut(t *testing.T) {
	raw, err := os.ReadFile("../../templates/research_pipeline.json")
	must(t, err)
	var tpl struct {
		Definition struct {
			Nodes []struct {
				ID   string `json:"id"`
				Task string `json:"task"`
			} `json:"nodes"`
		} `json:"definition"`
	}
	must(t, json.Unmarshal(raw, &tpl))
	seen := map[string]bool{}
	for _, n := range tpl.Definition.Nodes {
		seen[n.ID] = true
		has := strings.Contains(n.Task, templateOutSentence)
		if has != (n.ID == "writer") {
			t.Errorf("node %s: the sentence is present = %v\n%s", n.ID, has, n.Task)
		}
		if n.ID == "writer" {
			// One place for the report: the old path next to the new one would tell the
			// writer two things.
			if strings.Contains(n.Task, "runs/$RUN_ID/report.md") || !strings.Contains(n.Task, "runs/$RUN_ID/out/report.md") {
				t.Errorf("the writer's report path is not under out/:\n%s", n.Task)
			}
		}
	}
	if !seen["writer"] {
		t.Fatal("no writer node")
	}
}
