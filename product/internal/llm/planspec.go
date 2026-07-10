package llm

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// jsonBlockRe matches a fenced ```json … ``` block (the planner's structured
// sidecar). Also matches a bare ``` … ``` as a fallback for models that forget
// the language tag.
var (
	jsonTaggedRe = regexp.MustCompile("(?s)```json\\s*(.*?)```")
	anyFenceRe   = regexp.MustCompile("(?s)```\\s*(\\{.*?\\})\\s*```")
)

// ExtractSpec pulls the machine-readable plan out of a plan's fenced json
// block. It returns the parsed spec and the plan with that block removed, so
// the operator-facing markdown stays pure prose. A missing or unparseable
// block yields an empty spec and the plan unchanged — the customer UI then
// simply omits the structured sections rather than breaking.
func ExtractSpec(plan string) (project.PlanSpec, string) {
	tryParse := func(inner string) (project.PlanSpec, bool) {
		var spec project.PlanSpec
		if err := json.Unmarshal([]byte(strings.TrimSpace(inner)), &spec); err == nil && len(spec.Pages) > 0 {
			return spec, true
		}
		return project.PlanSpec{}, false
	}
	// Prefer the last ```json block (the sidecar sits at the end of the plan).
	if ms := jsonTaggedRe.FindAllStringSubmatch(plan, -1); len(ms) > 0 {
		last := ms[len(ms)-1]
		if spec, ok := tryParse(last[1]); ok {
			return spec, strings.TrimSpace(strings.Replace(plan, last[0], "", 1))
		}
	}
	if ms := anyFenceRe.FindAllStringSubmatch(plan, -1); len(ms) > 0 {
		last := ms[len(ms)-1]
		if spec, ok := tryParse(last[1]); ok {
			return spec, strings.TrimSpace(strings.Replace(plan, last[0], "", 1))
		}
	}
	return project.PlanSpec{}, plan
}
