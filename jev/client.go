// Package jev implements runtime.SkillRouter using TypeSafe AI's Jev System
// One model. Jev takes unstructured state and a set of typed questions, and
// returns structured answers (Choice, Score, or Noul) with calibrated
// confidence — no string generation, no hallucination. Call jev.New(apiKey)
// and pass the result to host.SetSkillRouter to enable fast, type-safe skill
// routing without an LLM tool-call round-trip.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

const (
	defaultEndpoint = "https://api.typesafe.ai/v1/systemone"
	defaultModel    = "jev-latest"
)

// Client is a Jev API client. Zero value is not usable — construct with New.
type Client struct {
	apiKey   string
	endpoint string
	http     *http.Client
}

func New(apiKey string) *Client {
	return &Client{apiKey: apiKey, http: &http.Client{}}
}

// Question types.
type (
	// Choice asks Jev to pick one option from a labelled set.
	Choice struct {
		// Instructions describes what the choice represents.
		Instructions string `json:"instructions"`
		// Criteria maps option keys to their human-readable descriptions.
		Criteria map[string]string `json:"criteria"`
	}

	// Score asks Jev to score state on an ordered rubric (0 = first item).
	Score struct {
		Instructions string   `json:"instructions"`
		Criteria     []string `json:"criteria"`
	}

	// Noul asks a true/false question; returns a float in [0,1].
	Noul struct {
		Instructions string `json:"instructions"`
	}
)

func (c Choice) questionType() string { return "choice" }
func (c Score) questionType() string  { return "score" }
func (c Noul) questionType() string   { return "noul" }

type question interface {
	questionType() string
}

func marshalQuestion(q question) (json.RawMessage, error) {
	type wrapper struct {
		Type         string            `json:"type"`
		Instructions string            `json:"instructions"`
		Criteria     any               `json:"criteria,omitempty"`
	}
	w := wrapper{Type: q.questionType()}
	switch v := q.(type) {
	case Choice:
		w.Instructions = v.Instructions
		w.Criteria = v.Criteria
	case Score:
		w.Instructions = v.Instructions
		w.Criteria = v.Criteria
	case Noul:
		w.Instructions = v.Instructions
	}
	return json.Marshal(w)
}

// Answer holds the result of one question.
type Answer struct {
	Type          string             `json:"type"`
	// Choice answers.
	Choice        string             `json:"choice,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Score answers.
	Score  float64            `json:"score,omitempty"`
	Legend map[string]string  `json:"legend,omitempty"`
	// Noul answers.
	Noul float64 `json:"noul,omitempty"`
}

// Response is the full Jev API response.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// SystemOne calls Jev with state and a named set of questions.
// questions is a map of question key → one of Choice, Score, or Noul.
func (c *Client) SystemOne(ctx context.Context, state string, questions map[string]question) (*Response, error) {
	qs := make(map[string]json.RawMessage, len(questions))
	for k, q := range questions {
		raw, err := marshalQuestion(q)
		if err != nil {
			return nil, fmt.Errorf("jev: marshal question %q: %w", k, err)
		}
		qs[k] = raw
	}

	body := map[string]any{
		"state":     state,
		"model":     defaultModel,
		"questions": qs,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("jev: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep(), bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("jev: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jev: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jev: status %d: %s", resp.StatusCode, body)
	}

	var result Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("jev: decode response: %w", err)
	}
	return &result, nil
}

const unknownSkill = "unknown"

// PickSkill implements runtime.JevRouter. It builds a Choice question from
// the agent's current skill list and calls Jev to pick the best match.
func (c *Client) PickSkill(ctx context.Context, userText string, skills []*domain.Skill) (string, float64, error) {
	criteria := make(map[string]string, len(skills)+1)
	for _, s := range skills {
		criteria[s.Name] = s.Description
	}
	criteria[unknownSkill] = "The request cannot be handled by any of the available skills"

	resp, err := c.SystemOne(ctx, userText, map[string]question{
		"skill": Choice{
			Instructions: "Which skill should handle this user request?",
			Criteria:     criteria,
		},
	})
	if err != nil {
		return "", 0, err
	}

	answer, ok := resp.Answers["skill"]
	if !ok {
		return "", 0, fmt.Errorf("jev: no 'skill' answer in response")
	}
	if answer.Choice == unknownSkill {
		return "", answer.Confidence, nil
	}
	return answer.Choice, answer.Confidence, nil
}

func (c *Client) ep() string {
	if c.endpoint != "" {
		return c.endpoint
	}
	return defaultEndpoint
}
