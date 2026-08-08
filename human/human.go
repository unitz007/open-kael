// Package human describes the person a deployment's agents serve, so
// system prompts can be genuinely personalized instead of only ever saying
// "the owner" — a real name, location, timezone, and freeform notes an LLM
// can reason with. Read once from the environment inside agent.NewAgent
// (see FromEnv) — not a constructor parameter, so every agent in a process
// picks up the same one automatically, and nothing changes for a caller
// that never sets HUMAN_NAME at all.
package human

import "os"

type Human struct {
	Name     string
	Location string
	Timezone string
	Notes    string // anything else worth an agent knowing — preferences, context, etc.
}

// FromEnv builds a Human from HUMAN_NAME/HUMAN_LOCATION/HUMAN_TIMEZONE/
// HUMAN_NOTES. Returns nil if HUMAN_NAME isn't set — the only required
// field, since a Human with a location/timezone/notes but no name isn't
// meaningfully usable in a prompt, and nil is what lets an agent behave
// exactly as if this package didn't exist for anyone not using it.
func FromEnv() *Human {
	name := os.Getenv("HUMAN_NAME")
	if name == "" {
		return nil
	}
	return &Human{
		Name:     name,
		Location: os.Getenv("HUMAN_LOCATION"),
		Timezone: os.Getenv("HUMAN_TIMEZONE"),
		Notes:    os.Getenv("HUMAN_NOTES"),
	}
}
