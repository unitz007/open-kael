// Package researchspecialist is a delegation-only demo agent: it has a
// single get_secret_code tool and no messenger, so the only way to reach
// it is another agent's delegate_to_research_specialist call — it exists
// to prove agent-to-agent delegation end to end.
package researchspecialist

import (
	"context"
	"encoding/json"

	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/llm/openai"
	"github.com/unitz007/kael/tools"
)

const secretCode = "PLATYPUS-42"

func Agent() *agent.Agent {
	rAgent := agent.NewAgent(
		"research_specialist",
		"Research Specialist Agent",
		"Looks up the secret code on request. Reachable only via delegation from another agent.",
		"You are a research specialist. When asked for the secret code, use your tool to look it up.",
		openai.NewClient("z-ai/glm-5.2", false))

	secretCodeTool := tools.NewToolBuilder("get_secret_code", "Looks up the current secret code").
		Handler(func(ctx context.Context, args json.RawMessage) (interface{}, error) {
			return secretCode, nil
		}).
		Build()
	rAgent.AddTool(secretCodeTool)

	return rAgent
}
