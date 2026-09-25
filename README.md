# open-kael

A Go framework for building AI agent systems — agent loop, multi-channel messaging, skills with cron/webhook triggers, tool execution, and a REST API with a plugin extension point.

```
github.com/unitz007/open-kael
```

---

## Packages

| Package | What it does |
|---------|-------------|
| `domain` | Core types: Agent, Skill, Integration, Identity, AppAuthorization, MessengerChannel, and the Store/Executor/LLM interfaces everything is wired through |
| `runtime` | Boots and runs agents — turn handling, inbound listener loop, cron scheduler, webhook dispatch, MCP server per agent |
| `api` | HTTP REST server over the domain model; accepts provider plugins via `RoutePlugin` so server.go has no provider imports |
| `postgres` | PostgreSQL implementation of `domain.Store`, with a `Migrate` function and a `LoadAll` bootstrap query |
| `crypto` | AES-GCM credential encryption/decryption; `Resolver` implements `domain.CredentialEncryptor` and `domain.CredentialResolver` |

---

## Core concepts

**Agent** — a named worker with instructions, an LLM config, and a set of Skills. Agents do not own tools directly; they own Skills that bind tools with configuration.

**Skill** — a reusable unit of agent behaviour. Each Skill carries a set of `ToolBinding`s, a `Trigger` (cron expression or webhook), and a visibility flag (`public` skills can be discovered and called by other agents as delegates).

**Integration** — a platform-level service record (GitHub, Slack, Telegram, …). Owned by the platform, seeded at boot, not per-user.

**Identity** — a bot or OAuth credential attached to an Integration. A GitHub App or a Telegram bot is an Identity. Created by creators, not end users.

**AppAuthorization** — a per-user credential grant. When a user connects their GitHub account, that creates an AppAuthorization referencing the Identity. The OAuth flow lives in the application layer, not here.

**Plugin** — a struct one provider package constructs and returns, bundling its Integration/Tools catalog entry, event catalogue, IdentityKind, and Executor factory. A composition root collects `[]domain.Plugin` and registers them in a single pass.

---

## Runtime

Boot from stored state:

```go
import (
    "github.com/unitz007/open-kael/domain"
    "github.com/unitz007/open-kael/postgres"
    "github.com/unitz007/open-kael/runtime"
)

pool, _ := pgxpool.New(ctx, dsn)
store   := postgres.New(pool)

agents, toolsByID, identitiesByID, integrationsByID, _ := store.LoadAll(ctx)

_, host := runtime.Boot(runtime.BootInput{
    Agents:           agents,
    ToolsByID:        toolsByID,
    IdentitiesByID:   identitiesByID,
    IntegrationsByID: integrationsByID,
}, runtime.BootLive{
    Executors: executors,   // domain.ExecutorRegistry
    LLMs:      llms,        // map[agentID][]domain.LLM
    Memory:    agentMemory, // map[agentID]domain.Memory
})

go host.ListenAndServe(ctx)
```

`Host` handles the agent loop, inbound messages, cron triggers, and webhook dispatch. Individual listeners, executors, and LLM clients are provided by the embedding application.

---

## HTTP server

```go
import "github.com/unitz007/open-kael/api"

srv := api.NewServer(store,
    api.WithBearerAuth(os.Getenv("API_AUTH_TOKEN")),
    api.WithCredentialEncryptor(encryptor),
    api.WithRoutePlugin(myOAuthPlugin),
)

http.ListenAndServe(":8080", srv)
```

The server exposes CRUD for all domain types (`/agents`, `/integrations`, `/tools`, `/skills`, `/identities`, `/users`, `/channels`) and delegates provider-specific routes to `RoutePlugin` implementations.

### RoutePlugin

Add OAuth callbacks or any provider-specific routes without modifying the server:

```go
type RoutePlugin interface {
    Mount(mux *http.ServeMux, ctx RouteContext)
}

type RouteContext struct {
    Store       domain.Store
    Encryptor   domain.CredentialEncryptor
    UserAuth    func(http.HandlerFunc) http.HandlerFunc
    CreatorAuth func(http.HandlerFunc) http.HandlerFunc
}
```

```go
type MyOAuthPlugin struct{ store domain.Store }

func (p *MyOAuthPlugin) Mount(mux *http.ServeMux, ctx api.RouteContext) {
    p.store = ctx.Store
    mux.HandleFunc("GET /integrations/myprovider/callback", p.callback)
}
```

Register with `api.WithRoutePlugin(p)`. HTTP helpers `api.WriteJSON`, `api.WriteError`, `api.DecodeJSON`, and the sentinel error `api.ErrEncryptorRequired` are exported for plugin use.

---

## Provider plugin pattern

Each provider package constructs a `domain.Plugin` value:

```go
func Plugin() domain.Plugin {
    return domain.Plugin{
        Service: "myprovider",
        Integration: &domain.Integration{
            ID: "myprovider", Service: "myprovider", Name: "My Provider",
        },
        Tools: []*domain.ToolDefinition{ /* ... */ },
        NewExecutor: func(d domain.PluginDeps) domain.Executor {
            return New(d.Resolver)
        },
    }
}
```

The composition root collects `[]domain.Plugin` and seeds integrations, registers executors, and wires identity kinds in one loop — no per-registry bookkeeping scattered across files.

---

## Credential encryption

```go
import "github.com/unitz007/open-kael/crypto"

key, _ := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_ENCRYPTION_KEY"))
resolver := crypto.NewResolver(key) // implements CredentialEncryptor + CredentialResolver

encrypted, _ := crypto.Encrypt(key, rawToken)
plaintext, _ := crypto.Decrypt(key, encrypted)
```

Generate a key: `openssl rand -base64 32`

---

## Installation

```
go get github.com/unitz007/open-kael@latest
```

Requires Go 1.22+ (uses `http.ServeMux` method+path routing) and PostgreSQL for persistence.
