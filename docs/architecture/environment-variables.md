# Environment Variables

None of these are read by this module in general — they're specific to whichever pieces an application actually wires up:

| Variable | Read by |
|----------|---------|
| `LLM_API_KEY`, `LLM_BASE_URL` | `examples/llm/openai.NewClient` — `os.Exit(1)` if either is missing |
| `TELEGRAM_TOKEN`, `TELEGRAM_CHAT_ID` | `examples/messenger`'s `NewTelegramBot` |
| `SLACK_APP_TOKEN`, `SLACK_BOT_TOKEN` | `examples/messenger`'s `NewSlackBot` |

An application adding its own tools/identities (a specific external API, a specific GitHub App) is responsible for its own environment variables and validation — this module has no knowledge of those.
