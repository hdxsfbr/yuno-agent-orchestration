package seed

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"yuno/internal/config"
	"yuno/internal/httpapi"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// The sentences of DESIGN 5.22 rule 10 and 5.23 rule 10, verbatim.
const (
	coderMergeRequestSentence = `When your task's protocol block names a merge request this run updates, call read_merge_request first ` +
		`and address every unresolved thread before anything else; say in your summary which threads you addressed and which you could not.`
	reviewerMergeRequestSentence = `When the run updates a merge request, check with read_merge_request that every unresolved thread is ` +
		`addressed by the change.`
	deployerMergeRequestSentence = `When the run updates a merge request, open_merge_request pushes your colleagues' work onto that ` +
		`request's branch instead of opening a new one; finish with status "merge_request_updated" and the URL it returned. When its answer ` +
		`carries replayed without updated, nothing new was pushed: say so in your status.`
	chiefMergeRequestSentence = `When the person asks to change, fix or continue an existing merge request (they say "on !12", "#12", or ` +
		`paste its link), pass it as merge_request to delegate_task with the project it belongs to, so the run works on that request's ` +
		`branch instead of opening a new one; read_merge_request tells you what a request says and which review threads are open.`
	chiefIntakeSentence = `Runs can also start from labelled issues on a project (the intake); run_status tells you the issue a run came from.`
)

// T5.22.12: the seeded agents carry the prompt sentences of the rules and the allowlists
// (read_merge_request for the Coder and the Reviewer, every chat tool for the Chief),
// and an install that has the old ones gets them from the reset (make seed-reset).
func TestShippedPromptsAndToolsForIteratingOnARequest(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, _, err := Seed(ctx, pool)
	must(t, err)

	sentences := map[string][]string{
		"Coder":      {coderMergeRequestSentence},
		"Reviewer":   {reviewerMergeRequestSentence},
		"Deployer":   {deployerMergeRequestSentence},
		ChiefName:    {chiefMergeRequestSentence, chiefIntakeSentence},
		"Researcher": nil,
		"Critic":     nil,
		"Writer":     nil,
	}
	holders := map[string]bool{"Coder": true, "Reviewer": true, ChiefName: true}
	check := func(when string) {
		t.Helper()
		for name, want := range sentences {
			a, err := findAgent(ctx, pool, name)
			must(t, err)
			for _, sentence := range want {
				if !strings.Contains(a.SystemPrompt, sentence) {
					t.Errorf("%s: the %s prompt lacks %q", when, name, sentence)
				}
			}
			if len(want) == 0 && (strings.Contains(a.SystemPrompt, "read_merge_request") || strings.Contains(a.SystemPrompt, "merge_request_updated")) {
				t.Errorf("%s: the %s prompt names the merge request tools", when, name)
			}
			if has := slices.Contains(a.ToolAllowlist, tools.ReadMergeRequestName); has != holders[name] {
				t.Errorf("%s: %s holds read_merge_request = %v, want %v (%v)", when, name, has, holders[name], a.ToolAllowlist)
			}
		}
		chief, err := findAgent(ctx, pool, ChiefName)
		must(t, err)
		if want := slices.Sorted(slices.Values(tools.ChatToolNames)); !slices.Equal(slices.Sorted(slices.Values(chief.ToolAllowlist)), want) {
			t.Errorf("%s: the Chief's tools are %v, want the chat tools %v", when, chief.ToolAllowlist, want)
		}
	}
	check("seeded")

	// The install from before this wave: the old prompts and allowlists.
	for _, name := range []string{"Coder", "Reviewer", "Deployer", ChiefName} {
		a, err := findAgent(ctx, pool, name)
		must(t, err)
		a.SystemPrompt = strings.NewReplacer(coderMergeRequestSentence, "", reviewerMergeRequestSentence, "", deployerMergeRequestSentence, "",
			chiefMergeRequestSentence, "", chiefIntakeSentence, "").Replace(a.SystemPrompt)
		a.ToolAllowlist = slices.DeleteFunc(slices.Clone(a.ToolAllowlist), func(s string) bool { return s == tools.ReadMergeRequestName })
		must(t, store.UpdateAgent(ctx, pool, a))
	}
	agents, _, err := ResetShipped(ctx, pool, nil)
	must(t, err)
	if !slices.Equal(agents, []string{"Coder", "Reviewer", "Deployer", ChiefName}) {
		t.Fatalf("reset %v", agents)
	}
	check("after the reset")
	agents, _, err = ResetShipped(ctx, pool, nil)
	must(t, err)
	if len(agents) != 0 {
		t.Fatalf("a second reset changed %v", agents)
	}
}

// T5.22.12: the Dev pipeline's deployer task says "open or update a merge request" and
// names both statuses, its description mentions updating, and the reset carries the
// definition to an install that has the old one.
func TestDevPipelineNamesBothMergeRequestStatuses(t *testing.T) {
	var tpl struct {
		Description string `json:"description"`
		Definition  struct {
			Nodes []struct {
				ID   string `json:"id"`
				Task string `json:"task"`
			} `json:"nodes"`
		} `json:"definition"`
	}
	raw, err := os.ReadFile("../../templates/dev_pipeline.json")
	must(t, err)
	must(t, json.Unmarshal(raw, &tpl))
	if !strings.Contains(tpl.Description, "updates the one the run names") {
		t.Fatalf("the description does not mention updating: %s", tpl.Description)
	}
	task := func(def json.RawMessage) string {
		t.Helper()
		var d struct {
			Nodes []struct {
				ID   string `json:"id"`
				Task string `json:"task"`
			} `json:"nodes"`
		}
		must(t, json.Unmarshal(def, &d))
		for _, n := range d.Nodes {
			if n.ID == "deployer" {
				return n.Task
			}
		}
		t.Fatal("no deployer node")
		return ""
	}
	deployer := ""
	for _, n := range tpl.Definition.Nodes {
		if n.ID == "deployer" {
			deployer = n.Task
		}
	}
	for _, want := range []string{"open or update a merge request", `"merge_request_opened"`, `"merge_request_updated"`, "merge_request_url"} {
		if !strings.Contains(deployer, want) {
			t.Errorf("the template's deployer task lacks %q:\n%s", want, deployer)
		}
	}

	// The reset: an instantiated Dev pipeline from before the wave gets the new task.
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err = Seed(ctx, pool)
	must(t, err)
	srv, err := httpapi.New(httpapi.Deps{Cfg: config.Config{AnthropicBaseURL: "http://127.0.0.1:1"}, Pool: pool, Log: log, TemplatesDir: "../../templates"})
	must(t, err)
	must(t, srv.LoadTemplates(ctx))
	sh := httpapi.NewShared(httpapi.Deps{Pool: pool, Log: log})
	_, err = Templates(ctx, pool, sh, log)
	must(t, err)
	devID := mustWorkflowNamed(t, pool, "Dev pipeline")
	dev, err := store.GetWorkflow(ctx, pool, devID)
	must(t, err)
	if got := task(dev.Definition); got != deployer {
		t.Fatalf("the instantiated deployer task = %s", got)
	}
	dev.Definition = json.RawMessage(strings.NewReplacer("open or update a merge request", "open a merge request",
		`\"merge_request_updated\"`, `\"merge_request_opened\"`).Replace(string(dev.Definition)))
	must(t, store.UpdateWorkflow(ctx, pool, dev))
	if strings.Contains(task(dev.Definition), "merge_request_updated") {
		t.Fatal("test setup: the old task still names the updated status")
	}
	_, workflows, err := ResetShipped(ctx, pool, sh)
	must(t, err)
	if !slices.Contains(workflows, "Dev pipeline") {
		t.Fatalf("reset workflows %v", workflows)
	}
	dev, err = store.GetWorkflow(ctx, pool, devID)
	must(t, err)
	if got := task(dev.Definition); !strings.Contains(got, "open or update a merge request") || !strings.Contains(got, "merge_request_updated") {
		t.Fatalf("after the reset the deployer task is %s", got)
	}
}
