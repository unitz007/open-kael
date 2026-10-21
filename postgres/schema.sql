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

-- persons: a human who talks to agents. Everything about someone being talked to hangs on a
-- person: their messenger accounts (messenger_channels), what agents have learned about them,
-- their app connections, tasks and settings. Created the first time someone writes to a bot.
-- merged_into marks a person who was found to be someone already known (see MergePersons).
CREATE TABLE IF NOT EXISTS persons (
    id          TEXT PRIMARY KEY,
    first_name  TEXT NOT NULL DEFAULT '',
    last_name   TEXT NOT NULL DEFAULT '',
    -- The person's own time zone (an IANA name such as Africa/Lagos), so agents can speak in their
    -- local time. Empty until they say. Likewise where they live and the language to speak to them in.
    timezone    TEXT NOT NULL DEFAULT '',
    language    TEXT NOT NULL DEFAULT '',
    country     TEXT NOT NULL DEFAULT '',
    city        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    merged_into TEXT REFERENCES persons (id) ON DELETE CASCADE
);

-- accounts: a login to the web app, and the owner of what a creator builds (created_by on agents,
-- integrations and identities). person_id is the person a login is for; several logins may point at
-- one person, and a login may have no person yet.
CREATE TABLE IF NOT EXISTS accounts (
    id                 TEXT PRIMARY KEY,
    -- NULL for a login that is only a link between a messenger and the web, until set otherwise.
    email              TEXT UNIQUE,
    password_hash      TEXT NOT NULL DEFAULT '',
    email_verified     BOOLEAN NOT NULL DEFAULT FALSE,
    verification_token TEXT NOT NULL DEFAULT '',
    person_id          TEXT REFERENCES persons (id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_accounts_person_id ON accounts (person_id);

-- Replaced by messenger_channels; kept so existing DBs don't error on startup.
CREATE TABLE IF NOT EXISTS user_channels (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    UNIQUE (provider, channel_id)
);

CREATE TABLE IF NOT EXISTS user_sessions (
    token      TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
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
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS created_by TEXT REFERENCES accounts (id);
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
ALTER TABLE identities ADD COLUMN IF NOT EXISTS created_by         TEXT REFERENCES accounts (id);

CREATE INDEX IF NOT EXISTS idx_identities_integration_id ON identities (integration_id);

-- app_authorizations: per-user OAuth token tied to an Identity.
CREATE TABLE IF NOT EXISTS app_authorizations (
    id               TEXT PRIMARY KEY,
    identity_id      TEXT NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    user_id          TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
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
    user_id     TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
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
ALTER TABLE tools ADD COLUMN IF NOT EXISTS needs_local_time         BOOLEAN NOT NULL DEFAULT FALSE;

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
ALTER TABLE agents ADD COLUMN IF NOT EXISTS created_by   TEXT REFERENCES accounts (id);
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
    -- Not used any more: a capability carries no trigger (what happens when something happens is a reaction a
    -- person sets up). Left in place so old rows and rollbacks are safe.
    trigger_type   TEXT,
    trigger_value  TEXT
);

CREATE INDEX IF NOT EXISTS idx_skills_agent_id ON skills (agent_id);

ALTER TABLE skills ADD COLUMN IF NOT EXISTS schedulable BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE skills ADD COLUMN IF NOT EXISTS integrations TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE skills ADD COLUMN IF NOT EXISTS limited_integrations TEXT[] NOT NULL DEFAULT '{}';

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
    user_id TEXT PRIMARY KEY REFERENCES persons (id) ON DELETE CASCADE,
    notes   TEXT NOT NULL DEFAULT ''
);

-- user_integration_notes: integration-specific facts about a user.
-- Shared across all agents that have access to the same integration.
CREATE TABLE IF NOT EXISTS user_integration_notes (
    user_id        TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    integration_id TEXT NOT NULL,
    notes          TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, integration_id)
);

-- profile_details: what an agent has learned about a person, one named detail per row, so changing
-- one never touches another. scope is 'personal' or an integration ID (the map for one connected app).
CREATE TABLE IF NOT EXISTS profile_details (
    user_id      TEXT        NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    scope        TEXT        NOT NULL,
    key          TEXT        NOT NULL,
    value        TEXT        NOT NULL,
    source       TEXT        NOT NULL DEFAULT 'said',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope, key)
);

-- profile_declined: values a person refused for a detail, so the same value is not asked again in other words.
-- value_norm is the value with case, accents, punctuation and spacing set aside.
CREATE TABLE IF NOT EXISTS profile_declined (
    user_id     TEXT        NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    scope       TEXT        NOT NULL,
    key         TEXT        NOT NULL,
    value_norm  TEXT        NOT NULL,
    declined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope, key, value_norm)
);

-- profile_migrations: a person's older free-text note for a scope has been moved into details.
CREATE TABLE IF NOT EXISTS profile_migrations (
    user_id     TEXT        NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    scope       TEXT        NOT NULL,
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope)
);

-- user_agent_configs: per-user personal instructions for one Agent. Injected
-- into the system prompt so the agent can personalise responses to each user.
CREATE TABLE IF NOT EXISTS user_agent_configs (
    user_id      TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
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
    user_id TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    tool_id TEXT NOT NULL REFERENCES tools (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, tool_id)
);

-- user_app_instructions: per-user per-integration instructions injected as
-- additional context when tools from that integration are being used.
CREATE TABLE IF NOT EXISTS user_app_instructions (
    user_id        TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
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
    user_id     TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    identity_id TEXT NOT NULL DEFAULT '',
    channel_ref TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ NOT NULL
);

-- pending_turns: what a person asked for while something (a connection) stood in the
-- way, kept so it can be carried out once it is sorted. One per person per connection;
-- a newer one replaces the older. Kept in the database so a restart doesn't lose it.
CREATE TABLE IF NOT EXISTS pending_turns (
    user_id     TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
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

-- One-time split of the old users table into persons and accounts. Every old user becomes a person
-- with the same id. One that acts as a login (it has an email or password, a session, or owns something
-- a creator built) also becomes an account with that same id, linked to the person, so created_by and
-- sessions keep pointing at the right row. Foreign keys are moved to the new tables, and the old table
-- is kept under another name as a backup. It runs once: after it, there is no users table.
DO $$
DECLARE
    r record;
BEGIN
    IF to_regclass('public.users') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE users ADD COLUMN IF NOT EXISTS first_name         TEXT NOT NULL DEFAULT '';
    ALTER TABLE users ADD COLUMN IF NOT EXISTS last_name          TEXT NOT NULL DEFAULT '';
    ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified     BOOLEAN NOT NULL DEFAULT FALSE;
    ALTER TABLE users ADD COLUMN IF NOT EXISTS verification_token TEXT NOT NULL DEFAULT '';
    ALTER TABLE users ADD COLUMN IF NOT EXISTS timezone           TEXT NOT NULL DEFAULT '';
    ALTER TABLE users ADD COLUMN IF NOT EXISTS language           TEXT NOT NULL DEFAULT '';
    ALTER TABLE users ADD COLUMN IF NOT EXISTS country            TEXT NOT NULL DEFAULT '';
    ALTER TABLE users ADD COLUMN IF NOT EXISTS city               TEXT NOT NULL DEFAULT '';

    INSERT INTO persons (id, first_name, last_name, timezone, language, country, city)
    SELECT id, first_name, last_name, timezone, language, country, city FROM users
    ON CONFLICT (id) DO NOTHING;

    INSERT INTO accounts (id, email, password_hash, email_verified, verification_token, person_id)
    SELECT u.id, NULLIF(u.email, ''), u.password_hash, u.email_verified, u.verification_token, u.id
    FROM users u
    WHERE COALESCE(u.email, '') <> ''
       OR u.password_hash <> ''
       OR EXISTS (SELECT 1 FROM user_sessions s WHERE s.user_id = u.id)
       OR EXISTS (SELECT 1 FROM agents a WHERE a.created_by = u.id)
       OR EXISTS (SELECT 1 FROM integrations i WHERE i.created_by = u.id)
       OR EXISTS (SELECT 1 FROM identities i WHERE i.created_by = u.id)
    ON CONFLICT (id) DO NOTHING;

    FOR r IN
        SELECT c.conrelid::regclass::text AS tbl, c.conname AS con, a.attname AS col
        FROM pg_constraint c
        JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
        WHERE c.contype = 'f' AND c.confrelid = 'public.users'::regclass
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.con);
        IF r.col = 'created_by' THEN
            EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES accounts (id)', r.tbl, r.con, r.col);
        ELSIF r.tbl = 'user_sessions' THEN
            EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES accounts (id) ON DELETE CASCADE', r.tbl, r.con, r.col);
        ELSE
            EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES persons (id) ON DELETE CASCADE', r.tbl, r.con, r.col);
        END IF;
    END LOOP;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'public' AND table_name = 'user_sessions' AND column_name = 'user_id') THEN
        ALTER TABLE user_sessions RENAME COLUMN user_id TO account_id;
    END IF;

    ALTER TABLE users RENAME TO users_before_split;
END
$$;

-- When the email-link state of a channel was last set, so that "awaiting an email" can lapse.
ALTER TABLE messenger_channels ADD COLUMN IF NOT EXISTS email_link_state_at TIMESTAMPTZ;

-- Emails are kept lower case and trimmed (domain.NormalizeEmail). Bring older rows in line, leaving alone
-- any group that would then be the same address twice: those need a person to decide.
UPDATE accounts SET email = lower(btrim(email))
WHERE email IS NOT NULL AND email <> lower(btrim(email))
  AND (SELECT count(*) FROM accounts b WHERE lower(btrim(b.email)) = lower(btrim(accounts.email))) = 1;

-- proposals: messages an agent sent with choices the person can tap (see domain.Proposal). Saved, so a tap
-- works later and after a restart. chosen is the button tapped, once one was.
CREATE TABLE IF NOT EXISTS proposals (
    id          TEXT PRIMARY KEY,
    person_id   TEXT NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    agent_id    TEXT NOT NULL DEFAULT '',
    identity_id TEXT NOT NULL DEFAULT '',
    chat_id     TEXT NOT NULL DEFAULT '',
    thread_id   TEXT NOT NULL DEFAULT '',
    message_id  TEXT NOT NULL DEFAULT '',
    text        TEXT NOT NULL DEFAULT '',
    choices     JSONB NOT NULL DEFAULT '[]',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,
    chosen      INT,
    chosen_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_proposals_person ON proposals (person_id);
CREATE INDEX IF NOT EXISTS idx_proposals_expires ON proposals (expires_at);

-- Events pipeline: what arrived from outside (a webhook), stored before anything is done with it, so a deploy or a
-- crash cannot lose it. The decoded facts are kept, never the raw delivery. A redelivery of the same delivery is the
-- same row.
CREATE TABLE IF NOT EXISTS events_inbox (
    id              BIGSERIAL PRIMARY KEY,
    source          TEXT NOT NULL,
    connection_id   TEXT NOT NULL,
    delivery_id     TEXT NOT NULL,
    event           TEXT NOT NULL,
    facts           JSONB NOT NULL,
    subject_kind    TEXT NOT NULL DEFAULT '',
    subject_value   TEXT NOT NULL DEFAULT '',
    repeat_key      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'new',      -- new | routing | done | dropped | failed
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_by       TEXT NOT NULL DEFAULT '',
    locked_until    TIMESTAMPTZ,
    note            TEXT NOT NULL DEFAULT '',
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source, delivery_id)
);
CREATE INDEX IF NOT EXISTS events_inbox_due ON events_inbox (next_attempt_at) WHERE status IN ('new', 'routing');
