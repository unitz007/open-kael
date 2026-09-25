package domain

// Agent is a live worker: identity/instructions, an LLM (with fallback —
// see NativeLoop's callLLM), a pluggable Loop, and the Skills it owns.
// Unlike the first draft of this type, this is no longer a pure blueprint —
// Agent needs real loop/LLM/delegation behavior, borrowed deliberately from
// kael-platform's agent.Agent mechanics but rebuilt around Skills instead
// of flat Tools (see action.go's BindSkill and delegate.go's AgentDirectory).
// LLMConfig is the storable portion of an agent's LLM configuration.
// Model and BaseURL override the process-wide defaults when set; the API
// key is never stored here — it lives in env or an Identity.CredentialRef.
type LLMConfig struct {
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

type Agent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	Instructions string `json:"instructions"`

	// LLMConfig is the stored LLM preference for this agent — model and
	// endpoint override. The live LLM client (Agent.LLMs) is built from it
	// at boot/session time; it is never itself persisted.
	LLMConfig LLMConfig `json:"llm_config,omitempty"`

	// LLMs is priority-ordered, same fallback intent as kael-platform's
	// Agent.LLMs — NativeLoop's callLLM is what drives the circuit breaker
	// over this list. Not persisted: a concrete LLM is a live client, not
	// storable data.
	LLMs []LLM `json:"-"`

	// Loop is pluggable; nil means "use NewNativeLoop(a.LLMs, a.MaxIterations)"
	// — mirrors kael-platform's Agent.loop/SetLoop nil-fallback pattern.
	Loop Loop `json:"-"`

	Skills []*Skill `json:"skills"`

	// IdentityIDs lists the specific Identity IDs this agent is authorized to
	// use. When a skill's tool belongs to an Integration, the runtime picks
	// the Identity from this list whose IntegrationID matches. Agents with
	// multiple identities for the same service (unusual) must resolve
	// ambiguity at the skill level.
	IdentityIDs []string `json:"identity_ids,omitempty"`

	// Directory resolves OTHER agents' Public Skills this Agent may
	// delegate to. Not persisted; wired at hydration time by whatever
	// process owns the set of live agents (a registry, same role
	// runtime.Runtime.DelegateTargets plays in the old framework).
	Directory AgentDirectory `json:"-"`

	MaxIterations int `json:"max_iterations"`

	// CreatedBy is the ID of the User who created this agent. Empty for
	// legacy/seeded agents that predate multi-tenancy.
	CreatedBy string `json:"created_by,omitempty"`
}
