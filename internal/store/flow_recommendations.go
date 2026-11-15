package store

// FlowModelRecommendation is a model suggestion for one role in one flow and
// harness.
type FlowModelRecommendation struct {
	Model string `json:"model"`
	Why   string `json:"why"`
}

type FlowModelRecommendationRow struct {
	RoleKey string                    `json:"role_key"`
	FlowKey string                    `json:"flow_key"`
	Tool    string                    `json:"tool"`
	Models  []FlowModelRecommendation `json:"models"`
}

// These are requested model IDs, including models that may not yet be in a
// particular machine's live catalog. The endpoint filters by that catalog
// when a machine and launcher are supplied.
var flowRecommendationModels = map[string]map[string]string{
	"claude": {"strong": "claude-opus-5-5", "fast": "claude-sonnet-5-5", "top": "claude-fable-5-2"},
	"codex":  {"strong": "gpt-6-sol", "fast": "gpt-6-luna", "top": "gpt-6-astra"},
}

var flowRecommendationWhy = map[string]string{
	"strong": "Decides or reviews: needs the most judgment.",
	"fast":   "Executes or routes: speed and cost matter.",
	"top":    "The strongest model for rare, high-stakes advice.",
}

type flowRecommendationRule struct {
	flow, role string
	tiers      []string
}

var flowRecommendationRules = []flowRecommendationRule{
	{"leader-workers", "leader", []string{"strong"}},
	{"leader-workers", "worker", []string{"fast"}},
	{"leader-workers", "reviewer", []string{"strong"}},
	{"leader-dispatcher", "leader", []string{"strong"}},
	{"leader-dispatcher", "dispatcher", []string{"fast"}},
	{"leader-dispatcher", "worker", []string{"fast"}},
	{"leader-dispatcher", "reviewer", []string{"strong"}},
	{"leader-advisor", "leader", []string{"fast"}},
	{"leader-advisor", "advisor", []string{"strong", "top"}},
	{"leader-advisor", "worker", []string{"fast"}},
	{"leader-advisor", "reviewer", []string{"strong"}},
}

func TeamFlowRecommendedModels() []FlowModelRecommendationRow {
	rows := make([]FlowModelRecommendationRow, 0, len(flowRecommendationRules)*len(flowRecommendationModels))
	for _, rule := range flowRecommendationRules {
		for _, tool := range []string{"claude", "codex"} {
			row := FlowModelRecommendationRow{RoleKey: rule.role, FlowKey: rule.flow, Tool: tool, Models: []FlowModelRecommendation{}}
			for _, tier := range rule.tiers {
				model := flowRecommendationModels[tool][tier]
				if model == "" {
					continue
				}
				why := flowRecommendationWhy[tier]
				if rule.role == "reviewer" {
					why += " Prefer a different vendor than the worker."
				}
				row.Models = append(row.Models, FlowModelRecommendation{Model: model, Why: why})
			}
			if len(row.Models) > 0 {
				rows = append(rows, row)
			}
		}
	}
	return rows
}
