package domain

// Agent is a live worker: identity/instructions, an LLM (with fallback —
// see NativeLoop's callLLM), a pluggable Loop, and the Skills it owns.
// Deliberately rebuilt around Skills instead of flat Tools — Agent has no
// Tools field at all; a Tool only ever reaches an Agent's action list by
// way of a Skill (see action.go's BindSkill and delegate.go's
// AgentDirectory).
//
// In this pilot, domain.Agent is used purely as a stored record (ID/Name/
// Description/Instructions/Skills) for the Sports Agent's Skills — it is
// never instantiated as a live runtime object. Sports agent's actual
// execution stays on kael-platform's existing agent.Agent/nativeLoop/
// llm.LLM; see Kael/agents/domainbridge for the translation layer.
type Agent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	Instructions string `json:"instructions"`

	// LLMs is priority-ordered, same fallback intent as kael-platform's
	// Agent.LLMs — NativeLoop's callLLM is what drives the circuit breaker
	// over this list. Not persisted: a concrete LLM is a live client, not
	// storable data.
	LLMs []LLM `json:"-"`

	// Loop is pluggable; nil means "use NewNativeLoop(a.LLMs, a.MaxIterations)"
	// — mirrors kael-platform's Agent.loop/SetLoop nil-fallback pattern.
	Loop Loop `json:"-"`

	Skills []*Skill `json:"skills"`

	// Directory resolves OTHER agents' Public Skills this Agent may
	// delegate to. Not persisted; wired at hydration time by whatever
	// process owns the set of live agents (e.g. an AgentRegistry).
	Directory AgentDirectory `json:"-"`

	MaxIterations int `json:"max_iterations"`
}
