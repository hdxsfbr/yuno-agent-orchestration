package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"yuno/internal/store"
)

// checkRevisePlan refuses, before the run starts, an input whose revise_plan cannot be
// revised (DESIGN 5.29 rule 16): not a plan id, no such plan, another project's plan, a plan
// that is done or cancelled, or one for another feature. publish_tickets makes the same
// checks again when it runs. Input without revise_plan, or a request without a project,
// passes.
func checkRevisePlan(ctx context.Context, q store.Querier, proj *store.Project, input json.RawMessage) ([]string, error) {
	var in struct {
		Feature    string `json:"feature"`
		RevisePlan string `json:"revise_plan"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil, nil
	}
	id := strings.TrimSpace(in.RevisePlan)
	if id == "" || proj == nil {
		return nil, nil
	}
	if _, err := uuid.Parse(id); err != nil {
		return []string{fmt.Sprintf("input.revise_plan: %q is not a plan id", id)}, nil
	}
	plan, err := store.GetPlan(ctx, q, id)
	if errors.Is(err, store.ErrNotFound) {
		return []string{fmt.Sprintf("input.revise_plan: there is no plan %s to revise", id)}, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case plan.ProjectID != proj.ID:
		return []string{fmt.Sprintf("input.revise_plan: plan %s belongs to another project", id)}, nil
	case plan.Status == store.PlanDone || plan.Status == store.PlanCancelled:
		return []string{fmt.Sprintf("input.revise_plan: plan %s is %s and cannot be revised", id, plan.Status)}, nil
	case plan.Feature != strings.TrimSpace(in.Feature):
		return []string{fmt.Sprintf("input.revise_plan: plan %s is for feature %q, not %q", id, plan.Feature, strings.TrimSpace(in.Feature))}, nil
	}
	return nil, nil
}
