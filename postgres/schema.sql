-- Schema for the closed-kael domain model. Idempotent (IF NOT EXISTS
-- throughout) so Migrate can run on every startup rather than needing a
-- separate migration framework — there's only one schema version so far.
--
-- Recursive Schema-shaped fields (input/output schemas) are stored as
-- JSONB; everything else in the Integration -> Tool -> Skill -> Agent
-- hierarchy is a real, foreign-keyed relational table, since Tools must be
-- reusable across many Skills by reference, not embedded.

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    -- NULL for messenger-provisioned users who haven't set a password yet.
    email         TEXT UNIQUE,
    password_hash TEXT NOT NULL DEFAULT ''
);

-- Idempotent for databases that already had these columns as NOT NULL.
ALTER TABLE users ALTER COLUMN email DROP NOT NULL;
ALTER TABLE users ALTER COLUMN password_hash SET DEFAULT '';

-- Replaced by messenger_channels; kept so existing DBs don't error on startup.
CREATE TABLE IF NOT EXISTS user_channels (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    UNIQUE (provider, channel_id)
);

CREATE TABLE IF NOT EXISTS user_sessions (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);

-- integrations: creator-owned service record; one per external service
-- (GitHub, Slack, etc.). Identities belong to an integration.
CREATE TABLE IF NOT EXISTS integrations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    service     TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT ''
);

-- Idempotent migration: add service for DBs that had the old provider column.
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS service TEXT NOT NULL DEFAULT '';

-- identities: per-integration app/bot credential.
CREATE TABLE IF NOT EXISTS identities (
    id             TEXT PRIMARY KEY,
    integration_id TEXT NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    name           TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL DEFAULT 'bot',
    app_id             TEXT NOT NULL DEFAULT '',
    credential_ref     TEXT NOT NULL DEFAULT '',
    webhook_secret_ref TEXT NOT NULL DEFAULT ''
);

-- Idempotent migrations for older identity schema.
ALTER TABLE identities ADD COLUMN IF NOT EXISTS integration_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE identities ADD COLUMN IF NOT EXISTS kind               TEXT NOT NULL DEFAULT 'bot';
ALTER TABLE identities ADD COLUMN IF NOT EXISTS app_id             TEXT NOT NULL DEFAULT '';
ALTER TABLE identities ADD COLUMN IF NOT EXISTS credential_ref     TEXT NOT NULL DEFAULT '';
ALTER TABLE identities ADD COLUMN IF NOT EXISTS webhook_secret_ref TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_identities_integration_id ON identities (integration_id);

-- app_authorizations: per-user OAuth token tied to an Identity.
CREATE TABLE IF NOT EXISTS app_authorizations (
    id               TEXT PRIMARY KEY,
    identity_id      TEXT NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    user_id          TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scope            TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL DEFAULT '',
    credential_ref   TEXT NOT NULL DEFAULT '',
    external_user_id TEXT NOT NULL DEFAULT '',
    UNIQUE (user_id, identity_id)
);

ALTER TABLE app_authorizations ADD COLUMN IF NOT EXISTS name             TEXT NOT NULL DEFAULT '';
ALTER TABLE app_authorizations ADD COLUMN IF NOT EXISTS external_user_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_app_authorizations_user_id ON app_authorizations (user_id);

-- messenger_channels: per-user conversation channel (replaces user_channels).
CREATE TABLE IF NOT EXISTS messenger_channels (
    id          TEXT PRIMARY KEY,
    identity_id TEXT NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel_ref TEXT NOT NULL DEFAULT '',
    UNIQUE (identity_id, channel_ref)
);

CREATE INDEX IF NOT EXISTS idx_messenger_channels_user_id ON messenger_channels (user_id);

CREATE TABLE IF NOT EXISTS channel_link_codes (
    code        TEXT PRIMARY KEY,
    identity_id TEXT NOT NULL DEFAULT '',
    channel_ref TEXT NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ NOT NULL
);

-- Migrate existing schema: add new columns, drop old user_id.
ALTER TABLE channel_link_codes ADD COLUMN IF NOT EXISTS identity_id TEXT NOT NULL DEFAULT '';
ALTER TABLE channel_link_codes ADD COLUMN IF NOT EXISTS channel_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE channel_link_codes DROP COLUMN IF EXISTS user_id;

CREATE TABLE IF NOT EXISTS tools (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    integration_id TEXT NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    input_schema   JSONB NOT NULL DEFAULT '{}',
    output_schema  JSONB NOT NULL DEFAULT '{}',
    action         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_tools_integration_id ON tools (integration_id);

CREATE TABLE IF NOT EXISTS agents (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    instructions   TEXT NOT NULL DEFAULT '',
    max_iterations INTEGER NOT NULL DEFAULT 0,
    llm_model      TEXT NOT NULL DEFAULT '',
    llm_base_url   TEXT NOT NULL DEFAULT ''
);

ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_model    TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_base_url TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS created_by   TEXT REFERENCES users(id);

CREATE TABLE IF NOT EXISTS skills (
    id             TEXT PRIMARY KEY,
    agent_id       TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    instructions   TEXT NOT NULL DEFAULT '',
    input_schema   JSONB NOT NULL DEFAULT '{}',
    output_schema  JSONB NOT NULL DEFAULT '{}',
    visibility     TEXT NOT NULL DEFAULT 'private',
    -- Trigger is optional on domain.Skill (nil = invoked on demand only) —
    -- both columns NULL together means no Trigger.
    trigger_type   TEXT,
    trigger_value  TEXT
);

CREATE INDEX IF NOT EXISTS idx_skills_agent_id ON skills (agent_id);

ALTER TABLE skills ADD COLUMN IF NOT EXISTS schedulable BOOLEAN NOT NULL DEFAULT false;

-- The many-to-many join between Skill and Tool (ToolBinding) — a Tool is
-- defined once and referenced by any number of Skills. ON DELETE RESTRICT
-- on tool_id: deleting a Tool still bound by a Skill is a real integrity
-- error, not something to silently cascade away.
CREATE TABLE IF NOT EXISTS skill_tools (
    skill_id TEXT NOT NULL REFERENCES skills (id) ON DELETE CASCADE,
    tool_id  TEXT NOT NULL REFERENCES tools (id) ON DELETE RESTRICT,
    PRIMARY KEY (skill_id, tool_id)
);

-- agent_identities: which Identities an Agent is authorized to use.
-- ON DELETE CASCADE on both sides: deleting an agent or identity silently
-- removes the binding.
CREATE TABLE IF NOT EXISTS agent_identities (
    agent_id    TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    identity_id TEXT NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    PRIMARY KEY (agent_id, identity_id)
);

-- conversations: one row per unique memory key (provider:chatID:threadID).
-- The id is the opaque memKey constructed by the runtime.
CREATE TABLE IF NOT EXISTS conversations (
    id         TEXT PRIMARY KEY,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- conversation_messages: ordered message history for each conversation.
-- BIGSERIAL id gives natural insertion order; the last N messages by id
-- form the sliding window returned by Memory.History.
-- tool_calls is JSONB so ToolCall structs serialize without a join table.
-- content is capped at 4000 chars before storage (large tool results are
-- replaced with a placeholder so the window stays within LLM context limits).
CREATE TABLE IF NOT EXISTS conversation_messages (
    id           BIGSERIAL PRIMARY KEY,
    conv_id      TEXT NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    tool_calls   JSONB NOT NULL DEFAULT '[]',
    tool_call_id TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_conv_messages_conv_id ON conversation_messages (conv_id, id);
