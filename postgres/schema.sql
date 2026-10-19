-- Schema for the closed-kael domain model. Idempotent (IF NOT EXISTS
-- throughout) so Migrate can run on every startup rather than needing a
-- separate migration framework — there's only one schema version so far.
--
-- One-off cleanups are appended here; they are safe to re-run (DELETE
-- with no matching rows is a no-op).
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
ALTER TABLE users ADD COLUMN IF NOT EXISTS first_name         TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_name          TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified     BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS verification_token TEXT NOT NULL DEFAULT '';

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
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'user';
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS created_by TEXT REFERENCES users(id);
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS source JSONB;

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
ALTER TABLE identities ADD COLUMN IF NOT EXISTS created_by         TEXT REFERENCES users(id);

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

ALTER TABLE messenger_channels ADD COLUMN IF NOT EXISTS onboarding_prompted_at TIMESTAMPTZ;
-- onboarded_at: when the person finished the welcome. The backfill below runs ONLY in the
-- moment the column is created, to treat channels that existed before onboarding was
-- introduced as already welcomed. It must never run again: a person who has not yet been
-- welcomed (or whose welcome was reset) has NULL here on purpose.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'messenger_channels' AND column_name = 'onboarded_at'
    ) THEN
        ALTER TABLE messenger_channels ADD COLUMN onboarded_at TIMESTAMPTZ;
        UPDATE messenger_channels SET onboarded_at = NOW();
    END IF;
END $$;
ALTER TABLE messenger_channels ADD COLUMN IF NOT EXISTS email_link_state TEXT NOT NULL DEFAULT '';
ALTER TABLE messenger_channels ADD COLUMN IF NOT EXISTS sender_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_messenger_channels_identity_sender ON messenger_channels (identity_id, sender_id) WHERE sender_id <> '';

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
    id                       TEXT PRIMARY KEY,
    name                     TEXT NOT NULL,
    function_name            TEXT NOT NULL DEFAULT '',
    description              TEXT NOT NULL DEFAULT '',
    instructions             TEXT NOT NULL DEFAULT '',
    integration_id           TEXT NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    input_schema             JSONB NOT NULL DEFAULT '{}',
    output_schema            JSONB NOT NULL DEFAULT '{}',
    action                   TEXT NOT NULL DEFAULT '',
    requires_approval        BOOLEAN NOT NULL DEFAULT FALSE,
    approval_prompt_template TEXT NOT NULL DEFAULT '',
    approval_timeout_seconds INT NOT NULL DEFAULT 0,
    read_only                BOOLEAN NOT NULL DEFAULT FALSE
);

-- Idempotent migrations for tools columns added after initial schema.
ALTER TABLE tools ADD COLUMN IF NOT EXISTS function_name            TEXT NOT NULL DEFAULT '';
ALTER TABLE tools ADD COLUMN IF NOT EXISTS instructions             TEXT NOT NULL DEFAULT '';
ALTER TABLE tools ADD COLUMN IF NOT EXISTS requires_approval        BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE tools ADD COLUMN IF NOT EXISTS approval_prompt_template TEXT NOT NULL DEFAULT '';
ALTER TABLE tools ADD COLUMN IF NOT EXISTS approval_timeout_seconds INT NOT NULL DEFAULT 0;
ALTER TABLE tools ADD COLUMN IF NOT EXISTS read_only                BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE tools ADD COLUMN IF NOT EXISTS disabled                 BOOLEAN NOT NULL DEFAULT FALSE;

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
ALTER TABLE agents ADD COLUMN IF NOT EXISTS commands     JSONB NOT NULL DEFAULT '[]'::jsonb;
-- rules: what the creator has said about how this agent may use its tools (see domain.ToolRule).
ALTER TABLE agents ADD COLUMN IF NOT EXISTS rules        JSONB NOT NULL DEFAULT '[]'::jsonb;

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
ALTER TABLE skills ADD COLUMN IF NOT EXISTS integrations TEXT[] NOT NULL DEFAULT '{}';

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

-- conversations: one row per unique (agent, identity, chat, thread) tuple.
CREATE TABLE IF NOT EXISTS conversations (
    agent_id    TEXT NOT NULL DEFAULT '',
    identity_id TEXT NOT NULL DEFAULT '',
    chat_id     TEXT NOT NULL DEFAULT '',
    thread_id   TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (agent_id, identity_id, chat_id, thread_id)
);

-- Migration: add composite columns to existing installations that only have
-- the old TEXT PRIMARY KEY column. Both ALTER TABLE statements are no-ops
-- when the columns already exist.
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS agent_id    TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS identity_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS chat_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS thread_id   TEXT NOT NULL DEFAULT '';

-- Drop the legacy single-column TEXT primary key if it exists (CASCADE removes
-- any dependent constraints such as the old PRIMARY KEY on that column).
ALTER TABLE conversations DROP COLUMN IF EXISTS id CASCADE;

DO $$ BEGIN
    ALTER TABLE conversations ADD CONSTRAINT conversations_composite_pk
        PRIMARY KEY (agent_id, identity_id, chat_id, thread_id);
EXCEPTION WHEN invalid_table_definition OR duplicate_table OR duplicate_object THEN NULL;
END $$;

-- Deduplicate rows before creating the unique index; keep the most recent
-- updated_at per (agent_id, identity_id, chat_id, thread_id). This is a
-- no-op when no duplicates exist.
DELETE FROM conversations a USING conversations b
WHERE a.ctid < b.ctid
  AND a.agent_id    = b.agent_id
  AND a.identity_id = b.identity_id
  AND a.chat_id     = b.chat_id
  AND a.thread_id   = b.thread_id;

-- Ensure a unique index exists even on installations where the composite
-- PRIMARY KEY migration above was silently skipped (e.g. the table already
-- had a different primary key). ON CONFLICT (agent_id, ...) in the upsert
-- requires at least one unique or exclusion constraint on those columns.
CREATE UNIQUE INDEX IF NOT EXISTS conversations_composite_uk
    ON conversations (agent_id, identity_id, chat_id, thread_id);

-- conversation_messages: ordered message history for each conversation.
-- BIGSERIAL id gives natural insertion order; the last N messages by id
-- form the sliding window returned by Memory.History.
-- tool_calls is JSONB so ToolCall structs serialize without a join table.
-- content is capped at 4000 chars before storage (large tool results are
-- replaced with a placeholder so the window stays within LLM context limits).
CREATE TABLE IF NOT EXISTS conversation_messages (
    id           BIGSERIAL PRIMARY KEY,
    agent_id     TEXT NOT NULL DEFAULT '',
    identity_id  TEXT NOT NULL DEFAULT '',
    chat_id      TEXT NOT NULL DEFAULT '',
    thread_id    TEXT NOT NULL DEFAULT '',
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    tool_calls   JSONB NOT NULL DEFAULT '[]',
    tool_call_id TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Migration: add composite columns to existing installations.
ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS agent_id    TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS identity_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS chat_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS thread_id   TEXT NOT NULL DEFAULT '';

-- Drop legacy conv_id FK column (references the old conversations.id that no longer exists).
ALTER TABLE conversation_messages DROP COLUMN IF EXISTS conv_id;

-- User-scoped memory: store the resolved user_id on every message so history
-- can be fetched across all channels for a given user (unified across platforms).
ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_conv_messages_composite
    ON conversation_messages (agent_id, identity_id, chat_id, thread_id, id);

CREATE INDEX IF NOT EXISTS idx_conv_messages_user
    ON conversation_messages (agent_id, user_id, id);

-- user_profiles: general facts about a user, learned by the agent over time.
-- Shared across all agents — keyed only by user_id.
CREATE TABLE IF NOT EXISTS user_profiles (
    user_id TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    notes   TEXT NOT NULL DEFAULT ''
);

-- user_integration_notes: integration-specific facts about a user.
-- Shared across all agents that have access to the same integration.
CREATE TABLE IF NOT EXISTS user_integration_notes (
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    integration_id TEXT NOT NULL,
    notes          TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, integration_id)
);

-- user_agent_configs: per-user personal instructions for one Agent. Injected
-- into the system prompt so the agent can personalise responses to each user.
CREATE TABLE IF NOT EXISTS user_agent_configs (
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    agent_id     TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    instructions TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, agent_id)
);

-- platform_config: generic key/value store for platform-level settings that
-- must survive restarts (e.g. rotating OAuth refresh tokens). Keys are
-- short lowercase identifiers like "slack_configurator_refresh_token".
CREATE TABLE IF NOT EXISTS platform_config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

-- user_tool_approvals: per-user approval preferences for tools.
-- Presence of a row means the user wants a human-approval gate on that tool
-- even when ToolDefinition.RequiresApproval is false.
CREATE TABLE IF NOT EXISTS user_tool_approvals (
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    tool_id TEXT NOT NULL REFERENCES tools (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, tool_id)
);

-- user_app_instructions: per-user per-integration instructions injected as
-- additional context when tools from that integration are being used.
CREATE TABLE IF NOT EXISTS user_app_instructions (
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    integration_id TEXT NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    instructions   TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, integration_id)
);

-- email_verifications: short-lived tokens for the in-chat email-linking flow.
-- Created when the user provides their email via the bot; consumed when they
-- click the verify link. IdentityID + channel_ref track where to push the
-- confirmation message once the link is clicked.
CREATE TABLE IF NOT EXISTS email_verifications (
    token       TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    identity_id TEXT NOT NULL DEFAULT '',
    channel_ref TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ NOT NULL
);

-- The person's own time zone (an IANA name such as Africa/Lagos), so agents can speak
-- in their local time. Empty until they say.
ALTER TABLE users ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT '';

-- Where they live and what language to speak to them in. Empty until they say.
ALTER TABLE users ADD COLUMN IF NOT EXISTS language TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS country  TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS city     TEXT NOT NULL DEFAULT '';

-- pending_turns: what a person asked for while something (a connection) stood in the
-- way, kept so it can be carried out once it is sorted. One per person per connection;
-- a newer one replaces the older. Kept in the database so a restart doesn't lose it.
CREATE TABLE IF NOT EXISTS pending_turns (
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    identity_id TEXT NOT NULL,
    agent_id    TEXT NOT NULL,
    conv        JSONB NOT NULL,
    message     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, identity_id)
);

-- The welcome message that carries the "Get started" button, so the button can be taken
-- off once they are done. Empty when there is none.
ALTER TABLE messenger_channels ADD COLUMN IF NOT EXISTS welcome_message_id TEXT NOT NULL DEFAULT '';

-- Cleanup: remove bot-kind identities that are no longer linked to any agent.
-- These accumulate when an agent is deleted (old code left the identity row)
-- or when agent creation fails after the identity was created.
DELETE FROM identities
WHERE kind = 'bot'
  AND id NOT IN (SELECT identity_id FROM agent_identities);

-- Activity: how an agent is doing, for the creator who owns it. Counts only. Nothing here holds
-- what anyone wrote or who they are: a person is a one-way code (an HMAC with a platform secret),
-- kept to count each person once, and removed after 35 days.
ALTER TABLE messenger_channels ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ;
ALTER TABLE messenger_channels ALTER COLUMN created_at SET DEFAULT NOW();

CREATE TABLE IF NOT EXISTS agent_activity_people (
    agent_id TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    day      DATE NOT NULL,
    person   TEXT NOT NULL,
    PRIMARY KEY (agent_id, day, person)
);
CREATE INDEX IF NOT EXISTS idx_agent_activity_people_day ON agent_activity_people (day);

CREATE TABLE IF NOT EXISTS agent_activity_events (
    id       BIGSERIAL PRIMARY KEY,
    agent_id TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    kind     TEXT NOT NULL,
    detail   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_agent_activity_events_agent ON agent_activity_events (agent_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_agent_activity_events_at ON agent_activity_events (at);

-- How many messages an agent handled each minute (a count and nothing else), kept for a day.
CREATE TABLE IF NOT EXISTS agent_activity_minutes (
    agent_id TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    minute   TIMESTAMPTZ NOT NULL,
    messages INT NOT NULL DEFAULT 0,
    PRIMARY KEY (agent_id, minute)
);
