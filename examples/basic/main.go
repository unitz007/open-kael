// A minimal runnable example: a personal-assistant-style agent that can
// delegate to a research specialist. Requires LLM_API_KEY and LLM_BASE_URL
// (and, to actually reply anywhere, a Messenger — see the platform's
// messaging package) to be set in the environment.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/examples/llm/openai"
	"github.com/unitz007/kael/examples/researchspecialist"
	"github.com/unitz007/kael/runtime"
)

func assistantAgent() *agent.Agent {
	return agent.NewAgent(
		"assistant",
		"Assistant Agent",
		"A general-purpose assistant that can delegate research questions.",
		"You are a helpful assistant.",
		openai.NewClient("z-ai/glm-5.2", false))
}

func main() {
	rt := runtime.NewRuntime()
	rt.RegisterAgent(assistantAgent())
	rt.RegisterAgent(researchspecialist.Agent())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.Println("kael-platform example running — press Ctrl+C to stop.")
	if err := rt.Launch(ctx); err != nil {
		log.Fatal("error launching runtime:", err)
	}
}
