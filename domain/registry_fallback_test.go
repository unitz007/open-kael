package domain_test

import (
	"context"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func TestExecutorRegistry_FallbackAndPrecedence(t *testing.T) {
	r := domain.NewExecutorRegistry()
	r.Register("known", echoExecutor{})
	r.SetFallback(func(service string) (domain.Executor, bool) {
		if service == "custom_1" {
			return echoExecutor{}, true
		}
		return nil, false
	})

	if _, ok := r.For("known"); !ok {
		t.Fatal("registered executor must resolve")
	}
	if _, ok := r.For("custom_1"); !ok {
		t.Fatal("fallback must resolve a service with no registration")
	}
	if _, ok := r.For("nope"); ok {
		t.Fatal("unknown service must not resolve")
	}
}

func TestExecutorRegistry_ConcurrentUse(t *testing.T) {
	r := domain.NewExecutorRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.Register("svc", echoExecutor{}) }()
		go func() { defer wg.Done(); r.For("svc") }()
	}
	wg.Wait()
}

func TestHydrateSkillTools_SkipsDisabledTools(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	on := &domain.ToolDefinition{ID: "t_on", Name: "on", IntegrationID: "int_1", Action: "a"}
	off := &domain.ToolDefinition{ID: "t_off", Name: "off", IntegrationID: "int_1", Action: "b", Disabled: true}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	skill := &domain.Skill{ID: "s", Integrations: []string{"int_1"}}
	bound, err := domain.HydrateSkillTools(skill, nil,
		map[string]*domain.ToolDefinition{"t_on": on, "t_off": off}, nil,
		map[string]*domain.Integration{"int_1": integration}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(bound) != 1 || bound[0].Spec.Name != "on" {
		t.Fatalf("want only the enabled tool, got %d: %+v", len(bound), bound)
	}

	// An explicit binding to a disabled tool is skipped too.
	skill = &domain.Skill{ID: "s2", Tools: []domain.ToolBinding{{ToolID: "t_off"}}}
	bound, err = domain.HydrateSkillTools(skill, nil,
		map[string]*domain.ToolDefinition{"t_off": off}, nil,
		map[string]*domain.Integration{"int_1": integration}, registry)
	if err != nil || len(bound) != 0 {
		t.Fatalf("disabled explicit binding: bound=%d err=%v", len(bound), err)
	}
}

type countingConnect struct{ calls int }

func (c *countingConnect) RequestConnect(context.Context, domain.ConversationRef, *domain.Identity, string) (string, error) {
	c.calls++
	return "", nil
}

func TestHydrateTool_TokenIdentityNeverAsksUserToConnect(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	tool := &domain.ToolDefinition{ID: "t", Name: "t", IntegrationID: "int_1", Action: "a"}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	for _, tc := range []struct {
		kind      string
		wantAsked int
	}{{domain.IdentityKindToken, 0}, {domain.IdentityKindOAuth, 1}} {
		identity := &domain.Identity{ID: "id_" + tc.kind, IntegrationID: "int_1", Kind: tc.kind}
		bound, err := domain.HydrateTool(tool, identity, integration, registry)
		if err != nil {
			t.Fatal(err)
		}
		req := &countingConnect{}
		ctx := domain.WithConnectRequester(context.Background(), req)
		ctx = domain.WithConversation(ctx, domain.ConversationRef{})
		ctx = domain.WithUserID(ctx, "u1")
		_, _ = bound.Invoke(ctx, map[string]any{})
		if req.calls != tc.wantAsked {
			t.Errorf("kind %s: connect requested %d times, want %d", tc.kind, req.calls, tc.wantAsked)
		}
	}
}
