// Package human lets a deployment's agents know who they're serving, so
// system prompts can be genuinely personalized instead of only ever saying
// "the owner." The platform defines nothing but the interface and where
// it's wired in (agent.Agent.SetHuman / runtime.LocalRuntime.SetHuman) — how the
// details are sourced or shaped (env vars, a database, a webhook-fed
// cache, free text a user typed once) is entirely up to whoever implements
// Human. Nothing changes for a caller that never sets one at all.
package human

// Human is implemented by whoever wants their details injected into every
// agent's system prompt. Details is called fresh on each prompt build and
// its return value is dropped into the prompt as-is — the agent reads it
// directly, so a plain sentence ("My name is Charles, I live in Wolfsburg,
// I'm 23") works exactly as well as anything more structured.
type Human interface {
	Details() string
}
