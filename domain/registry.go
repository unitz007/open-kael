package domain

import (
	"context"
	"fmt"
	"sync"
)

// AgentRegistry is a collection of live Agents (plus the ToolDefinitions/
// Integrations/Executors needed to hydrate their Public Skills) that
// produces per-agent AgentDirectory views. Defined as an interface so the
// in-memory implementation below (InMemoryRegistry) can be swapped for a
// database-backed one later.
//
// Not wired up in the Sports Agent pilot — Sports agent's delegation stays
// on kael-platform's own runtime.Runtime/AgentDirectory. Kept for domain/'s
// own test suite and for a future phase.
type AgentRegistry interface {
	// RegisterAgent adds or replaces an Agent, and sets its Directory
	// field to this registry's view for it — so registering an agent is
	// all that's needed to make it both delegatable (its Public Skills
	// become reachable) and able to delegate (it can reach every other
	// registered agent's Public Skills).
	RegisterAgent(a *Agent)
	// RegisterTool adds or replaces a ToolDefinition — needed to hydrate
	// any Skill that binds it via ToolBinding.
	RegisterTool(t *ToolDefinition)
	// RegisterIntegration adds or replaces an Integration.
	RegisterIntegration(i *Integration)
	// DirectoryFor returns the AgentDirectory view scoped to agentID —
	// what that Agent's own Directory field should be set to
	// (RegisterAgent does this automatically).
	DirectoryFor(agentID string) AgentDirectory
}

// InMemoryRegistry is the default AgentRegistry — an in-memory collection
// of every live Agent, plus the ToolDefinitions/Integrations/Executors
// needed to hydrate their Public Skills into real BoundActions on demand.
//
// DirectoryFor(agentID) returns the per-agent AgentDirectory view: every
// OTHER registered Agent's Public Skills, each wrapped as a depth-limited,
// delegate BoundAction — the same mechanics a hand-wired delegate call
// uses (see delegate.go's doc comment).
type InMemoryRegistry struct {
	mu sync.RWMutex

	agents       map[string]*Agent
	tools        map[string]*ToolDefinition
	integrations map[string]*Integration
	executors    *ExecutorRegistry
}

func NewInMemoryRegistry(executors *ExecutorRegistry) *InMemoryRegistry {
	return &InMemoryRegistry{
		agents:       make(map[string]*Agent),
		tools:        make(map[string]*ToolDefinition),
		integrations: make(map[string]*Integration),
		executors:    executors,
	}
}

var _ AgentRegistry = (*InMemoryRegistry)(nil)

func (r *InMemoryRegistry) RegisterAgent(a *Agent) {
	r.mu.Lock()
	r.agents[a.ID] = a
	r.mu.Unlock()
	a.Directory = r.DirectoryFor(a.ID)
}

func (r *InMemoryRegistry) RegisterTool(t *ToolDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.ID] = t
}

func (r *InMemoryRegistry) RegisterIntegration(i *Integration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.integrations[i.ID] = i
}

// DirectoryFor reads the registry fresh on every PublicSkills call, so
// agents/tools/integrations registered later are picked up without
// re-wiring anything.
func (r *InMemoryRegistry) DirectoryFor(agentID string) AgentDirectory {
	return &inMemoryDirectoryView{registry: r, selfID: agentID}
}

type inMemoryDirectoryView struct {
	registry *InMemoryRegistry
	selfID   string
}

var _ AgentDirectory = (*inMemoryDirectoryView)(nil)

func (v *inMemoryDirectoryView) PublicSkills(_ context.Context) []*BoundAction {
	r := v.registry
	r.mu.RLock()
	defer r.mu.RUnlock()

	var actions []*BoundAction
	for _, agent := range r.agents {
		if agent.ID == v.selfID {
			continue // an agent already has direct access to its own Skills
		}
		for _, skill := range agent.Skills {
			if skill.Visibility != SkillPublic {
				continue
			}
			bound, err := r.bindDelegateSkill(agent, skill)
			if err != nil {
				// A Skill whose Tools/Integration aren't registered yet is
				// skipped rather than failing the whole directory lookup —
				// it becomes reachable as soon as the missing piece is
				// registered.
				continue
			}
			actions = append(actions, bound)
		}
	}
	return actions
}

// bindDelegateSkill hydrates skill's Tools and wraps the result as a
// depth-limited BoundAction, using owner's own LLMs/MaxIterations to run
// its nested loop — identical mechanics to a hand-wired delegate call (see
// delegate.go and loop_test.go's TestDelegation_CallsTargetAgentsOwnLoop).
func (r *InMemoryRegistry) bindDelegateSkill(owner *Agent, skill *Skill) (*BoundAction, error) {
	tools, err := HydrateSkillTools(skill, r.tools, r.integrations, r.executors)
	if err != nil {
		return nil, err
	}
	bound := BindSkill(owner, skill, tools)

	return &BoundAction{
		Spec: bound.Spec,
		Invoke: func(ctx context.Context, input map[string]any) (any, error) {
			ctx, depth, ok := IncrementDelegationDepth(ctx)
			if !ok {
				return nil, fmt.Errorf("delegate %q: depth %d exceeds max delegation depth", skill.Name, depth)
			}
			return bound.Invoke(ctx, input)
		},
	}, nil
}
