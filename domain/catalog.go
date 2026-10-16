package domain

import (
	"errors"
	"fmt"
	"sync"
)

// Catalog is the one live list of what agents can use: integrations, the
// identities they run with, and their tools. Built-in plugins, connections people
// build themselves and tool servers all live in it, and everything that needs to
// look a tool up — turns, scheduled runs, the settings screens, delegation between
// agents — reads it, so none of them can see a different set from the others and
// nothing depends on when the server last restarted.
//
// It is safe for concurrent use. Readers get snapshots they may iterate freely and
// must not modify.
//
// Tools enter through AddSource, which holds every one of them to the tool contract
// (ValidateTool). A tool that breaks the contract is refused there.
type Catalog struct {
	mu           sync.RWMutex
	tools        map[string]*ToolDefinition
	integrations map[string]*Integration
	identities   map[string]*Identity
}

func NewCatalog() *Catalog {
	return &Catalog{
		tools:        map[string]*ToolDefinition{},
		integrations: map[string]*Integration{},
		identities:   map[string]*Identity{},
	}
}

// ToolSource provides one integration with its identities and tools. A built-in
// plugin, a creator's own connection and a tool server are each a ToolSource.
type ToolSource interface {
	// Integration the tools belong to. Its CreatedBy says who owns it ("" for the platform).
	Integration() *Integration
	// Identities the integration runs with (may be empty).
	Identities() []*Identity
	// Tools the integration offers.
	Tools() []*ToolDefinition
}

// StaticSource is a ToolSource over plain values.
type StaticSource struct {
	Integ    *Integration
	Idents   []*Identity
	ToolDefs []*ToolDefinition
}

func (s StaticSource) Integration() *Integration { return s.Integ }
func (s StaticSource) Identities() []*Identity   { return s.Idents }
func (s StaticSource) Tools() []*ToolDefinition  { return s.ToolDefs }

// AddSource registers a source, replacing whatever the catalog held for that
// integration (its tools and identities), as one step: no reader sees half of it.
//
// Every tool is checked against the tool contract. A tool that breaks it is left
// out and reported in the returned error; the tools that keep it are still
// registered, so one badly described tool can't take a whole integration down.
func (c *Catalog) AddSource(src ToolSource) error {
	integ := src.Integration()
	if integ == nil || integ.ID == "" {
		return errors.New("catalog: a source must have an integration with an ID")
	}
	var errs []error
	var keep []*ToolDefinition
	for _, t := range src.Tools() {
		if t == nil {
			continue
		}
		cp := *t
		if cp.IntegrationID == "" {
			cp.IntegrationID = integ.ID
		}
		switch {
		case cp.IntegrationID != integ.ID:
			errs = append(errs, ToolProblem{Tool: cp.ID, Message: fmt.Sprintf("belongs to integration %q, not %q", cp.IntegrationID, integ.ID)})
		default:
			if err := ValidateTool(&cp); err != nil {
				errs = append(errs, err)
				continue
			}
			keep = append(keep, &cp)
		}
	}
	// Names must stay distinct within the integration, or the model can't tell them apart.
	if err := ValidateTools(keep); err != nil {
		errs = append(errs, err)
		keep = dropDuplicates(keep)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for id, t := range c.tools {
		if t.IntegrationID == integ.ID {
			delete(c.tools, id)
		}
	}
	for id, i := range c.identities {
		if i.IntegrationID == integ.ID {
			delete(c.identities, id)
		}
	}
	c.integrations[integ.ID] = integ
	for _, i := range src.Identities() {
		if i != nil {
			c.identities[i.ID] = i
		}
	}
	for _, t := range keep {
		c.tools[t.ID] = t
	}
	return errors.Join(errs...)
}

// dropDuplicates keeps the first tool for each ID and each model name.
func dropDuplicates(tools []*ToolDefinition) []*ToolDefinition {
	ids, names := map[string]bool{}, map[string]bool{}
	var out []*ToolDefinition
	for _, t := range tools {
		if ids[t.ID] || names[t.ModelName()] {
			continue
		}
		ids[t.ID], names[t.ModelName()] = true, true
		out = append(out, t)
	}
	return out
}

// RemoveSource removes an integration with its tools and identities.
func (c *Catalog) RemoveSource(integrationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.integrations, integrationID)
	for id, t := range c.tools {
		if t.IntegrationID == integrationID {
			delete(c.tools, id)
		}
	}
	for id, i := range c.identities {
		if i.IntegrationID == integrationID {
			delete(c.identities, id)
		}
	}
}

// PutIntegration, PutIdentity and PutTool add or replace a single entry without
// the contract check. They exist for incremental changes to something already
// registered (a listener attaching an identity); new tools should arrive through AddSource.
func (c *Catalog) PutIntegration(i *Integration) {
	c.mu.Lock()
	c.integrations[i.ID] = i
	c.mu.Unlock()
}

func (c *Catalog) PutIdentity(i *Identity) {
	c.mu.Lock()
	c.identities[i.ID] = i
	c.mu.Unlock()
}

func (c *Catalog) PutTool(t *ToolDefinition) {
	c.mu.Lock()
	c.tools[t.ID] = t
	c.mu.Unlock()
}

func (c *Catalog) Tool(id string) (*ToolDefinition, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tools[id]
	return t, ok
}

func (c *Catalog) Integration(id string) (*Integration, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	i, ok := c.integrations[id]
	return i, ok
}

func (c *Catalog) Identity(id string) (*Identity, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	i, ok := c.identities[id]
	return i, ok
}

// Tools, Integrations and Identities return snapshots keyed by ID.
func (c *Catalog) Tools() map[string]*ToolDefinition {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*ToolDefinition, len(c.tools))
	for k, v := range c.tools {
		out[k] = v
	}
	return out
}

func (c *Catalog) Integrations() map[string]*Integration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*Integration, len(c.integrations))
	for k, v := range c.integrations {
		out[k] = v
	}
	return out
}

func (c *Catalog) Identities() map[string]*Identity {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*Identity, len(c.identities))
	for k, v := range c.identities {
		out[k] = v
	}
	return out
}

// ToolsOf returns an integration's tools.
func (c *Catalog) ToolsOf(integrationID string) []*ToolDefinition {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*ToolDefinition
	for _, t := range c.tools {
		if t.IntegrationID == integrationID {
			out = append(out, t)
		}
	}
	return out
}

// VisibleTo reports whether a creator may use an integration: the platform's are
// shared, a creator's own belong to them alone. This is the one place that rule lives.
func VisibleTo(i *Integration, owner string) bool {
	return i.CreatedBy == "" || i.CreatedBy == owner
}

// HydrateSkill resolves a skill's tools against the catalog as it is right now.
func (c *Catalog) HydrateSkill(skill *Skill, agent *Agent, executors *ExecutorRegistry) ([]*BoundAction, error) {
	return HydrateSkillTools(skill, agent, c.Tools(), c.Identities(), c.Integrations(), executors)
}
