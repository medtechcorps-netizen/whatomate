\set ON_ERROR_STOP on

-- Klinik Relive comprehensive omnichannel showcase fixture.
--
-- SAFETY CONTRACT
--   * This file is intentionally a dry run and ends with ROLLBACK.
--   * It never inserts credentials, provider secrets, outbox work, inbound
--     webhook events, scheduled work, customer activity events, or settings.
--   * Every operational rule/flow is disabled or scoped to the synthetic
--     account name "[MOCK] DEMO".
--   * Every message, event, transfer, session, and Copilot run is historical
--     and terminal. Nothing in this file dispatches a provider request.
--   * Change only the final ROLLBACK after reviewing the assertions and dry-run
--     output in the intended database session.

BEGIN;
SET LOCAL app.current_organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a';

CREATE TEMP TABLE _omni_ctx AS
SELECT
    'c73f761f-5154-4fe1-9a13-06bae570277a'::uuid AS organization_id,
    u.id AS owner_id,
    COALESCE(NULLIF(u.full_name, ''), u.email) AS owner_name
FROM users u
WHERE u.email = 'admintest@rereply.com'
  AND u.deleted_at IS NULL
LIMIT 1;

DO $$
DECLARE
    target_org uuid := 'c73f761f-5154-4fe1-9a13-06bae570277a';
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM organizations
        WHERE id = target_org
          AND deleted_at IS NULL
          AND name ILIKE '%Klinik%Relive%'
    ) THEN
        RAISE EXCEPTION 'Target Klinik Relive organization is missing or does not match the expected name';
    END IF;

    IF (SELECT count(*) FROM _omni_ctx) <> 1
       OR (SELECT owner_id FROM _omni_ctx LIMIT 1) IS NULL THEN
        RAISE EXCEPTION 'Seed owner admintest@rereply.com is missing';
    END IF;

    IF (
        SELECT count(*)
        FROM contacts c
        WHERE c.organization_id = target_org
          AND c.phone_number ~ '^6000000000(1[1-9]|[23][0-9]|40)$'
          AND c.deleted_at IS NULL
          AND c.metadata->>'mock_dataset' = 'klinik-relive-crm-v2'
    ) <> 30 THEN
        RAISE EXCEPTION 'Expected Klinik Relive CRM v2 contacts 11-40 before omnichannel seeding';
    END IF;
END $$;

-- Snapshot all durable dispatch/automation queues before any fixture writes.
CREATE TEMP TABLE _omni_safety_before AS
SELECT
    (SELECT count(*) FROM outbox_jobs
      WHERE organization_id = ctx.organization_id) AS outbox_jobs,
    (SELECT count(*) FROM inbound_events
      WHERE organization_id = ctx.organization_id) AS inbound_events,
    (SELECT count(*) FROM automation_event_receipts
      WHERE organization_id = ctx.organization_id) AS automation_event_receipts,
    (SELECT count(*) FROM scheduled_jobs
      WHERE organization_id = ctx.organization_id) AS scheduled_jobs,
    (SELECT count(*) FROM outbox_events
      WHERE organization_id = ctx.organization_id) AS outbox_events,
    (SELECT count(*) FROM customer_activity_events
      WHERE organization_id = ctx.organization_id) AS customer_activity_events
FROM _omni_ctx ctx;

-- ---------------------------------------------------------------------------
-- Inactive synthetic agents and an inactive team.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_agents (
    seq integer PRIMARY KEY,
    id uuid NOT NULL,
    email text NOT NULL,
    full_name text NOT NULL
);

INSERT INTO _omni_agents VALUES
    (1, md5('klinik-relive-omnichannel-v1-agent-1')::uuid,
        'mock.omni.aina@klinik-relive.example.invalid', '[MOCK] Aina Rahman'),
    (2, md5('klinik-relive-omnichannel-v1-agent-2')::uuid,
        'mock.omni.daniel@klinik-relive.example.invalid', '[MOCK] Daniel Lee'),
    (3, md5('klinik-relive-omnichannel-v1-agent-3')::uuid,
        'mock.omni.priya@klinik-relive.example.invalid', '[MOCK] Priya Nair');

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM users u
        JOIN _omni_agents a ON a.id = u.id
        CROSS JOIN _omni_ctx ctx
        WHERE u.organization_id IS DISTINCT FROM ctx.organization_id
           OR u.email <> a.email
           OR COALESCE(u.settings->>'mock_dataset', '') NOT IN (
                '',
                'klinik-relive-omnichannel-v1'
           )
    ) THEN
        RAISE EXCEPTION 'A deterministic mock agent ID is owned by unrelated data';
    END IF;
END $$;

INSERT INTO users (
    id,
    organization_id,
    email,
    password_hash,
    full_name,
    role_id,
    settings,
    is_active,
    is_available,
    is_super_admin,
    sso_provider,
    sso_provider_id,
    created_at,
    updated_at
)
SELECT
    a.id,
    ctx.organization_id,
    a.email,
    '!disabled-synthetic-account-no-login!',
    a.full_name,
    NULL,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'login_disabled', true,
        'historical_analytics_only', true
    ),
    false,
    false,
    false,
    '',
    '',
    CURRENT_TIMESTAMP - interval '120 days',
    CURRENT_TIMESTAMP
FROM _omni_agents a
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    organization_id = EXCLUDED.organization_id,
    email = EXCLUDED.email,
    password_hash = EXCLUDED.password_hash,
    full_name = EXCLUDED.full_name,
    role_id = NULL,
    settings = EXCLUDED.settings,
    is_active = false,
    is_available = false,
    is_super_admin = false,
    sso_provider = '',
    sso_provider_id = '',
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO teams (
    id,
    organization_id,
    name,
    description,
    assignment_strategy,
    per_agent_timeout_secs,
    is_active,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-team')::uuid,
    ctx.organization_id,
    '[MOCK] Omnichannel Care Team (Inactive)',
    '[MOCK] Synthetic analytics team. Inactive by design; it must never receive live assignments.',
    'manual',
    0,
    false,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - interval '120 days',
    CURRENT_TIMESTAMP
FROM _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    organization_id = EXCLUDED.organization_id,
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    assignment_strategy = EXCLUDED.assignment_strategy,
    per_agent_timeout_secs = EXCLUDED.per_agent_timeout_secs,
    is_active = false,
    created_by_id = EXCLUDED.created_by_id,
    updated_by_id = EXCLUDED.updated_by_id,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO team_members (
    id,
    team_id,
    user_id,
    role,
    last_assigned_at,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-team-member-' || a.seq)::uuid,
    md5('klinik-relive-omnichannel-v1-team')::uuid,
    a.id,
    'agent',
    CURRENT_TIMESTAMP - make_interval(days => 7 + a.seq),
    CURRENT_TIMESTAMP - interval '120 days',
    CURRENT_TIMESTAMP
FROM _omni_agents a
ON CONFLICT (id) DO UPDATE SET
    team_id = EXCLUDED.team_id,
    user_id = EXCLUDED.user_id,
    role = EXCLUDED.role,
    last_assigned_at = EXCLUDED.last_assigned_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Provider-neutral channel accounts. They have no credentials and both
-- inbound and outbound runtime controls are explicitly disabled.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_accounts (
    seq integer PRIMARY KEY,
    id uuid NOT NULL,
    channel text NOT NULL,
    name text NOT NULL,
    status text NOT NULL,
    external_account_id text NOT NULL
);

INSERT INTO _omni_accounts VALUES
    (1, md5('klinik-relive-omnichannel-v1-account-whatsapp')::uuid,
        'whatsapp', '[MOCK] Omni - WhatsApp', 'active', 'mock-omni-whatsapp'),
    (2, md5('klinik-relive-omnichannel-v1-account-instagram')::uuid,
        'instagram', '[MOCK] Omni - Instagram', 'active', 'mock-omni-instagram'),
    (3, md5('klinik-relive-omnichannel-v1-account-messenger')::uuid,
        'messenger', '[MOCK] Omni - Messenger', 'active', 'mock-omni-messenger'),
    (4, md5('klinik-relive-omnichannel-v1-account-threads')::uuid,
        'threads', '[MOCK] Omni - Threads', 'degraded', 'mock-omni-threads'),
    (5, md5('klinik-relive-omnichannel-v1-account-email')::uuid,
        'email', '[MOCK] Omni - Email', 'suspended', 'mock-omni-email'),
    (6, md5('klinik-relive-omnichannel-v1-account-webchat')::uuid,
        'webchat', '[MOCK] Omni - Web Chat', 'disconnected', 'mock-omni-webchat');

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM channel_accounts ca
        JOIN _omni_accounts seed ON seed.id = ca.id
        CROSS JOIN _omni_ctx ctx
        WHERE ca.organization_id <> ctx.organization_id
           OR COALESCE(ca.metadata->>'mock_dataset', '') NOT IN (
                '',
                'klinik-relive-omnichannel-v1'
           )
    ) THEN
        RAISE EXCEPTION 'A deterministic channel account ID is owned by unrelated data';
    END IF;
END $$;

INSERT INTO channel_accounts (
    id,
    organization_id,
    channel,
    provider,
    name,
    external_account_id,
    status,
    capabilities,
    config,
    metadata,
    is_default_incoming,
    is_default_outgoing,
    connected_at,
    last_health_check_at,
    last_inbound_at,
    last_outbound_at,
    last_error_at,
    last_error,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    seed.id,
    ctx.organization_id,
    seed.channel,
    'mock_fixture',
    seed.name,
    seed.external_account_id,
    seed.status,
    jsonb_build_object(
        'text', true,
        'media', true,
        'templates', seed.channel = 'whatsapp',
        'interactive', seed.channel IN ('whatsapp', 'messenger', 'instagram', 'webchat'),
        'reactions', seed.channel IN ('instagram', 'messenger', 'threads'),
        'mock_only', true
    ),
    jsonb_build_object(
        'inbound_enabled', false,
        'outbound_enabled', false,
        'ai_reply_enabled', false,
        'credential_mode', 'none',
        'historical_only', true,
        'mock_only', true
    ),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'safe_to_display', true,
        'provider_calls_allowed', false,
        'description', '[MOCK] Credential-free historical channel showcase'
    ),
    false,
    false,
    CASE WHEN seed.status IN ('active', 'degraded')
        THEN CURRENT_TIMESTAMP - interval '180 days'
        ELSE NULL
    END,
    CURRENT_TIMESTAMP - make_interval(hours => seed.seq),
    CURRENT_TIMESTAMP - make_interval(days => seed.seq),
    CURRENT_TIMESTAMP - make_interval(days => seed.seq, hours => 2),
    CASE WHEN seed.status IN ('degraded', 'suspended', 'disconnected')
        THEN CURRENT_TIMESTAMP - make_interval(hours => seed.seq)
        ELSE NULL
    END,
    CASE seed.status
        WHEN 'degraded' THEN '[MOCK] Historical health warning; no live relay configured.'
        WHEN 'suspended' THEN '[MOCK] Deliberately suspended synthetic account.'
        WHEN 'disconnected' THEN '[MOCK] Deliberately disconnected synthetic account.'
        ELSE ''
    END,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - interval '180 days',
    CURRENT_TIMESTAMP
FROM _omni_accounts seed
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    channel = EXCLUDED.channel,
    provider = EXCLUDED.provider,
    name = EXCLUDED.name,
    external_account_id = EXCLUDED.external_account_id,
    status = EXCLUDED.status,
    capabilities = EXCLUDED.capabilities,
    config = EXCLUDED.config,
    metadata = EXCLUDED.metadata,
    is_default_incoming = false,
    is_default_outgoing = false,
    connected_at = EXCLUDED.connected_at,
    last_health_check_at = EXCLUDED.last_health_check_at,
    last_inbound_at = EXCLUDED.last_inbound_at,
    last_outbound_at = EXCLUDED.last_outbound_at,
    last_error_at = EXCLUDED.last_error_at,
    last_error = EXCLUDED.last_error,
    created_by_id = EXCLUDED.created_by_id,
    updated_by_id = EXCLUDED.updated_by_id,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _omni_contacts AS
SELECT
    g AS seq,
    10 + g AS contact_idx,
    c.id AS contact_id,
    c.phone_number,
    c.profile_name,
    c.metadata,
    a.id AS account_id,
    a.channel,
    a.name AS account_name,
    ag.id AS agent_id,
    ag.full_name AS agent_name
FROM generate_series(1, 30) AS series(g)
JOIN contacts c
  ON c.organization_id = (SELECT organization_id FROM _omni_ctx)
 AND c.phone_number = '6000000000' || lpad((10 + g)::text, 2, '0')
 AND c.deleted_at IS NULL
JOIN _omni_accounts a
  ON a.seq = ((g - 1) % 6) + 1
JOIN _omni_agents ag
  ON ag.seq = ((g - 1) % 3) + 1;

DO $$
BEGIN
    IF (SELECT count(*) FROM _omni_contacts) <> 30 THEN
        RAISE EXCEPTION 'Omnichannel contact mapping did not produce exactly 30 contacts';
    END IF;
END $$;

INSERT INTO contact_identities (
    id,
    organization_id,
    contact_id,
    channel_account_id,
    channel,
    external_id,
    address,
    normalized_address,
    display_name,
    avatar_url,
    is_primary,
    is_verified,
    first_seen_at,
    last_seen_at,
    metadata,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-identity-' || c.seq)::uuid,
    ctx.organization_id,
    c.contact_id,
    c.account_id,
    c.channel,
    'mock-' || c.channel || '-patient-' || lpad(c.contact_idx::text, 2, '0'),
    CASE c.channel
        WHEN 'email' THEN 'patient.' || lpad(c.contact_idx::text, 2, '0') ||
            '@klinik-relive.example.invalid'
        WHEN 'webchat' THEN 'visitor-mock-' || lpad(c.contact_idx::text, 2, '0')
        WHEN 'whatsapp' THEN c.phone_number
        ELSE '@mock_patient_' || lpad(c.contact_idx::text, 2, '0')
    END,
    lower(CASE c.channel
        WHEN 'email' THEN 'patient.' || lpad(c.contact_idx::text, 2, '0') ||
            '@klinik-relive.example.invalid'
        WHEN 'webchat' THEN 'visitor-mock-' || lpad(c.contact_idx::text, 2, '0')
        WHEN 'whatsapp' THEN c.phone_number
        ELSE '@mock_patient_' || lpad(c.contact_idx::text, 2, '0')
    END),
    c.profile_name,
    '',
    true,
    true,
    CURRENT_TIMESTAMP - make_interval(days => 90 + c.seq),
    CURRENT_TIMESTAMP - make_interval(hours => c.seq),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'historical_only', true,
        'identity_quality', CASE WHEN c.seq % 4 = 0 THEN 'enriched' ELSE 'verified synthetic' END
    ),
    CURRENT_TIMESTAMP - make_interval(days => 90 + c.seq),
    CURRENT_TIMESTAMP
FROM _omni_contacts c
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    channel_account_id = EXCLUDED.channel_account_id,
    channel = EXCLUDED.channel,
    external_id = EXCLUDED.external_id,
    address = EXCLUDED.address,
    normalized_address = EXCLUDED.normalized_address,
    display_name = EXCLUDED.display_name,
    avatar_url = '',
    is_primary = true,
    is_verified = true,
    first_seen_at = EXCLUDED.first_seen_at,
    last_seen_at = EXCLUDED.last_seen_at,
    metadata = EXCLUDED.metadata,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _omni_conversations AS
SELECT
    c.seq,
    md5('klinik-relive-omnichannel-v1-conversation-' || c.seq)::uuid AS id,
    md5('klinik-relive-omnichannel-v1-identity-' || c.seq)::uuid AS identity_id,
    c.contact_id,
    c.phone_number,
    c.profile_name,
    c.account_id,
    c.account_name,
    c.channel,
    c.agent_id,
    c.agent_name,
    CASE
        WHEN c.seq <= 12 THEN 'open'
        WHEN c.seq <= 18 THEN 'pending'
        WHEN c.seq <= 22 THEN 'snoozed'
        WHEN c.seq <= 28 THEN 'resolved'
        ELSE 'archived'
    END AS status
FROM _omni_contacts c;

INSERT INTO inbox_conversations (
    id,
    organization_id,
    channel_account_id,
    contact_id,
    contact_identity_id,
    channel,
    external_conversation_id,
    status,
    subject,
    priority,
    assigned_user_id,
    assigned_team_id,
    unread_count,
    last_message_preview,
    opened_at,
    last_message_at,
    last_inbound_at,
    last_outbound_at,
    service_window_ends_at,
    snoozed_until,
    resolved_at,
    archived_at,
    config,
    metadata,
    created_at,
    updated_at
)
SELECT
    c.id,
    ctx.organization_id,
    c.account_id,
    c.contact_id,
    c.identity_id,
    c.channel,
    'mock-conversation-' || lpad(c.seq::text, 3, '0'),
    c.status,
    CASE c.seq % 6
        WHEN 0 THEN '[MOCK] Package renewal and follow-up'
        WHEN 1 THEN '[MOCK] Initial assessment enquiry'
        WHEN 2 THEN '[MOCK] Appointment availability'
        WHEN 3 THEN '[MOCK] Pricing and package comparison'
        WHEN 4 THEN '[MOCK] Post-visit care question'
        ELSE '[MOCK] Wellness programme enquiry'
    END,
    CASE c.seq % 4 WHEN 0 THEN 3 WHEN 1 THEN 2 WHEN 2 THEN 1 ELSE 0 END,
    c.agent_id,
    md5('klinik-relive-omnichannel-v1-team')::uuid,
    CASE WHEN c.seq <= 12 THEN 1 ELSE 0 END,
    '',
    date_trunc('month', CURRENT_TIMESTAMP) -
        make_interval(days => 30 + (c.seq % 20)),
    NULL,
    NULL,
    NULL,
    NULL,
    CASE WHEN c.status = 'snoozed'
        THEN CURRENT_TIMESTAMP + make_interval(days => 60 + c.seq)
        ELSE NULL
    END,
    CASE WHEN c.status IN ('resolved', 'archived')
        THEN CURRENT_TIMESTAMP - make_interval(days => c.seq % 9 + 1)
        ELSE NULL
    END,
    CASE WHEN c.status = 'archived'
        THEN CURRENT_TIMESTAMP - make_interval(days => c.seq % 5 + 1)
        ELSE NULL
    END,
    jsonb_build_object(
        'outbound_enabled', false,
        'ai_reply_enabled', false,
        'historical_only', true
    ),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'journey', CASE c.seq % 4
            WHEN 0 THEN 'follow_up'
            WHEN 1 THEN 'lead_qualification'
            WHEN 2 THEN 'appointment'
            ELSE 'package_consultation'
        END,
        'risk', CASE c.seq % 5 WHEN 0 THEN 'needs_attention' ELSE 'normal' END
    ),
    date_trunc('month', CURRENT_TIMESTAMP) -
        make_interval(days => 30 + (c.seq % 20)),
    CURRENT_TIMESTAMP
FROM _omni_conversations c
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    channel_account_id = EXCLUDED.channel_account_id,
    contact_id = EXCLUDED.contact_id,
    contact_identity_id = EXCLUDED.contact_identity_id,
    channel = EXCLUDED.channel,
    external_conversation_id = EXCLUDED.external_conversation_id,
    status = EXCLUDED.status,
    subject = EXCLUDED.subject,
    priority = EXCLUDED.priority,
    assigned_user_id = EXCLUDED.assigned_user_id,
    assigned_team_id = EXCLUDED.assigned_team_id,
    unread_count = EXCLUDED.unread_count,
    opened_at = EXCLUDED.opened_at,
    snoozed_until = EXCLUDED.snoozed_until,
    resolved_at = EXCLUDED.resolved_at,
    archived_at = EXCLUDED.archived_at,
    config = EXCLUDED.config,
    metadata = EXCLUDED.metadata,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO conversation_participants (
    id,
    organization_id,
    conversation_id,
    participant_key,
    role,
    user_id,
    contact_identity_id,
    external_id,
    display_name,
    address,
    joined_at,
    left_at,
    metadata,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-participant-customer-' || c.seq)::uuid,
    ctx.organization_id,
    c.id,
    'external:mock-' || c.channel || '-patient-' || lpad((10 + c.seq)::text, 2, '0'),
    'customer',
    NULL,
    c.identity_id,
    'mock-' || c.channel || '-patient-' || lpad((10 + c.seq)::text, 2, '0'),
    c.profile_name,
    identity.address,
    date_trunc('month', CURRENT_TIMESTAMP) - make_interval(days => 30 + c.seq % 20),
    NULL,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true
    ),
    date_trunc('month', CURRENT_TIMESTAMP) - make_interval(days => 30 + c.seq % 20),
    CURRENT_TIMESTAMP
FROM _omni_conversations c
CROSS JOIN _omni_ctx ctx
JOIN contact_identities identity
  ON identity.id = c.identity_id
 AND identity.organization_id = ctx.organization_id
UNION ALL
SELECT
    md5('klinik-relive-omnichannel-v1-participant-agent-' || c.seq)::uuid,
    ctx.organization_id,
    c.id,
    'user:' || c.agent_id::text,
    'agent',
    c.agent_id,
    NULL,
    '',
    c.agent_name,
    '',
    date_trunc('month', CURRENT_TIMESTAMP) - make_interval(days => 29 + c.seq % 20),
    CASE WHEN c.status IN ('resolved', 'archived')
        THEN CURRENT_TIMESTAMP - make_interval(days => c.seq % 5 + 1)
        ELSE NULL
    END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'inactive_demo_agent', true
    ),
    date_trunc('month', CURRENT_TIMESTAMP) - make_interval(days => 29 + c.seq % 20),
    CURRENT_TIMESTAMP
FROM _omni_conversations c
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    conversation_id = EXCLUDED.conversation_id,
    participant_key = EXCLUDED.participant_key,
    role = EXCLUDED.role,
    user_id = EXCLUDED.user_id,
    contact_identity_id = EXCLUDED.contact_identity_id,
    external_id = EXCLUDED.external_id,
    display_name = EXCLUDED.display_name,
    address = EXCLUDED.address,
    joined_at = EXCLUDED.joined_at,
    left_at = EXCLUDED.left_at,
    metadata = EXCLUDED.metadata,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Terminal current-month agent analytics. No active transfer or open break
-- exists. The team and all three agents are inactive.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_transfer_rows AS
SELECT
    seq,
    md5('klinik-relive-omnichannel-v1-transfer-' || seq)::uuid AS id,
    ((seq - 1) % 3) + 1 AS agent_seq,
    10 + seq AS contact_idx,
    CASE (seq - 1) % 4
        WHEN 0 THEN 'manual'
        WHEN 1 THEN 'flow'
        WHEN 2 THEN 'keyword'
        ELSE 'chatbot_disabled'
    END AS source,
    CASE WHEN seq <= 18 THEN 'resumed' ELSE 'expired' END AS status,
    LEAST(
        date_trunc('month', CURRENT_TIMESTAMP)
            + make_interval(
                days => 1 + ((seq * 2) %
                    GREATEST(EXTRACT(day FROM CURRENT_TIMESTAMP)::integer, 1)),
                hours => 9 + (seq % 8)
              ),
        CURRENT_TIMESTAMP - interval '3 hours'
    ) AS transferred_at
FROM generate_series(1, 24) AS series(seq);

INSERT INTO agent_transfers (
    id,
    organization_id,
    contact_id,
    whats_app_account,
    phone_number,
    status,
    source,
    agent_id,
    team_id,
    transferred_by_user_id,
    notes,
    transferred_at,
    resumed_at,
    resumed_by,
    sla_response_deadline,
    sla_resolution_deadline,
    sla_escalation_at,
    expires_at,
    picked_up_at,
    first_response_at,
    escalation_level,
    escalated_at,
    sla_breached,
    sla_breached_at,
    created_at,
    updated_at
)
SELECT
    seed.id,
    ctx.organization_id,
    contact.id,
    '[MOCK] DEMO',
    contact.phone_number,
    seed.status,
    seed.source,
    agent.id,
    md5('klinik-relive-omnichannel-v1-team')::uuid,
    ctx.owner_id,
    '[MOCK] Historical terminal transfer for analytics only. No assignment notification was sent.',
    seed.transferred_at,
    CASE WHEN seed.status = 'resumed'
        THEN seed.transferred_at + make_interval(mins => 35 + (seed.seq % 7) * 9)
        ELSE NULL
    END,
    CASE WHEN seed.status = 'resumed' THEN agent.id ELSE NULL END,
    seed.transferred_at + interval '10 minutes',
    seed.transferred_at + interval '120 minutes',
    seed.transferred_at + interval '8 minutes',
    seed.transferred_at + interval '180 minutes',
    seed.transferred_at + make_interval(mins => 2 + (seed.seq % 6)),
    seed.transferred_at + make_interval(mins => 4 + (seed.seq % 9)),
    CASE WHEN seed.seq % 5 = 0 THEN 2 ELSE 0 END,
    CASE WHEN seed.seq % 5 = 0
        THEN seed.transferred_at + interval '12 minutes'
        ELSE NULL
    END,
    seed.seq % 5 = 0,
    CASE WHEN seed.seq % 5 = 0
        THEN seed.transferred_at + interval '12 minutes'
        ELSE NULL
    END,
    seed.transferred_at,
    seed.transferred_at + make_interval(mins => 2 + (seed.seq % 6))
FROM _omni_transfer_rows seed
CROSS JOIN _omni_ctx ctx
JOIN _omni_agents agent ON agent.seq = seed.agent_seq
JOIN contacts contact
  ON contact.organization_id = ctx.organization_id
 AND contact.phone_number =
    '6000000000' || lpad(seed.contact_idx::text, 2, '0')
 AND contact.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    whats_app_account = '[MOCK] DEMO',
    phone_number = EXCLUDED.phone_number,
    status = EXCLUDED.status,
    source = EXCLUDED.source,
    agent_id = EXCLUDED.agent_id,
    team_id = EXCLUDED.team_id,
    transferred_by_user_id = EXCLUDED.transferred_by_user_id,
    notes = EXCLUDED.notes,
    transferred_at = EXCLUDED.transferred_at,
    resumed_at = EXCLUDED.resumed_at,
    resumed_by = EXCLUDED.resumed_by,
    sla_response_deadline = EXCLUDED.sla_response_deadline,
    sla_resolution_deadline = EXCLUDED.sla_resolution_deadline,
    sla_escalation_at = EXCLUDED.sla_escalation_at,
    expires_at = EXCLUDED.expires_at,
    picked_up_at = EXCLUDED.picked_up_at,
    first_response_at = EXCLUDED.first_response_at,
    escalation_level = EXCLUDED.escalation_level,
    escalated_at = EXCLUDED.escalated_at,
    sla_breached = EXCLUDED.sla_breached,
    sla_breached_at = EXCLUDED.sla_breached_at,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _omni_availability_rows AS
SELECT
    seq,
    ((seq - 1) % 3) + 1 AS agent_seq,
    ((seq - 1) / 3) + 1 AS break_seq,
    date_trunc('month', CURRENT_TIMESTAMP)
        + make_interval(
            days => 1 + ((((seq - 1) / 3) * 4 + ((seq - 1) % 3)) %
                GREATEST(EXTRACT(day FROM CURRENT_TIMESTAMP)::integer, 1)),
            hours => 11 + (((seq - 1) % 3) * 2)
          ) AS started_at
FROM generate_series(1, 12) AS series(seq);

INSERT INTO user_availability_logs (
    id,
    user_id,
    organization_id,
    is_available,
    started_at,
    ended_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-availability-' || seed.seq)::uuid,
    agent.id,
    ctx.organization_id,
    false,
    LEAST(seed.started_at, CURRENT_TIMESTAMP - interval '90 minutes'),
    LEAST(seed.started_at, CURRENT_TIMESTAMP - interval '90 minutes')
        + make_interval(mins => seed.break_seq * 15)
FROM _omni_availability_rows seed
CROSS JOIN _omni_ctx ctx
JOIN _omni_agents agent ON agent.seq = seed.agent_seq
ON CONFLICT (id) DO UPDATE SET
    user_id = EXCLUDED.user_id,
    organization_id = EXCLUDED.organization_id,
    is_available = false,
    started_at = EXCLUDED.started_at,
    ended_at = EXCLUDED.ended_at;

-- ---------------------------------------------------------------------------
-- Seven shared analytics widgets. Message widgets cover the complete synthetic
-- message dataset so they remain compatible with older deployments whose
-- account-field SQL alias is incorrect; sessions and transfers show the full
-- synthetic organizational outcome sets supported by those data sources.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_widget_defs (
    seq integer PRIMARY KEY,
    name text NOT NULL,
    description text NOT NULL,
    data_source text NOT NULL,
    filters jsonb NOT NULL,
    display_type text NOT NULL,
    chart_type text NOT NULL,
    group_by_field text NOT NULL,
    color text NOT NULL,
    size text NOT NULL,
    grid_x integer NOT NULL,
    grid_y integer NOT NULL,
    grid_w integer NOT NULL,
    grid_h integer NOT NULL
);

INSERT INTO _omni_widget_defs VALUES
    (1, '[MOCK] Omnichannel Messages',
        'Current-period historical messages across six synthetic channels.',
        'messages',
        '[]'::jsonb,
        'number', '', '', 'blue', 'small', 0, 26, 3, 3),
    (2, '[MOCK] Message Volume Trend',
        'Daily synthetic message volume across the mock omnichannel dataset.',
        'messages',
        '[]'::jsonb,
        'chart', 'line', '', 'purple', 'medium', 3, 26, 6, 5),
    (3, '[MOCK] Direction Mix',
        'Incoming and outgoing synthetic conversation balance.',
        'messages',
        '[]'::jsonb,
        'chart', 'pie', 'direction', 'green', 'medium', 9, 26, 3, 5),
    (4, '[MOCK] Delivery Outcomes',
        'Terminal historical delivery states; no provider delivery was attempted.',
        'messages',
        '[]'::jsonb,
        'chart', 'pie', 'status', 'orange', 'medium', 0, 31, 4, 5),
    (5, '[MOCK] Message Type Mix',
        'Text, interactive, flow, template, location, and contact examples.',
        'messages',
        '[]'::jsonb,
        'chart', 'bar', 'message_type', 'blue', 'medium', 4, 31, 4, 5),
    (6, '[MOCK] Chatbot Session Outcomes',
        'Completed, timeout, and cancelled terminal demo sessions.',
        'sessions', '[]'::jsonb,
        'chart', 'pie', 'status', 'purple', 'medium', 8, 31, 4, 5),
    (7, '[MOCK] Agent Transfer Sources',
        'Terminal synthetic handoffs by manual, flow, keyword, and disabled-chatbot source.',
        'transfers', '[]'::jsonb,
        'chart', 'bar', 'source', 'red', 'large', 0, 36, 8, 5);

INSERT INTO widgets (
    id,
    organization_id,
    user_id,
    name,
    description,
    data_source,
    metric,
    field,
    filters,
    display_type,
    chart_type,
    group_by_field,
    show_change,
    color,
    size,
    display_order,
    grid_x,
    grid_y,
    grid_w,
    grid_h,
    config,
    is_shared,
    is_default,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-widget-' || seed.seq)::uuid,
    ctx.organization_id,
    ctx.owner_id,
    seed.name,
    seed.description,
    seed.data_source,
    'count',
    '',
    seed.filters,
    seed.display_type,
    seed.chart_type,
    seed.group_by_field,
    true,
    seed.color,
    seed.size,
    40 + seed.seq,
    seed.grid_x,
    seed.grid_y,
    seed.grid_w,
    seed.grid_h,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'historical_only', true
    ),
    true,
    false,
    CURRENT_TIMESTAMP,
    CURRENT_TIMESTAMP
FROM _omni_widget_defs seed
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    user_id = EXCLUDED.user_id,
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    data_source = EXCLUDED.data_source,
    metric = 'count',
    field = '',
    filters = EXCLUDED.filters,
    display_type = EXCLUDED.display_type,
    chart_type = EXCLUDED.chart_type,
    group_by_field = EXCLUDED.group_by_field,
    show_change = true,
    color = EXCLUDED.color,
    size = EXCLUDED.size,
    display_order = EXCLUDED.display_order,
    grid_x = EXCLUDED.grid_x,
    grid_y = EXCLUDED.grid_y,
    grid_w = EXCLUDED.grid_w,
    grid_h = EXCLUDED.grid_h,
    config = EXCLUDED.config,
    is_shared = true,
    is_default = false,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Disabled chatbot keyword library. These records are visible in the UI but
-- cannot match or send because every rule is disabled.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_keyword_defs (
    seq integer PRIMARY KEY,
    name text NOT NULL,
    keywords jsonb NOT NULL,
    match_type text NOT NULL,
    response_type text NOT NULL,
    response_content jsonb NOT NULL
);

INSERT INTO _omni_keyword_defs VALUES
    (1, '[MOCK] Appointment availability',
        '["[MOCK:APPOINTMENT]","mock appointment slot"]'::jsonb,
        'contains', 'text',
        '{"body":"[MOCK] I can show synthetic appointment choices for a human to review.","buttons":[{"id":"mock_morning","title":"Morning"},{"id":"mock_afternoon","title":"Afternoon"}]}'::jsonb),
    (2, '[MOCK] Package comparison',
        '["[MOCK:PACKAGE]","mock package comparison"]'::jsonb,
        'contains', 'text',
        '{"body":"[MOCK] Here is a demo-only package comparison. No purchase was created."}'::jsonb),
    (3, '[MOCK] Pricing request',
        '["[MOCK:PRICING]"]'::jsonb,
        'exact', 'text',
        '{"body":"[MOCK] Pricing in this record is synthetic and requires human confirmation."}'::jsonb),
    (4, '[MOCK] Follow-up request',
        '["[MOCK:FOLLOWUP]","mock follow up"]'::jsonb,
        'contains', 'text',
        '{"body":"[MOCK] Your preferred follow-up window is recorded for demo review only."}'::jsonb),
    (5, '[MOCK] Clinic hours',
        '["[MOCK:HOURS]"]'::jsonb,
        'exact', 'text',
        '{"body":"[MOCK] Demo hours: Monday-Friday 9:00-18:00 and Saturday 9:00-13:00."}'::jsonb),
    (6, '[MOCK] Location request',
        '["^\\[MOCK:LOCATION\\]$"]'::jsonb,
        'regex', 'text',
        '{"body":"[MOCK] The synthetic showcase location is Kuala Lumpur; verify real details with staff."}'::jsonb),
    (7, '[MOCK] Preparation guidance',
        '["[MOCK:PREP]","mock preparation"]'::jsonb,
        'contains', 'text',
        '{"body":"[MOCK] General preparation notes only; clinical instructions require practitioner review."}'::jsonb),
    (8, '[MOCK] Payment options',
        '["[MOCK:PAYMENT]"]'::jsonb,
        'exact', 'text',
        '{"body":"[MOCK] Demo payment options are card and bank transfer. No payment link was created."}'::jsonb),
    (9, '[MOCK] Reschedule request',
        '["[MOCK:RESCHEDULE]","mock reschedule"]'::jsonb,
        'contains', 'text',
        '{"body":"[MOCK] A reschedule preference was captured; no appointment was changed."}'::jsonb),
    (10, '[MOCK] Human care request',
        '["[MOCK:HUMAN]"]'::jsonb,
        'exact', 'transfer',
        '{"body":"[MOCK] This would request a human handoff in a live configuration."}'::jsonb),
    (11, '[MOCK] Safety escalation',
        '["^\\[MOCK:URGENT\\]$"]'::jsonb,
        'regex', 'transfer',
        '{"body":"[MOCK] Demo safety escalation. A real urgent concern requires immediate human review."}'::jsonb),
    (12, '[MOCK] Feedback capture',
        '["[MOCK:FEEDBACK]","mock feedback"]'::jsonb,
        'contains', 'text',
        '{"body":"[MOCK] Thank you. This synthetic feedback was not submitted externally."}'::jsonb);

INSERT INTO keyword_rules (
    id,
    organization_id,
    whats_app_account,
    name,
    is_enabled,
    priority,
    keywords,
    match_type,
    case_sensitive,
    response_type,
    response_content,
    conditions,
    active_from,
    active_until,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-keyword-' || seed.seq)::uuid,
    ctx.organization_id,
    '[MOCK] DEMO',
    seed.name,
    false,
    seed.seq * 10,
    seed.keywords,
    seed.match_type,
    false,
    seed.response_type,
    seed.response_content || jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'delivery_attempted', false
    ),
    '[MOCK] Disabled historical showcase rule.',
    NULL,
    NULL,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => 70 - seed.seq),
    CURRENT_TIMESTAMP
FROM _omni_keyword_defs seed
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    whats_app_account = '[MOCK] DEMO',
    name = EXCLUDED.name,
    is_enabled = false,
    priority = EXCLUDED.priority,
    keywords = EXCLUDED.keywords,
    match_type = EXCLUDED.match_type,
    case_sensitive = false,
    response_type = EXCLUDED.response_type,
    response_content = EXCLUDED.response_content,
    conditions = EXCLUDED.conditions,
    active_from = NULL,
    active_until = NULL,
    created_by_id = EXCLUDED.created_by_id,
    updated_by_id = EXCLUDED.updated_by_id,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Complete the existing 11-session CRM fixture to 24 terminal sessions and
-- give every one of the final 24 sessions a six-message transcript.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_new_session_rows AS
SELECT
    seq,
    md5('klinik-relive-omnichannel-v1-chatbot-session-' || seq)::uuid AS id,
    11 + seq AS contact_idx,
    md5('klinik-relive-omnichannel-v1-chat-flow-' ||
        (((seq - 1) % 8) + 1))::uuid AS flow_id,
    CASE
        WHEN seq IN (13, 17, 20, 24) THEN 'timeout'
        WHEN seq IN (15, 22) THEN 'cancelled'
        ELSE 'completed'
    END AS status,
    CASE
        WHEN seq <= 18 THEN LEAST(
            date_trunc('month', CURRENT_TIMESTAMP)
                + make_interval(
                    days => 1 + ((seq * 2) %
                        GREATEST(EXTRACT(day FROM CURRENT_TIMESTAMP)::integer, 1)),
                    hours => 10 + (seq % 5)
                  ),
            CURRENT_TIMESTAMP - interval '2 hours'
        )
        ELSE date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
            + make_interval(
                days => 2 + ((seq * 3) % 20),
                hours => 10 + (seq % 5)
              )
    END AS started_at
FROM generate_series(12, 24) AS series(seq);

INSERT INTO chatbot_sessions (
    id,
    organization_id,
    contact_id,
    whats_app_account,
    phone_number,
    status,
    current_flow_id,
    current_step,
    step_retries,
    session_data,
    started_at,
    last_activity_at,
    completed_at,
    created_at,
    updated_at
)
SELECT
    seed.id,
    ctx.organization_id,
    contact.id,
    '[MOCK] DEMO',
    contact.phone_number,
    seed.status,
    NULL,
    'end',
    CASE WHEN seed.status = 'timeout' THEN 2 ELSE 0 END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'historical_only', true,
        'delivery_attempted', false,
        'intent', CASE seed.seq % 4
            WHEN 0 THEN 'appointment'
            WHEN 1 THEN 'package inquiry'
            WHEN 2 THEN 'follow-up'
            ELSE 'support'
        END,
        'service_interest', CASE seed.seq % 3
            WHEN 0 THEN 'Initial assessment'
            WHEN 1 THEN 'Follow-up care'
            ELSE 'Wellness package'
        END,
        'preferred_date', to_char(seed.started_at + interval '7 days', 'YYYY-MM-DD'),
        'preferred_language', CASE WHEN seed.seq % 2 = 0 THEN 'English' ELSE 'Bahasa Melayu' END,
        'lead_temperature', CASE seed.seq % 3
            WHEN 0 THEN 'hot'
            WHEN 1 THEN 'warm'
            ELSE 'nurture'
        END,
        'outcome', CASE seed.status
            WHEN 'completed' THEN 'demo journey completed'
            WHEN 'timeout' THEN 'demo journey timed out safely'
            ELSE 'demo journey cancelled safely'
        END,
        'consent', 'synthetic_demo_only'
    ),
    seed.started_at,
    seed.started_at + interval '18 minutes',
    seed.started_at + interval '20 minutes',
    seed.started_at,
    seed.started_at + interval '20 minutes'
FROM _omni_new_session_rows seed
CROSS JOIN _omni_ctx ctx
JOIN contacts contact
  ON contact.organization_id = ctx.organization_id
 AND contact.phone_number =
    '6000000000' || lpad(seed.contact_idx::text, 2, '0')
 AND contact.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    whats_app_account = '[MOCK] DEMO',
    phone_number = EXCLUDED.phone_number,
    status = EXCLUDED.status,
    current_flow_id = NULL,
    current_step = 'end',
    step_retries = EXCLUDED.step_retries,
    session_data = EXCLUDED.session_data,
    started_at = EXCLUDED.started_at,
    last_activity_at = EXCLUDED.last_activity_at,
    completed_at = EXCLUDED.completed_at,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

UPDATE chatbot_sessions session
SET
    whats_app_account = '[MOCK] DEMO',
    current_flow_id = NULL,
    current_step = 'end',
    session_data = COALESCE(session.session_data, '{}'::jsonb) ||
        jsonb_build_object(
            'omnichannel_extension', 'klinik-relive-omnichannel-v1',
            'historical_only', true,
            'delivery_attempted', false,
            'service_interest', CASE series.seq % 3
                WHEN 0 THEN 'Initial assessment'
                WHEN 1 THEN 'Follow-up care'
                ELSE 'Wellness package'
            END,
            'preferred_date', to_char(session.started_at + interval '7 days', 'YYYY-MM-DD'),
            'preferred_language', CASE WHEN series.seq % 2 = 0
                THEN 'English'
                ELSE 'Bahasa Melayu'
            END,
            'lead_temperature', CASE series.seq % 3
                WHEN 0 THEN 'hot'
                WHEN 1 THEN 'warm'
                ELSE 'nurture'
            END,
            'consent', 'synthetic_demo_only',
            'outcome', CASE session.status
                WHEN 'completed' THEN 'demo journey completed'
                WHEN 'timeout' THEN 'demo journey timed out safely'
                ELSE 'demo journey cancelled safely'
            END
        ),
    updated_at = CURRENT_TIMESTAMP
FROM generate_series(1, 11) AS series(seq)
WHERE session.id =
    md5('klinik-relive-crm-v2-chatbot-session-' || series.seq)::uuid
  AND session.organization_id = (SELECT organization_id FROM _omni_ctx)
  AND session.status IN ('completed', 'timeout', 'cancelled');

CREATE TEMP TABLE _omni_session_map AS
SELECT
    seq,
    md5('klinik-relive-crm-v2-chatbot-session-' || seq)::uuid AS session_id
FROM generate_series(1, 11) AS series(seq)
UNION ALL
SELECT
    seq,
    md5('klinik-relive-omnichannel-v1-chatbot-session-' || seq)::uuid
FROM generate_series(12, 24) AS series(seq);

DO $$
BEGIN
    IF (
        SELECT count(*)
        FROM _omni_session_map map
        JOIN chatbot_sessions session ON session.id = map.session_id
        WHERE session.organization_id = (SELECT organization_id FROM _omni_ctx)
          AND session.status IN ('completed', 'timeout', 'cancelled')
          AND session.completed_at IS NOT NULL
          AND session.deleted_at IS NULL
    ) <> 24 THEN
        RAISE EXCEPTION 'The final terminal chatbot session set is not exactly 24 rows';
    END IF;
END $$;

INSERT INTO chatbot_session_messages (
    id,
    session_id,
    direction,
    message,
    step_name,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-session-message-' ||
        map.seq || '-' || message_no)::uuid,
    map.session_id,
    CASE WHEN message_no % 2 = 1 THEN 'incoming' ELSE 'outgoing' END,
    CASE message_no
        WHEN 1 THEN '[MOCK] Hi, I would like help planning my next clinic step.'
        WHEN 2 THEN '[MOCK] Welcome. I can collect preferences for a human agent to review.'
        WHEN 3 THEN CASE map.seq % 3
            WHEN 0 THEN '[MOCK] I am interested in an initial assessment.'
            WHEN 1 THEN '[MOCK] I would like to compare the follow-up packages.'
            ELSE '[MOCK] I need help choosing a suitable appointment window.'
        END
        WHEN 4 THEN '[MOCK] Please share your preferred day, time, and language.'
        WHEN 5 THEN CASE WHEN map.seq % 2 = 0
            THEN '[MOCK] Weekday morning, in English, would be ideal.'
            ELSE '[MOCK] Saturday afternoon, in Bahasa Melayu, would be ideal.'
        END
        ELSE CASE session.status
            WHEN 'completed' THEN '[MOCK] Demo summary saved for human review. Nothing was booked or sent.'
            WHEN 'timeout' THEN '[MOCK] Demo session timed out safely. No external action was created.'
            ELSE '[MOCK] Demo session was cancelled safely. No external action was created.'
        END
    END,
    CASE message_no
        WHEN 1 THEN 'intent'
        WHEN 2 THEN 'welcome'
        WHEN 3 THEN 'service_interest'
        WHEN 4 THEN 'preference_prompt'
        WHEN 5 THEN 'preference_capture'
        ELSE CASE WHEN map.seq % 4 = 0 THEN 'ai_response' ELSE 'end' END
    END,
    session.started_at + make_interval(mins => message_no * 3),
    session.started_at + make_interval(mins => message_no * 3)
FROM _omni_session_map map
JOIN chatbot_sessions session
  ON session.id = map.session_id
CROSS JOIN generate_series(1, 6) AS transcript(message_no)
ON CONFLICT (id) DO UPDATE SET
    session_id = EXCLUDED.session_id,
    direction = EXCLUDED.direction,
    message = EXCLUDED.message,
    step_name = EXCLUDED.step_name,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Qwen Copilot history. These are completed/failed/blocked/cancelled database
-- records only; no setting or API key is created and no model call is made.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_copilot_rows AS
SELECT
    seq,
    md5('klinik-relive-omnichannel-v1-copilot-run-' || seq)::uuid AS id,
    ((seq - 1) % 30) + 1 AS conversation_seq,
    10 + (((seq - 1) % 30) + 1) AS contact_idx,
    CASE (seq - 1) % 4
        WHEN 0 THEN 'reply'
        WHEN 1 THEN 'summary'
        WHEN 2 THEN 'qualify'
        ELSE 'extract_actions'
    END AS task_type,
    CASE
        WHEN seq <= 42 THEN 'completed'
        WHEN seq <= 44 THEN 'failed'
        WHEN seq <= 46 THEN 'blocked'
        ELSE 'cancelled'
    END AS status,
    CASE
        WHEN seq <= 36 THEN LEAST(
            date_trunc('month', CURRENT_TIMESTAMP)
                + make_interval(
                    days => 1 + ((seq * 2) %
                        GREATEST(EXTRACT(day FROM CURRENT_TIMESTAMP)::integer, 1)),
                    hours => 9 + (seq % 8)
                  ),
            CURRENT_TIMESTAMP - interval '15 minutes'
        )
        ELSE date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
            + make_interval(days => 2 + ((seq * 3) % 20), hours => 9 + (seq % 8))
    END AS created_at
FROM generate_series(1, 48) AS series(seq);

INSERT INTO copilot_runs (
    id,
    organization_id,
    contact_id,
    requested_by_id,
    task_type,
    status,
    model,
    prompt_version,
    input_message_ids,
    input_hash,
    result_text,
    result_data,
    context_source_ids,
    context_source_names,
    safety_labels,
    warnings,
    prompt_tokens,
    completion_tokens,
    latency_ms,
    error_code,
    error_message,
    idempotency_key,
    expires_at,
    version,
    created_at,
    updated_at
)
SELECT
    seed.id,
    ctx.organization_id,
    contact.id,
    ctx.owner_id,
    seed.task_type,
    seed.status,
    'qwen3.7-plus',
    'mock-history-v1',
    jsonb_build_array(
        md5('klinik-relive-omnichannel-v1-message-' ||
            (((seed.conversation_seq - 1) * 3) + 1))::uuid::text,
        md5('klinik-relive-omnichannel-v1-message-' ||
            (((seed.conversation_seq - 1) * 3) + 2))::uuid::text,
        md5('klinik-relive-omnichannel-v1-message-' ||
            (((seed.conversation_seq - 1) * 3) + 3))::uuid::text
    ),
    md5('klinik-relive-omnichannel-v1-copilot-input-' || seed.seq),
    CASE
        WHEN seed.status <> 'completed' THEN ''
        WHEN seed.task_type = 'reply' THEN
            '[MOCK] Thank you for sharing your preferences. I have prepared a summary for a Klinik Relive team member to review; no appointment or payment has been completed.'
        WHEN seed.task_type = 'summary' THEN
            '[MOCK] Intent: appointment or package guidance. Preferences: weekday morning, human confirmation required. Open item: verify availability and current pricing.'
        WHEN seed.task_type = 'qualify' THEN
            '{"summary":"[MOCK] Warm wellness lead","intent":"appointment","urgency":"routine","fit":"human review required","objections":["pricing clarity"],"recommended_next_step":"agent verifies slot and price"}'
        ELSE
            '{"actions":[{"title":"[MOCK] Verify appointment availability","owner_hint":"care team","due_hint":"next business day","evidence":"customer requested a weekday morning"},{"title":"[MOCK] Confirm current package pricing","owner_hint":"care team","due_hint":"before reply","evidence":"customer asked for package comparison"}]}'
    END,
    CASE
        WHEN seed.task_type = 'qualify' AND seed.status = 'completed' THEN
            jsonb_build_object(
                'summary', '[MOCK] Warm wellness lead',
                'intent', 'appointment',
                'urgency', 'routine',
                'fit', 'human review required',
                'objections', jsonb_build_array('pricing clarity'),
                'recommended_next_step', 'agent verifies slot and price',
                'mock_dataset', 'klinik-relive-omnichannel-v1'
            )
        WHEN seed.task_type = 'extract_actions' AND seed.status = 'completed' THEN
            jsonb_build_object(
                'actions', jsonb_build_array(
                    jsonb_build_object(
                        'title', '[MOCK] Verify appointment availability',
                        'owner_hint', 'care team',
                        'due_hint', 'next business day',
                        'evidence', 'customer requested a weekday morning'
                    ),
                    jsonb_build_object(
                        'title', '[MOCK] Confirm current package pricing',
                        'owner_hint', 'care team',
                        'due_hint', 'before reply',
                        'evidence', 'customer asked for package comparison'
                    )
                ),
                'mock_dataset', 'klinik-relive-omnichannel-v1'
            )
        ELSE jsonb_build_object(
            'mock_dataset', 'klinik-relive-omnichannel-v1',
            'historical_only', true
        )
    END,
    jsonb_build_array(
        md5('klinik-relive-omnichannel-v1-ai-context-' ||
            (((seed.seq - 1) % 10) + 1))::uuid::text,
        md5('klinik-relive-omnichannel-v1-ai-context-' ||
            ((seed.seq % 10) + 1))::uuid::text
    ),
    jsonb_build_array(
        (ARRAY[
            '[MOCK] Clinic profile and hours',
            '[MOCK] Service catalogue',
            '[MOCK] Booking policy',
            '[MOCK] Price guidance',
            '[MOCK] Package guidance',
            '[MOCK] Visit preparation',
            '[MOCK] Follow-up continuity',
            '[MOCK] Payments and invoices',
            '[MOCK] Safety and escalation',
            '[MOCK] Brand voice'
        ])[(((seed.seq - 1) % 10) + 1)],
        (ARRAY[
            '[MOCK] Clinic profile and hours',
            '[MOCK] Service catalogue',
            '[MOCK] Booking policy',
            '[MOCK] Price guidance',
            '[MOCK] Package guidance',
            '[MOCK] Visit preparation',
            '[MOCK] Follow-up continuity',
            '[MOCK] Payments and invoices',
            '[MOCK] Safety and escalation',
            '[MOCK] Brand voice'
        ])[((seed.seq % 10) + 1)]
    ),
    jsonb_build_array('synthetic_demo', 'human_review_required', 'no_external_action'),
    jsonb_build_array(
        '[MOCK] Historical fixture result; no Qwen request was made.',
        'A human must verify facts before acting.'
    ),
    CASE WHEN seed.status = 'completed' THEN 450 + seed.seq * 3 ELSE 0 END,
    CASE WHEN seed.status = 'completed' THEN 90 + seed.seq * 2 ELSE 0 END,
    CASE WHEN seed.status = 'completed' THEN 600 + seed.seq * 17 ELSE 0 END,
    CASE seed.status
        WHEN 'failed' THEN 'MOCK_PROVIDER_UNAVAILABLE'
        WHEN 'blocked' THEN 'MOCK_SAFETY_REVIEW'
        WHEN 'cancelled' THEN 'MOCK_USER_CANCELLED'
        ELSE ''
    END,
    CASE seed.status
        WHEN 'failed' THEN '[MOCK] Historical simulated provider failure.'
        WHEN 'blocked' THEN '[MOCK] Historical simulated safety block.'
        WHEN 'cancelled' THEN '[MOCK] Historical simulated cancellation.'
        ELSE ''
    END,
    '[MOCK]-omnichannel-copilot-' || lpad(seed.seq::text, 3, '0'),
    seed.created_at + interval '180 days',
    1,
    seed.created_at,
    seed.created_at + CASE WHEN seed.status = 'completed'
        THEN interval '2 seconds'
        ELSE interval '1 second'
    END
FROM _omni_copilot_rows seed
CROSS JOIN _omni_ctx ctx
JOIN contacts contact
  ON contact.organization_id = ctx.organization_id
 AND contact.phone_number =
    '6000000000' || lpad(seed.contact_idx::text, 2, '0')
 AND contact.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    requested_by_id = EXCLUDED.requested_by_id,
    task_type = EXCLUDED.task_type,
    status = EXCLUDED.status,
    model = EXCLUDED.model,
    prompt_version = EXCLUDED.prompt_version,
    input_message_ids = EXCLUDED.input_message_ids,
    input_hash = EXCLUDED.input_hash,
    result_text = EXCLUDED.result_text,
    result_data = EXCLUDED.result_data,
    context_source_ids = EXCLUDED.context_source_ids,
    context_source_names = EXCLUDED.context_source_names,
    safety_labels = EXCLUDED.safety_labels,
    warnings = EXCLUDED.warnings,
    prompt_tokens = EXCLUDED.prompt_tokens,
    completion_tokens = EXCLUDED.completion_tokens,
    latency_ms = EXCLUDED.latency_ms,
    error_code = EXCLUDED.error_code,
    error_message = EXCLUDED.error_message,
    idempotency_key = EXCLUDED.idempotency_key,
    expires_at = EXCLUDED.expires_at,
    version = 1,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO copilot_feedback (
    id,
    organization_id,
    run_id,
    user_id,
    rating,
    accepted,
    final_message_id,
    edit_distance,
    reason,
    metadata,
    version,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-copilot-feedback-' || seed.seq)::uuid,
    ctx.organization_id,
    seed.id,
    ctx.owner_id,
    CASE WHEN seed.seq % 5 = 0 THEN 'not_helpful' ELSE 'helpful' END,
    CASE WHEN seed.seq % 3 = 0 THEN false ELSE true END,
    NULL,
    CASE WHEN seed.seq % 3 = 0 THEN 0.42000 ELSE 0.08000 END,
    CASE WHEN seed.seq % 5 = 0
        THEN '[MOCK] Needed more precise wording before human use.'
        ELSE '[MOCK] Useful synthetic draft after human review.'
    END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'historical_only', true,
        'message_sent', false,
        'reviewed_final_text',
            '[MOCK] Human-reviewed example only; no message was sent.'
    ),
    1,
    seed.created_at + interval '10 minutes',
    seed.created_at + interval '10 minutes'
FROM _omni_copilot_rows seed
CROSS JOIN _omni_ctx ctx
WHERE seed.seq <= 30
  AND seed.status = 'completed'
ON CONFLICT (id) DO UPDATE SET
    run_id = EXCLUDED.run_id,
    user_id = EXCLUDED.user_id,
    rating = EXCLUDED.rating,
    accepted = EXCLUDED.accepted,
    final_message_id = NULL,
    edit_distance = EXCLUDED.edit_distance,
    reason = EXCLUDED.reason,
    metadata = EXCLUDED.metadata,
    version = 1,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Eight local-only WhatsApp Flow drafts. No Meta ID, account credential, or
-- preview URL exists, so these are editable visual examples only.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_wa_flow_defs (
    seq integer PRIMARY KEY,
    id uuid NOT NULL,
    name text NOT NULL,
    category text NOT NULL,
    screen_prefix text NOT NULL,
    heading text NOT NULL,
    prompt text NOT NULL,
    field_label text NOT NULL
);

INSERT INTO _omni_wa_flow_defs VALUES
    (1, md5('klinik-relive-omnichannel-v1-wa-flow-1')::uuid,
        '[MOCK] Appointment Preferences', 'APPOINTMENT_BOOKING',
        'APPOINTMENT', 'Choose an appointment preference',
        'Tell us the preferred day and time for a human to review.',
        'Preferred date or time'),
    (2, md5('klinik-relive-omnichannel-v1-wa-flow-2')::uuid,
        '[MOCK] Wellness Goal Intake', 'LEAD_GENERATION',
        'WELLNESS', 'Share your wellness goal',
        'This synthetic intake helps demonstrate lead qualification.',
        'Primary goal'),
    (3, md5('klinik-relive-omnichannel-v1-wa-flow-3')::uuid,
        '[MOCK] Contact Request', 'CONTACT_US',
        'CONTACT', 'Request a callback',
        'No call will be scheduled; this is a local draft preview.',
        'Best callback window'),
    (4, md5('klinik-relive-omnichannel-v1-wa-flow-4')::uuid,
        '[MOCK] Care Support Request', 'CUSTOMER_SUPPORT',
        'SUPPORT', 'Describe the support needed',
        'Urgent or clinical concerns must be handled by clinic staff.',
        'Support topic'),
    (5, md5('klinik-relive-omnichannel-v1-wa-flow-5')::uuid,
        '[MOCK] Visit Feedback', 'SURVEY',
        'FEEDBACK', 'Tell us about the demo visit',
        'Synthetic feedback remains local to this rollback fixture.',
        'Feedback summary'),
    (6, md5('klinik-relive-omnichannel-v1-wa-flow-6')::uuid,
        '[MOCK] Patient Interest Registration', 'SIGN_UP',
        'REGISTRATION', 'Register an interest',
        'This does not create a real patient registration.',
        'Area of interest'),
    (7, md5('klinik-relive-omnichannel-v1-wa-flow-7')::uuid,
        '[MOCK] Returning Patient Check-in', 'SIGN_IN',
        'CHECKIN', 'Check in to the demo journey',
        'No identity verification or account access occurs.',
        'Reference note'),
    (8, md5('klinik-relive-omnichannel-v1-wa-flow-8')::uuid,
        '[MOCK] Package Renewal Review', 'OTHER',
        'RENEWAL', 'Review a renewal preference',
        'No package will be renewed or charged.',
        'Renewal preference');

CREATE TEMP TABLE _omni_wa_flow_rows AS
SELECT
    seed.*,
    jsonb_build_array(
        jsonb_build_object(
            'id', seed.screen_prefix || '_DETAILS',
            'title', '[MOCK] Details',
            'data', '{}'::jsonb,
            'layout', jsonb_build_object(
                'type', 'SingleColumnLayout',
                'children', jsonb_build_array(
                    jsonb_build_object(
                        'type', 'TextHeading',
                        'text', seed.heading
                    ),
                    jsonb_build_object(
                        'type', 'TextBody',
                        'text', seed.prompt
                    ),
                    jsonb_build_object(
                        'type', 'TextInput',
                        'name', 'full_name',
                        'label', 'Name',
                        'required', true,
                        'input-type', 'text'
                    ),
                    jsonb_build_object(
                        'type', 'TextArea',
                        'name', 'preference',
                        'label', seed.field_label,
                        'required', true
                    ),
                    jsonb_build_object(
                        'type', 'Dropdown',
                        'name', 'preferred_language',
                        'label', 'Preferred language',
                        'required', true,
                        'data-source', jsonb_build_array(
                            jsonb_build_object('id', 'english', 'title', 'English'),
                            jsonb_build_object('id', 'bahasa_melayu', 'title', 'Bahasa Melayu')
                        )
                    ),
                    jsonb_build_object(
                        'type', 'Footer',
                        'label', 'Review',
                        'on-click-action', jsonb_build_object(
                            'name', 'navigate',
                            'next', jsonb_build_object(
                                'type', 'screen',
                                'name', seed.screen_prefix || '_REVIEW'
                            ),
                            'payload', jsonb_build_object(
                                'full_name', '${form.full_name}',
                                'preference', '${form.preference}',
                                'preferred_language', '${form.preferred_language}'
                            )
                        )
                    )
                )
            )
        ),
        jsonb_build_object(
            'id', seed.screen_prefix || '_REVIEW',
            'title', '[MOCK] Review',
            'terminal', true,
            'success', true,
            'data', jsonb_build_object(
                'full_name', jsonb_build_object(
                    'type', 'string',
                    '__example__', '[MOCK] Example Patient'
                ),
                'preference', jsonb_build_object(
                    'type', 'string',
                    '__example__', '[MOCK] Weekday morning'
                ),
                'preferred_language', jsonb_build_object(
                    'type', 'string',
                    '__example__', 'English'
                )
            ),
            'layout', jsonb_build_object(
                'type', 'SingleColumnLayout',
                'children', jsonb_build_array(
                    jsonb_build_object(
                        'type', 'TextHeading',
                        'text', '[MOCK] Review your entry'
                    ),
                    jsonb_build_object(
                        'type', 'TextBody',
                        'text', 'This is synthetic local data. Submitting does not create an external action.'
                    ),
                    jsonb_build_object(
                        'type', 'Footer',
                        'label', 'Complete demo',
                        'on-click-action', jsonb_build_object(
                            'name', 'complete',
                            'payload', jsonb_build_object(
                                'full_name', '${data.full_name}',
                                'preference', '${data.preference}',
                                'preferred_language', '${data.preferred_language}',
                                'mock_dataset', 'klinik-relive-omnichannel-v1'
                            )
                        )
                    )
                )
            )
        )
    ) AS screens
FROM _omni_wa_flow_defs seed;

INSERT INTO whatsapp_flows (
    id,
    organization_id,
    whats_app_account,
    meta_flow_id,
    name,
    status,
    category,
    json_version,
    flow_json,
    screens,
    preview_url,
    has_local_changes,
    created_at,
    updated_at
)
SELECT
    seed.id,
    ctx.organization_id,
    '[MOCK] DEMO',
    '',
    seed.name,
    'DRAFT',
    seed.category,
    '6.0',
    jsonb_build_object(
        'version', '6.0',
        'screens', seed.screens,
        'metadata', jsonb_build_object(
            'mock_dataset', 'klinik-relive-omnichannel-v1',
            'local_only', true,
            'provider_calls_allowed', false
        )
    ),
    seed.screens,
    '',
    true,
    CURRENT_TIMESTAMP - make_interval(days => 45 - seed.seq),
    CURRENT_TIMESTAMP
FROM _omni_wa_flow_rows seed
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    whats_app_account = '[MOCK] DEMO',
    meta_flow_id = '',
    name = EXCLUDED.name,
    status = 'DRAFT',
    category = EXCLUDED.category,
    json_version = '6.0',
    flow_json = EXCLUDED.flow_json,
    screens = EXCLUDED.screens,
    preview_url = '',
    has_local_changes = true,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Eight rich chatbot v2 flow graphs. All are disabled. The diverse third node
-- demonstrates prompt, buttons, condition, timing, transfer, AI, WhatsApp
-- Flow, and API controls without making any runtime path executable.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_chat_flow_defs (
    seq integer PRIMARY KEY,
    id uuid NOT NULL,
    name text NOT NULL,
    description text NOT NULL,
    node_type text NOT NULL,
    captured_key text NOT NULL,
    node_label text NOT NULL
);

INSERT INTO _omni_chat_flow_defs VALUES
    (1, md5('klinik-relive-omnichannel-v1-chat-flow-1')::uuid,
        '[MOCK] Appointment Concierge',
        '[MOCK] Captures appointment preferences for human review.',
        'prompt', 'preferred_date', 'Capture preferred date'),
    (2, md5('klinik-relive-omnichannel-v1-chat-flow-2')::uuid,
        '[MOCK] Lead Qualification',
        '[MOCK] Demonstrates structured service-interest choices.',
        'buttons', 'service_interest', 'Choose an interest'),
    (3, md5('klinik-relive-omnichannel-v1-chat-flow-3')::uuid,
        '[MOCK] FAQ Router',
        '[MOCK] Demonstrates conditional routing with no live execution.',
        'condition', 'intent', 'Route by intent'),
    (4, md5('klinik-relive-omnichannel-v1-chat-flow-4')::uuid,
        '[MOCK] Follow-up Journey',
        '[MOCK] Demonstrates business-hours timing logic.',
        'timing', 'follow_up_window', 'Check service hours'),
    (5, md5('klinik-relive-omnichannel-v1-chat-flow-5')::uuid,
        '[MOCK] Human Care Handoff',
        '[MOCK] Demonstrates transfer design against an inactive mock team.',
        'transfer', 'handoff_reason', 'Transfer to care team'),
    (6, md5('klinik-relive-omnichannel-v1-chat-flow-6')::uuid,
        '[MOCK] Feedback Summarizer',
        '[MOCK] Demonstrates an AI-response node; flow remains disabled.',
        'ai_response', 'feedback_summary', 'Summarize feedback'),
    (7, md5('klinik-relive-omnichannel-v1-chat-flow-7')::uuid,
        '[MOCK] WhatsApp Intake Launcher',
        '[MOCK] Demonstrates linking to a local-only WhatsApp Flow draft.',
        'whatsapp_flow', 'intake_response', 'Open local intake'),
    (8, md5('klinik-relive-omnichannel-v1-chat-flow-8')::uuid,
        '[MOCK] CRM Lookup Demonstration',
        '[MOCK] Demonstrates a disabled API node using a .invalid host.',
        'api_call', 'crm_result', 'Run mock CRM lookup');

INSERT INTO chatbot_flows (
    id,
    organization_id,
    whats_app_account,
    name,
    is_enabled,
    description,
    trigger_keywords,
    trigger_button_id,
    initial_message,
    initial_message_type,
    initial_template_id,
    completion_message,
    on_complete_action,
    completion_config,
    timeout_message,
    cancel_keywords,
    panel_config,
    graph,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    seed.id,
    ctx.organization_id,
    '[MOCK] DEMO',
    seed.name,
    false,
    seed.description,
    jsonb_build_array('[MOCK:FLOW:' || seed.seq || ']'),
    '',
    '[MOCK] This flow is a disabled visual demonstration.',
    'text',
    NULL,
    '[MOCK] Demo flow complete. No external action was performed.',
    'none',
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'external_action_allowed', false
    ),
    '[MOCK] Demo session ended safely.',
    jsonb_build_array('[MOCK:CANCEL]'),
    jsonb_build_object(
        'sections', jsonb_build_array(
            jsonb_build_object(
                'id', 'mock_intake',
                'label', '[MOCK] Intake summary',
                'columns', 2,
                'collapsible', true,
                'default_collapsed', false,
                'order', 1,
                'fields', jsonb_build_array(
                    jsonb_build_object(
                        'key', seed.captured_key,
                        'label', seed.node_label,
                        'order', 1,
                        'display_type', 'text',
                        'color', 'info'
                    ),
                    jsonb_build_object(
                        'key', 'outcome',
                        'label', 'Demo outcome',
                        'order', 2,
                        'display_type', 'badge',
                        'color', 'success'
                    )
                )
            )
        )
    ),
    jsonb_build_object(
        'version', 2,
        'entry_node', 'start',
        'nodes', jsonb_build_array(
            jsonb_build_object(
                'id', 'start',
                'type', 'start',
                'label', 'Start',
                'position', jsonb_build_object('x', 80, 'y', 160),
                'config', '{}'::jsonb
            ),
            jsonb_build_object(
                'id', 'welcome',
                'type', 'message',
                'label', '[MOCK] Welcome',
                'position', jsonb_build_object('x', 320, 'y', 160),
                'config', jsonb_build_object(
                    'message', '[MOCK] Welcome to the synthetic Klinik Relive journey. A human reviews every next step.'
                )
            ),
            jsonb_build_object(
                'id', 'demo_action',
                'type', seed.node_type,
                'label', seed.node_label,
                'position', jsonb_build_object('x', 560, 'y', 160),
                'config', CASE seed.node_type
                    WHEN 'prompt' THEN jsonb_build_object(
                        'body', '[MOCK] What day or time do you prefer?',
                        'store_as', seed.captured_key,
                        'validation_regex', '^.{2,80}$',
                        'validation_error', '[MOCK] Enter a short preference.',
                        'max_retries', 2
                    )
                    WHEN 'buttons' THEN jsonb_build_object(
                        'body', '[MOCK] Which option is closest to your interest?',
                        'store_as', seed.captured_key,
                        'buttons', jsonb_build_array(
                            jsonb_build_object('id', 'assessment', 'title', 'Assessment'),
                            jsonb_build_object('id', 'follow_up', 'title', 'Follow-up'),
                            jsonb_build_object('id', 'packages', 'title', 'Packages')
                        )
                    )
                    WHEN 'condition' THEN jsonb_build_object(
                        'expression', 'session_data.intent == "appointment"',
                        'mock_only', true
                    )
                    WHEN 'timing' THEN jsonb_build_object(
                        'schedule', jsonb_build_array(
                            jsonb_build_object('day', 'monday', 'enabled', true, 'start_time', '09:00', 'end_time', '18:00'),
                            jsonb_build_object('day', 'tuesday', 'enabled', true, 'start_time', '09:00', 'end_time', '18:00'),
                            jsonb_build_object('day', 'wednesday', 'enabled', true, 'start_time', '09:00', 'end_time', '18:00'),
                            jsonb_build_object('day', 'thursday', 'enabled', true, 'start_time', '09:00', 'end_time', '18:00'),
                            jsonb_build_object('day', 'friday', 'enabled', true, 'start_time', '09:00', 'end_time', '18:00'),
                            jsonb_build_object('day', 'saturday', 'enabled', true, 'start_time', '09:00', 'end_time', '13:00'),
                            jsonb_build_object('day', 'sunday', 'enabled', false, 'start_time', '09:00', 'end_time', '18:00')
                        )
                    )
                    WHEN 'transfer' THEN jsonb_build_object(
                        'body', '[MOCK] A human handoff would be requested here.',
                        'team_id', md5('klinik-relive-omnichannel-v1-team')::uuid::text,
                        'notes', '[MOCK] Inactive synthetic team; do not assign.'
                    )
                    WHEN 'ai_response' THEN jsonb_build_object(
                        'instruction', '[MOCK] Summarize the feedback without clinical advice or completed-action claims.',
                        'store_as', seed.captured_key,
                        'human_review_required', true
                    )
                    WHEN 'whatsapp_flow' THEN jsonb_build_object(
                        'flow_id', md5('klinik-relive-omnichannel-v1-wa-flow-1')::uuid::text,
                        'header', '[MOCK] Appointment intake',
                        'body', '[MOCK] Open the local-only draft preview.',
                        'cta', 'Open demo'
                    )
                    ELSE jsonb_build_object(
                        'url', 'https://crm.klinik-relive.example.invalid/mock/lookup',
                        'method', 'GET',
                        'headers', '{}'::jsonb,
                        'body', '',
                        'response_mapping', jsonb_build_object(
                            seed.captured_key, '$.mock_result'
                        ),
                        'message_template', '[MOCK] CRM result: {{crm_result}}'
                    )
                END
            ),
            jsonb_build_object(
                'id', 'summary',
                'type', 'message',
                'label', '[MOCK] Summary',
                'position', jsonb_build_object('x', 800, 'y', 160),
                'config', jsonb_build_object(
                    'message', '[MOCK] Your preference is stored for demonstration only. No booking, transfer, AI call, or API call occurred.'
                )
            ),
            jsonb_build_object(
                'id', 'end',
                'type', 'end',
                'label', 'End',
                'position', jsonb_build_object('x', 1040, 'y', 160),
                'config', jsonb_build_object(
                    'message', '[MOCK] Journey complete.'
                )
            )
        ),
        'edges', jsonb_build_array(
            jsonb_build_object('from', 'start', 'to', 'welcome', 'condition', 'default'),
            jsonb_build_object('from', 'welcome', 'to', 'demo_action', 'condition', 'default'),
            jsonb_build_object(
                'from', 'demo_action',
                'to', 'summary',
                'condition', CASE seed.node_type
                    WHEN 'buttons' THEN 'button:assessment'
                    WHEN 'condition' THEN 'default'
                    WHEN 'timing' THEN 'in_hours'
                    WHEN 'api_call' THEN 'http:2xx'
                    ELSE 'default'
                END
            ),
            jsonb_build_object('from', 'summary', 'to', 'end', 'condition', 'default')
        )
    ),
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => 55 - seed.seq),
    CURRENT_TIMESTAMP
FROM _omni_chat_flow_defs seed
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    whats_app_account = '[MOCK] DEMO',
    name = EXCLUDED.name,
    is_enabled = false,
    description = EXCLUDED.description,
    trigger_keywords = EXCLUDED.trigger_keywords,
    trigger_button_id = '',
    initial_message = EXCLUDED.initial_message,
    initial_message_type = EXCLUDED.initial_message_type,
    initial_template_id = NULL,
    completion_message = EXCLUDED.completion_message,
    on_complete_action = 'none',
    completion_config = EXCLUDED.completion_config,
    timeout_message = EXCLUDED.timeout_message,
    cancel_keywords = EXCLUDED.cancel_keywords,
    panel_config = EXCLUDED.panel_config,
    graph = EXCLUDED.graph,
    created_by_id = EXCLUDED.created_by_id,
    updated_by_id = EXCLUDED.updated_by_id,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- The session rows were inserted with current_flow_id = NULL because this file
-- creates the referenced disabled flow graphs later. Link them only after the
-- graph rows exist, preserving FK-safe statement order.
UPDATE chatbot_sessions session
SET
    current_flow_id = md5('klinik-relive-omnichannel-v1-chat-flow-' ||
        (((map.seq - 1) % 8) + 1))::uuid,
    current_step = 'end',
    updated_at = CURRENT_TIMESTAMP
FROM _omni_session_map map
WHERE session.id = map.session_id
  AND session.organization_id = (SELECT organization_id FROM _omni_ctx)
  AND session.status IN ('completed', 'timeout', 'cancelled')
  AND EXISTS (
      SELECT 1
      FROM chatbot_flows flow
      WHERE flow.id = md5('klinik-relive-omnichannel-v1-chat-flow-' ||
            (((map.seq - 1) % 8) + 1))::uuid
        AND flow.organization_id = session.organization_id
        AND flow.is_enabled = false
        AND flow.deleted_at IS NULL
  );

-- ---------------------------------------------------------------------------
-- Copilot grounding library. Enabled sources are static and strictly scoped to
-- "[MOCK] DEMO"; the only API examples are disabled and use .invalid hosts.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_context_defs (
    seq integer PRIMARY KEY,
    name text NOT NULL,
    context_type text NOT NULL,
    is_enabled boolean NOT NULL,
    trigger_keywords jsonb NOT NULL,
    static_content text NOT NULL,
    api_config jsonb NOT NULL
);

INSERT INTO _omni_context_defs VALUES
    (1, '[MOCK] Clinic profile and hours', 'static', true,
        '["hours","location","open"]'::jsonb,
        '[MOCK] Synthetic showcase knowledge: clinic hours are Monday-Friday 09:00-18:00 and Saturday 09:00-13:00. Verify all real operating details with Klinik Relive staff.',
        '{}'::jsonb),
    (2, '[MOCK] Service catalogue', 'static', true,
        '["service","assessment","follow-up"]'::jsonb,
        '[MOCK] Demo services include an initial assessment, structured follow-up, preventive wellness review, and package progress review. Never diagnose or prescribe.',
        '{}'::jsonb),
    (3, '[MOCK] Booking policy', 'static', true,
        '["book","appointment","reschedule"]'::jsonb,
        '[MOCK] A booking is not confirmed until a human agent verifies availability. Copilot must never claim that an appointment was created, changed, or cancelled.',
        '{}'::jsonb),
    (4, '[MOCK] Price guidance', 'static', true,
        '["price","cost","budget"]'::jsonb,
        '[MOCK] All displayed price bands are synthetic. A human must confirm current pricing, inclusions, tax treatment, discounts, and refund terms.',
        '{}'::jsonb),
    (5, '[MOCK] Package guidance', 'static', true,
        '["package","sessions","renew"]'::jsonb,
        '[MOCK] Demo package choices include four-session follow-up care and six-session metabolic wellness. No package purchase, renewal, or entitlement can be completed by Copilot.',
        '{}'::jsonb),
    (6, '[MOCK] Visit preparation', 'static', true,
        '["prepare","before visit","bring"]'::jsonb,
        '[MOCK] Provide only general administrative preparation reminders. Clinical preparation, medication, fasting, and suitability questions must be escalated to a practitioner.',
        '{}'::jsonb),
    (7, '[MOCK] Follow-up continuity', 'static', true,
        '["follow-up","same practitioner","progress"]'::jsonb,
        '[MOCK] Capture preferred day, time, practitioner continuity, unresolved questions, and next human action. Do not promise practitioner availability.',
        '{}'::jsonb),
    (8, '[MOCK] Payments and invoices', 'static', true,
        '["payment","invoice","refund"]'::jsonb,
        '[MOCK] Demo payment methods are card and bank transfer. Never state that a payment, refund, invoice, or payment link was completed.',
        '{}'::jsonb),
    (9, '[MOCK] Safety and escalation', 'static', true,
        '["urgent","pain","reaction","unsafe"]'::jsonb,
        '[MOCK] If a message suggests urgent health or safety risk, stop routine drafting and direct the human agent to follow the clinic emergency escalation process immediately.',
        '{}'::jsonb),
    (10, '[MOCK] Brand voice', 'static', true,
        '["tone","reply","language"]'::jsonb,
        '[MOCK] Use clear, warm, concise Malaysian English or Bahasa Melayu as requested. Avoid medical claims, pressure tactics, invented facts, and promises of completed actions.',
        '{}'::jsonb),
    (11, '[MOCK] Disabled CRM enrichment API', 'api', false,
        '["enrichment"]'::jsonb,
        '',
        '{"url":"https://crm.klinik-relive.example.invalid/mock/context","method":"GET","headers":{},"body":{},"mock_only":true}'::jsonb),
    (12, '[MOCK] Disabled package inventory API', 'api', false,
        '["inventory"]'::jsonb,
        '',
        '{"url":"https://packages.klinik-relive.example.invalid/mock/context","method":"POST","headers":{},"body":{"synthetic":true},"mock_only":true}'::jsonb);

INSERT INTO ai_contexts (
    id,
    organization_id,
    whats_app_account,
    name,
    is_enabled,
    priority,
    context_type,
    trigger_keywords,
    static_content,
    api_config,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-ai-context-' || seed.seq)::uuid,
    ctx.organization_id,
    '[MOCK] DEMO',
    seed.name,
    seed.is_enabled,
    seed.seq * 10,
    seed.context_type,
    seed.trigger_keywords,
    seed.static_content,
    seed.api_config,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => 60 - seed.seq),
    CURRENT_TIMESTAMP
FROM _omni_context_defs seed
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    whats_app_account = '[MOCK] DEMO',
    name = EXCLUDED.name,
    is_enabled = EXCLUDED.is_enabled,
    priority = EXCLUDED.priority,
    context_type = EXCLUDED.context_type,
    trigger_keywords = EXCLUDED.trigger_keywords,
    static_content = EXCLUDED.static_content,
    api_config = EXCLUDED.api_config,
    created_by_id = EXCLUDED.created_by_id,
    updated_by_id = EXCLUDED.updated_by_id,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- 135 inert historical messages:
--   * 90 in the current month (15 per channel)
--   * 45 in the previous month
--   * 90 incoming / 45 outgoing overall
-- Each envelope has one ready message part and one terminal historical event.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_message_seed AS
SELECT
    ((c.seq - 1) * 3) + p.position AS message_seq,
    c.seq AS conversation_seq,
    'current'::text AS period_key,
    p.position,
    LEAST(
        date_trunc('month', CURRENT_TIMESTAMP)
            + make_interval(
                -- Keep each conversation's three-message sequence on one day
                -- so the read cursor always falls between positions 2 and 3.
                days => 1 + ((c.seq - 1) % 28),
                hours => 8 + (p.position * 2),
                mins => c.seq % 40
              ),
        CURRENT_TIMESTAMP - interval '5 minutes'
    ) AS occurred_at
FROM _omni_conversations c
CROSS JOIN generate_series(1, 3) AS p(position)
UNION ALL
SELECT
    90 + ((c.seq - 1) * 3) + p.position AS message_seq,
    c.seq AS conversation_seq,
    'previous'::text AS period_key,
    p.position,
    date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
        + make_interval(
            days => 1 + ((c.seq * 3 + p.position) % 20),
            hours => 8 + (p.position * 2),
            mins => c.seq % 40
          ) AS occurred_at
FROM _omni_conversations c
CROSS JOIN generate_series(1, 3) AS p(position)
WHERE c.seq <= 15;

CREATE TEMP TABLE _omni_message_rows AS
WITH typed AS (
    SELECT
        seed.*,
        c.id AS conversation_id,
        c.contact_id,
        c.account_id,
        c.account_name,
        c.channel,
        c.agent_id,
        CASE WHEN seed.position = 2 THEN 'outgoing' ELSE 'incoming' END AS direction,
        CASE (seed.message_seq - 1) % 10
            WHEN 0 THEN 'text'
            WHEN 1 THEN 'interactive'
            WHEN 2 THEN 'flow'
            WHEN 3 THEN 'template'
            WHEN 4 THEN 'location'
            WHEN 5 THEN 'contact'
            WHEN 6 THEN 'text'
            WHEN 7 THEN 'text'
            WHEN 8 THEN 'interactive'
            ELSE 'text'
        END AS message_type
    FROM _omni_message_seed seed
    JOIN _omni_conversations c ON c.seq = seed.conversation_seq
)
SELECT
    typed.*,
    md5('klinik-relive-omnichannel-v1-message-' || typed.message_seq)::uuid AS message_id,
    'MOCK-OMNI-MSG-' || lpad(typed.message_seq::text, 4, '0') AS external_message_id,
    CASE
        WHEN typed.direction = 'incoming' THEN 'received'
        WHEN typed.conversation_seq % 3 = 0 THEN 'sent'
        WHEN typed.conversation_seq % 3 = 1 THEN 'delivered'
        ELSE 'read'
    END AS message_status,
    CASE typed.position
        WHEN 1 THEN CASE typed.conversation_seq % 6
            WHEN 0 THEN '[MOCK] I would like to compare the available wellness packages.'
            WHEN 1 THEN '[MOCK] Could you share the next initial-assessment slots?'
            WHEN 2 THEN '[MOCK] I am following up after my recent appointment.'
            WHEN 3 THEN '[MOCK] Can you explain the preparation steps for my visit?'
            WHEN 4 THEN '[MOCK] I would like help choosing a suitable follow-up time.'
            ELSE '[MOCK] Please tell me what information you need to arrange a callback.'
        END
        WHEN 2 THEN CASE typed.conversation_seq % 6
            WHEN 0 THEN '[MOCK] Here is a human-reviewed comparison summary for your demo record.'
            WHEN 1 THEN '[MOCK] I have listed synthetic appointment options for review.'
            WHEN 2 THEN '[MOCK] Your demo follow-up notes are ready for a care-team review.'
            WHEN 3 THEN '[MOCK] These are general demo preparation notes, not medical advice.'
            WHEN 4 THEN '[MOCK] I have recorded your preferred follow-up window.'
            ELSE '[MOCK] A synthetic callback task is noted; no real call was scheduled.'
        END
        ELSE CASE typed.conversation_seq % 6
            WHEN 0 THEN '[MOCK] Thank you. The six-session option looks closest to my goal.'
            WHEN 1 THEN '[MOCK] A weekday morning would work best for me.'
            WHEN 2 THEN '[MOCK] Please keep the same practitioner if possible.'
            WHEN 3 THEN '[MOCK] Understood. I will bring the requested information.'
            WHEN 4 THEN '[MOCK] Saturday afternoon is my second preference.'
            ELSE '[MOCK] Please have a human agent review this before taking action.'
        END
    END AS content
FROM typed;

DO $$
BEGIN
    IF (SELECT count(*) FROM _omni_message_rows) <> 135 THEN
        RAISE EXCEPTION 'Message plan did not produce exactly 135 rows';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM messages m
        JOIN _omni_message_rows seed ON seed.message_id = m.id
        CROSS JOIN _omni_ctx ctx
        WHERE m.organization_id <> ctx.organization_id
           OR COALESCE(m.metadata->>'mock_dataset', '') NOT IN (
                '',
                'klinik-relive-omnichannel-v1'
           )
    ) THEN
        RAISE EXCEPTION 'A deterministic message ID is owned by unrelated data';
    END IF;
END $$;

INSERT INTO messages (
    id,
    organization_id,
    whats_app_account,
    contact_id,
    whats_app_message_id,
    conversation_id,
    inbox_conversation_id,
    direction,
    message_type,
    content,
    template_name,
    template_params,
    interactive_data,
    flow_response,
    status,
    error_message,
    is_reply,
    reply_to_message_id,
    sent_by_user_id,
    metadata,
    created_at,
    updated_at
)
SELECT
    row.message_id,
    ctx.organization_id,
    row.account_name,
    row.contact_id,
    row.external_message_id,
    'MOCK-OMNI-CONV-' || lpad(row.conversation_seq::text, 3, '0'),
    row.conversation_id,
    row.direction,
    row.message_type,
    row.content,
    CASE WHEN row.message_type = 'template'
        THEN 'mock_omnichannel_follow_up'
        ELSE ''
    END,
    CASE WHEN row.message_type = 'template'
        THEN jsonb_build_object(
            'language', 'en',
            'parameters', jsonb_build_array(
                '[MOCK] ' || row.channel,
                'human review required'
            )
        )
        ELSE '{}'::jsonb
    END,
    CASE WHEN row.message_type = 'interactive'
        THEN jsonb_build_object(
            'type', 'button_reply',
            'button_id', 'mock_option_' || (row.conversation_seq % 3 + 1),
            'button_title', CASE row.conversation_seq % 3
                WHEN 0 THEN 'View packages'
                WHEN 1 THEN 'Choose a slot'
                ELSE 'Ask an agent'
            END,
            'synthetic', true
        )
        ELSE '{}'::jsonb
    END,
    CASE WHEN row.message_type = 'flow'
        THEN jsonb_build_object(
            'flow_name', '[MOCK] Appointment Preference',
            'screen', 'MOCK_CONFIRMATION',
            'response', jsonb_build_object(
                'preferred_day', CASE WHEN row.conversation_seq % 2 = 0
                    THEN 'Weekday'
                    ELSE 'Saturday'
                END,
                'preferred_time', CASE WHEN row.conversation_seq % 3 = 0
                    THEN 'Morning'
                    ELSE 'Afternoon'
                END,
                'consent', 'synthetic_demo_only'
            )
        )
        ELSE '{}'::jsonb
    END,
    row.message_status,
    '',
    row.position > 1,
    CASE WHEN row.position > 1
        THEN md5('klinik-relive-omnichannel-v1-message-' ||
            (row.message_seq - 1))::uuid
        ELSE NULL
    END,
    CASE WHEN row.direction = 'outgoing' THEN row.agent_id ELSE NULL END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'historical_only', true,
        'delivery_attempted', false,
        'provider_request_created', false,
        'channel', row.channel,
        'period', row.period_key,
        'source', 'synthetic_omnichannel_history'
    ),
    row.occurred_at,
    row.occurred_at + interval '2 minutes'
FROM _omni_message_rows row
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    whats_app_account = EXCLUDED.whats_app_account,
    contact_id = EXCLUDED.contact_id,
    whats_app_message_id = EXCLUDED.whats_app_message_id,
    conversation_id = EXCLUDED.conversation_id,
    inbox_conversation_id = EXCLUDED.inbox_conversation_id,
    direction = EXCLUDED.direction,
    message_type = EXCLUDED.message_type,
    content = EXCLUDED.content,
    template_name = EXCLUDED.template_name,
    template_params = EXCLUDED.template_params,
    interactive_data = EXCLUDED.interactive_data,
    flow_response = EXCLUDED.flow_response,
    status = EXCLUDED.status,
    error_message = '',
    is_reply = EXCLUDED.is_reply,
    reply_to_message_id = EXCLUDED.reply_to_message_id,
    sent_by_user_id = EXCLUDED.sent_by_user_id,
    metadata = EXCLUDED.metadata,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO message_parts (
    id,
    organization_id,
    message_id,
    conversation_id,
    position,
    type,
    status,
    text,
    caption,
    media_url,
    storage_key,
    provider_media_ref,
    mime_type,
    filename,
    size_bytes,
    checksum,
    payload,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-message-part-' || row.message_seq)::uuid,
    ctx.organization_id,
    row.message_id,
    row.conversation_id,
    0,
    CASE row.message_type
        WHEN 'flow' THEN 'interactive'
        ELSE row.message_type
    END,
    'ready',
    row.content,
    CASE WHEN row.message_type IN ('image', 'video', 'document')
        THEN '[MOCK] Historical media placeholder'
        ELSE ''
    END,
    '',
    '',
    '',
    '',
    '',
    0,
    '',
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'envelope_type', row.message_type,
        'terminal_history', true,
        'location', CASE WHEN row.message_type = 'location'
            THEN jsonb_build_object(
                'latitude', 3.1390,
                'longitude', 101.6869,
                'label', '[MOCK] Kuala Lumpur'
            )
            ELSE NULL
        END,
        'contact_card', CASE WHEN row.message_type = 'contact'
            THEN jsonb_build_object(
                'name', '[MOCK] Klinik Relive Care Desk',
                'phone', '+600000000000'
            )
            ELSE NULL
        END
    ),
    row.occurred_at,
    row.occurred_at + interval '2 minutes'
FROM _omni_message_rows row
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    message_id = EXCLUDED.message_id,
    conversation_id = EXCLUDED.conversation_id,
    position = 0,
    type = EXCLUDED.type,
    status = 'ready',
    text = EXCLUDED.text,
    caption = EXCLUDED.caption,
    media_url = '',
    storage_key = '',
    provider_media_ref = '',
    mime_type = '',
    filename = '',
    size_bytes = 0,
    checksum = '',
    payload = EXCLUDED.payload,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO message_events (
    id,
    organization_id,
    channel_account_id,
    conversation_id,
    message_id,
    provider_event_id,
    external_message_id,
    type,
    occurred_at,
    actor_external_id,
    error_code,
    error_message,
    payload,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-message-event-' || row.message_seq)::uuid,
    ctx.organization_id,
    row.account_id,
    row.conversation_id,
    row.message_id,
    'mock-terminal-event-' || lpad(row.message_seq::text, 4, '0'),
    row.external_message_id,
    row.message_status,
    row.occurred_at + interval '2 minutes',
    CASE WHEN row.direction = 'incoming'
        THEN 'mock-contact-' || lpad(row.conversation_seq::text, 3, '0')
        ELSE row.agent_id::text
    END,
    '',
    '',
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'terminal', true,
        'historical_only', true,
        'provider_callback_received', false,
        'dispatch_job_created', false
    ),
    row.occurred_at + interval '2 minutes',
    row.occurred_at + interval '2 minutes'
FROM _omni_message_rows row
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    channel_account_id = EXCLUDED.channel_account_id,
    conversation_id = EXCLUDED.conversation_id,
    message_id = EXCLUDED.message_id,
    provider_event_id = EXCLUDED.provider_event_id,
    external_message_id = EXCLUDED.external_message_id,
    type = EXCLUDED.type,
    occurred_at = EXCLUDED.occurred_at,
    actor_external_id = EXCLUDED.actor_external_id,
    error_code = '',
    error_message = '',
    payload = EXCLUDED.payload,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

UPDATE inbox_conversations conversation
SET
    last_message_preview = activity.last_message_preview,
    last_message_at = activity.last_message_at,
    last_inbound_at = activity.last_inbound_at,
    last_outbound_at = activity.last_outbound_at,
    service_window_ends_at = activity.last_inbound_at + interval '24 hours',
    updated_at = CURRENT_TIMESTAMP
FROM (
    SELECT
        row.conversation_id,
        max(row.occurred_at) AS last_message_at,
        max(row.occurred_at) FILTER (
            WHERE row.direction = 'incoming'
        ) AS last_inbound_at,
        max(row.occurred_at) FILTER (
            WHERE row.direction = 'outgoing'
        ) AS last_outbound_at,
        (array_agg(row.content ORDER BY row.occurred_at DESC))[1] AS last_message_preview
    FROM _omni_message_rows row
    GROUP BY row.conversation_id
) activity
WHERE conversation.id = activity.conversation_id
  AND conversation.organization_id = (SELECT organization_id FROM _omni_ctx);

UPDATE channel_accounts account
SET
    last_inbound_at = activity.last_inbound_at,
    last_outbound_at = activity.last_outbound_at,
    updated_at = CURRENT_TIMESTAMP
FROM (
    SELECT
        row.account_id,
        max(row.occurred_at) FILTER (
            WHERE row.direction = 'incoming'
        ) AS last_inbound_at,
        max(row.occurred_at) FILTER (
            WHERE row.direction = 'outgoing'
        ) AS last_outbound_at
    FROM _omni_message_rows row
    GROUP BY row.account_id
) activity
WHERE account.id = activity.account_id
  AND account.organization_id = (SELECT organization_id FROM _omni_ctx);

UPDATE contacts contact
SET
    whats_app_account = '[MOCK] DEMO',
    last_message_at = activity.last_message_at,
    last_inbound_at = activity.last_inbound_at,
    last_message_preview = activity.last_message_preview,
    is_read = activity.conversation_seq > 12,
    metadata = COALESCE(contact.metadata, '{}'::jsonb) || jsonb_build_object(
        'omnichannel_mock_dataset', 'klinik-relive-omnichannel-v1',
        'omnichannel_channel', activity.channel,
        'omnichannel_account', activity.account_name,
        'copilot_context_scope', '[MOCK] DEMO'
    ),
    updated_at = CURRENT_TIMESTAMP
FROM (
    SELECT
        row.contact_id,
        row.conversation_seq,
        row.channel,
        row.account_name,
        max(row.occurred_at) AS last_message_at,
        max(row.occurred_at) FILTER (
            WHERE row.direction = 'incoming'
        ) AS last_inbound_at,
        (array_agg(row.content ORDER BY row.occurred_at DESC))[1] AS last_message_preview
    FROM _omni_message_rows row
    GROUP BY
        row.contact_id,
        row.conversation_seq,
        row.channel,
        row.account_name
) activity
WHERE contact.id = activity.contact_id
  AND contact.organization_id = (SELECT organization_id FROM _omni_ctx)
  AND contact.metadata->>'mock_dataset' = 'klinik-relive-crm-v2';

INSERT INTO conversation_reads (
    id,
    organization_id,
    conversation_id,
    participant_id,
    user_id,
    reader_key,
    last_read_message_id,
    last_read_external_id,
    last_read_at,
    metadata,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-read-' || c.seq)::uuid,
    ctx.organization_id,
    c.id,
    NULL,
    ctx.owner_id,
    'user:' || ctx.owner_id::text,
    row.message_id,
    row.external_message_id,
    row.occurred_at + interval '30 seconds',
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-omnichannel-v1',
        'is_mock', true,
        'expected_unread_after_cursor', CASE WHEN c.seq <= 12 THEN 1 ELSE 0 END
    ),
    row.occurred_at + interval '30 seconds',
    CURRENT_TIMESTAMP
FROM _omni_conversations c
CROSS JOIN _omni_ctx ctx
JOIN _omni_message_rows row
  ON row.conversation_seq = c.seq
 AND row.period_key = 'current'
 AND row.position = CASE WHEN c.seq <= 12 THEN 2 ELSE 3 END
ON CONFLICT (id) DO UPDATE SET
    conversation_id = EXCLUDED.conversation_id,
    participant_id = NULL,
    user_id = EXCLUDED.user_id,
    reader_key = EXCLUDED.reader_key,
    last_read_message_id = EXCLUDED.last_read_message_id,
    last_read_external_id = EXCLUDED.last_read_external_id,
    last_read_at = EXCLUDED.last_read_at,
    metadata = EXCLUDED.metadata,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

-- ---------------------------------------------------------------------------
-- Inert activity history for detail panels. These rows describe this fixture;
-- they are not customer activity events and cannot trigger automation.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_audit_resources AS
SELECT
    seed.seq,
    'keyword_rule'::text AS resource_type,
    md5('klinik-relive-omnichannel-v1-keyword-' || seed.seq)::uuid AS resource_id,
    seed.name AS resource_name,
    'disabled mock-scoped rule'::text AS operational_state
FROM _omni_keyword_defs seed
UNION ALL
SELECT
    seed.seq,
    'chatbot_flow',
    seed.id,
    seed.name,
    'disabled v2 graph'
FROM _omni_chat_flow_defs seed
UNION ALL
SELECT
    seed.seq,
    'ai_context',
    md5('klinik-relive-omnichannel-v1-ai-context-' || seed.seq)::uuid,
    seed.name,
    CASE WHEN seed.is_enabled
        THEN 'enabled static context scoped to [MOCK] DEMO'
        ELSE 'disabled API context on .invalid host'
    END
FROM _omni_context_defs seed
UNION ALL
SELECT
    seed.seq,
    'whatsapp_flow',
    seed.id,
    seed.name,
    'local DRAFT without Meta ID'
FROM _omni_wa_flow_defs seed;

INSERT INTO audit_logs (
    id,
    organization_id,
    resource_type,
    resource_id,
    user_id,
    user_name,
    action,
    changes,
    created_at
)
SELECT
    md5('klinik-relive-omnichannel-v1-audit-' ||
        resource.resource_type || '-' || resource.seq)::uuid,
    ctx.organization_id,
    resource.resource_type,
    resource.resource_id,
    ctx.owner_id,
    ctx.owner_name,
    'created',
    jsonb_build_array(
        jsonb_build_object(
            'field', 'name',
            'old_value', NULL,
            'new_value', resource.resource_name
        ),
        jsonb_build_object(
            'field', 'operational_state',
            'old_value', NULL,
            'new_value', resource.operational_state
        ),
        jsonb_build_object(
            'field', 'mock_dataset',
            'old_value', NULL,
            'new_value', 'klinik-relive-omnichannel-v1'
        ),
        jsonb_build_object(
            'field', 'external_action_performed',
            'old_value', NULL,
            'new_value', false
        )
    ),
    CURRENT_TIMESTAMP - make_interval(
        days => 20 - LEAST(resource.seq, 19),
        mins => resource.seq
    )
FROM _omni_audit_resources resource
CROSS JOIN _omni_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    organization_id = EXCLUDED.organization_id,
    resource_type = EXCLUDED.resource_type,
    resource_id = EXCLUDED.resource_id,
    user_id = EXCLUDED.user_id,
    user_name = EXCLUDED.user_name,
    action = 'created',
    changes = EXCLUDED.changes,
    created_at = EXCLUDED.created_at;

-- ---------------------------------------------------------------------------
-- Assertions and safety fence.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _omni_safety_after AS
SELECT
    (SELECT count(*) FROM outbox_jobs
      WHERE organization_id = ctx.organization_id) AS outbox_jobs,
    (SELECT count(*) FROM inbound_events
      WHERE organization_id = ctx.organization_id) AS inbound_events,
    (SELECT count(*) FROM automation_event_receipts
      WHERE organization_id = ctx.organization_id) AS automation_event_receipts,
    (SELECT count(*) FROM scheduled_jobs
      WHERE organization_id = ctx.organization_id) AS scheduled_jobs,
    (SELECT count(*) FROM outbox_events
      WHERE organization_id = ctx.organization_id) AS outbox_events,
    (SELECT count(*) FROM customer_activity_events
      WHERE organization_id = ctx.organization_id) AS customer_activity_events
FROM _omni_ctx ctx;

DO $$
DECLARE
    org uuid := 'c73f761f-5154-4fe1-9a13-06bae570277a';
    owner uuid := (SELECT owner_id FROM _omni_ctx);
    actual bigint;
    current_mock_messages bigint;
    previous_mock_messages bigint;
    current_sessions bigint;
    previous_sessions bigint;
    active_contacts bigint;
    unread_messages bigint;
BEGIN
    IF EXISTS (
        SELECT 1
        FROM _omni_safety_before before_counts
        CROSS JOIN _omni_safety_after after_counts
        WHERE before_counts.outbox_jobs <> after_counts.outbox_jobs
           OR before_counts.inbound_events <> after_counts.inbound_events
           OR before_counts.automation_event_receipts <>
                after_counts.automation_event_receipts
           OR before_counts.scheduled_jobs <> after_counts.scheduled_jobs
           OR before_counts.outbox_events <> after_counts.outbox_events
           OR before_counts.customer_activity_events <>
                after_counts.customer_activity_events
    ) THEN
        RAISE EXCEPTION
            'SAFETY FAILURE: a dispatch, webhook, scheduled, automation, outbox, or customer activity table changed';
    END IF;

    SELECT count(*) INTO actual
    FROM channel_accounts account
    JOIN _omni_accounts seed ON seed.id = account.id
    WHERE account.organization_id = org
      AND account.deleted_at IS NULL
      AND account.provider = 'mock_fixture'
      AND account.config->>'outbound_enabled' = 'false'
      AND account.config->>'inbound_enabled' = 'false'
      AND account.config->>'ai_reply_enabled' = 'false'
      AND account.is_default_incoming = false
      AND account.is_default_outgoing = false;
    IF actual <> 6 THEN
        RAISE EXCEPTION 'Expected 6 credential-free disabled channel accounts, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM channel_credentials credential
    JOIN _omni_accounts account ON account.id = credential.channel_account_id
    WHERE credential.organization_id = org;
    IF actual <> 0 THEN
        RAISE EXCEPTION 'Mock channel accounts unexpectedly have % credential rows', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM contact_identities identity
    JOIN _omni_contacts contact
      ON identity.id =
        md5('klinik-relive-omnichannel-v1-identity-' || contact.seq)::uuid
    WHERE identity.organization_id = org
      AND identity.deleted_at IS NULL;
    IF actual <> 30 THEN
        RAISE EXCEPTION 'Expected 30 mock contact identities, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM inbox_conversations conversation
    JOIN _omni_conversations seed ON seed.id = conversation.id
    WHERE conversation.organization_id = org
      AND conversation.deleted_at IS NULL;
    IF actual <> 30 THEN
        RAISE EXCEPTION 'Expected 30 mock inbox conversations, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM conversation_participants participant
    JOIN _omni_conversations conversation
      ON conversation.id = participant.conversation_id
    WHERE participant.organization_id = org
      AND participant.deleted_at IS NULL;
    IF actual <> 60 THEN
        RAISE EXCEPTION 'Expected 60 mock conversation participants, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM conversation_reads read_cursor
    JOIN _omni_conversations conversation
      ON conversation.id = read_cursor.conversation_id
    WHERE read_cursor.organization_id = org
      AND read_cursor.reader_key = 'user:' || owner::text
      AND read_cursor.deleted_at IS NULL;
    IF actual <> 30 THEN
        RAISE EXCEPTION 'Expected 30 mock read cursors, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM messages message
    JOIN _omni_message_rows seed ON seed.message_id = message.id
    WHERE message.organization_id = org
      AND message.deleted_at IS NULL;
    IF actual <> 135 THEN
        RAISE EXCEPTION 'Expected 135 omnichannel messages, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM message_parts part
    JOIN _omni_message_rows seed
      ON part.id =
        md5('klinik-relive-omnichannel-v1-message-part-' ||
            seed.message_seq)::uuid
    WHERE part.organization_id = org
      AND part.status = 'ready'
      AND part.deleted_at IS NULL;
    IF actual <> 135 THEN
        RAISE EXCEPTION 'Expected 135 ready message parts, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM message_events event
    JOIN _omni_message_rows seed
      ON event.id =
        md5('klinik-relive-omnichannel-v1-message-event-' ||
            seed.message_seq)::uuid
    WHERE event.organization_id = org
      AND event.type::text = seed.message_status
      AND event.payload->>'terminal' = 'true'
      AND event.deleted_at IS NULL;
    IF actual <> 135 THEN
        RAISE EXCEPTION 'Expected 135 terminal message events, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM _omni_message_rows
    WHERE period_key = 'current';
    IF actual <> 90 THEN
        RAISE EXCEPTION 'Expected 90 current-month omnichannel messages, found %', actual;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM _omni_message_rows
        WHERE period_key = 'current'
        GROUP BY account_id
        HAVING count(*) <> 15
    ) OR (
        SELECT count(DISTINCT account_id)
        FROM _omni_message_rows
        WHERE period_key = 'current'
    ) <> 6 THEN
        RAISE EXCEPTION 'Expected exactly 15 current messages for each of six channels';
    END IF;

    IF (
        SELECT count(*)
        FROM _omni_message_rows
        WHERE period_key = 'current'
          AND direction = 'incoming'
    ) <> 60 OR (
        SELECT count(*)
        FROM _omni_message_rows
        WHERE period_key = 'current'
          AND direction = 'outgoing'
    ) <> 30 THEN
        RAISE EXCEPTION 'Expected current direction mix of 60 incoming and 30 outgoing';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM _omni_message_rows
        WHERE period_key = 'current'
          AND direction = 'outgoing'
        GROUP BY message_status
        HAVING count(*) <> 10
    ) OR (
        SELECT count(DISTINCT message_status)
        FROM _omni_message_rows
        WHERE period_key = 'current'
          AND direction = 'outgoing'
    ) <> 3 THEN
        RAISE EXCEPTION 'Expected outgoing delivery mix of 10 sent, 10 delivered, and 10 read';
    END IF;

    SELECT count(*) INTO unread_messages
    FROM messages message
    JOIN _omni_conversations conversation
      ON conversation.id = message.inbox_conversation_id
    JOIN conversation_reads read_cursor
      ON read_cursor.organization_id = message.organization_id
     AND read_cursor.conversation_id = message.inbox_conversation_id
     AND read_cursor.reader_key = 'user:' || owner::text
     AND read_cursor.deleted_at IS NULL
    WHERE message.organization_id = org
      AND message.direction = 'incoming'
      AND message.created_at > read_cursor.last_read_at
      AND message.deleted_at IS NULL;
    IF unread_messages <> 12 THEN
        RAISE EXCEPTION 'Expected 12 calculated unread messages, found %', unread_messages;
    END IF;

    SELECT count(*) INTO actual
    FROM keyword_rules rule
    WHERE rule.id IN (
        SELECT md5('klinik-relive-omnichannel-v1-keyword-' || seq)::uuid
        FROM _omni_keyword_defs
    )
      AND rule.organization_id = org
      AND rule.whats_app_account = '[MOCK] DEMO'
      AND rule.is_enabled = false
      AND rule.deleted_at IS NULL;
    IF actual <> 12 THEN
        RAISE EXCEPTION 'Expected 12 disabled mock-scoped keyword rules, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM chatbot_flows flow
    JOIN _omni_chat_flow_defs seed ON seed.id = flow.id
    WHERE flow.organization_id = org
      AND flow.whats_app_account = '[MOCK] DEMO'
      AND flow.is_enabled = false
      AND flow.graph->>'version' = '2'
      AND flow.graph->>'entry_node' = 'start'
      AND jsonb_array_length(flow.graph->'nodes') = 5
      AND flow.deleted_at IS NULL;
    IF actual <> 8 THEN
        RAISE EXCEPTION 'Expected 8 disabled rich chatbot v2 flows, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM ai_contexts context
    WHERE context.id IN (
        SELECT md5('klinik-relive-omnichannel-v1-ai-context-' || seq)::uuid
        FROM _omni_context_defs
    )
      AND context.organization_id = org
      AND context.whats_app_account = '[MOCK] DEMO'
      AND context.deleted_at IS NULL;
    IF actual <> 12 THEN
        RAISE EXCEPTION 'Expected 12 strictly mock-scoped AI contexts, found %', actual;
    END IF;

    IF (
        SELECT count(*)
        FROM ai_contexts context
        WHERE context.id IN (
            SELECT md5('klinik-relive-omnichannel-v1-ai-context-' || seq)::uuid
            FROM generate_series(1, 10) AS series(seq)
        )
          AND context.organization_id = org
          AND context.context_type = 'static'
          AND context.is_enabled = true
          AND context.whats_app_account = '[MOCK] DEMO'
          AND context.deleted_at IS NULL
    ) <> 10 OR (
        SELECT count(*)
        FROM ai_contexts context
        WHERE context.id IN (
            SELECT md5('klinik-relive-omnichannel-v1-ai-context-' || seq)::uuid
            FROM generate_series(11, 12) AS series(seq)
        )
          AND context.organization_id = org
          AND context.context_type = 'api'
          AND context.is_enabled = false
          AND context.api_config->>'url' LIKE '%.invalid/%'
          AND context.deleted_at IS NULL
    ) <> 2 THEN
        RAISE EXCEPTION 'AI context enabled/type/scope safety distribution is invalid';
    END IF;

    SELECT count(*) INTO actual
    FROM _omni_session_map map
    JOIN chatbot_sessions session ON session.id = map.session_id
    WHERE session.organization_id = org
      AND session.status IN ('completed', 'timeout', 'cancelled')
      AND session.current_flow_id IS NOT NULL
      AND session.completed_at IS NOT NULL
      AND session.deleted_at IS NULL;
    IF actual <> 24 THEN
        RAISE EXCEPTION 'Expected 24 terminal sessions linked to disabled flows, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM _omni_session_map map
    CROSS JOIN generate_series(1, 6) AS transcript(message_no)
    JOIN chatbot_session_messages message
      ON message.id = md5('klinik-relive-omnichannel-v1-session-message-' ||
        map.seq || '-' || transcript.message_no)::uuid
     AND message.session_id = map.session_id
    WHERE message.deleted_at IS NULL;
    IF actual <> 144 THEN
        RAISE EXCEPTION 'Expected 144 chatbot session transcript rows, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM _omni_session_map map
    JOIN chatbot_session_messages message
      ON message.session_id = map.session_id
     AND message.step_name = 'ai_response'
     AND message.deleted_at IS NULL;
    IF actual <> 6 THEN
        RAISE EXCEPTION 'Expected 6 synthetic AI-response transcript rows, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM whatsapp_flows flow
    JOIN _omni_wa_flow_defs seed ON seed.id = flow.id
    WHERE flow.organization_id = org
      AND flow.whats_app_account = '[MOCK] DEMO'
      AND flow.status = 'DRAFT'
      AND flow.meta_flow_id = ''
      AND flow.preview_url = ''
      AND flow.has_local_changes = true
      AND jsonb_array_length(flow.screens) = 2
      AND flow.deleted_at IS NULL;
    IF actual <> 8 THEN
        RAISE EXCEPTION 'Expected 8 local-only WhatsApp Flow drafts, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM copilot_runs run
    JOIN _omni_copilot_rows seed ON seed.id = run.id
    WHERE run.organization_id = org
      AND run.status IN ('completed', 'failed', 'blocked', 'cancelled')
      AND run.idempotency_key LIKE '[MOCK]-omnichannel-copilot-%'
      AND run.deleted_at IS NULL;
    IF actual <> 48 THEN
        RAISE EXCEPTION 'Expected 48 terminal Copilot runs, found %', actual;
    END IF;

    IF (
        SELECT count(*) FROM copilot_runs run
        JOIN _omni_copilot_rows seed ON seed.id = run.id
        WHERE run.status = 'completed' AND run.deleted_at IS NULL
    ) <> 42 OR (
        SELECT count(*) FROM copilot_runs run
        JOIN _omni_copilot_rows seed ON seed.id = run.id
        WHERE run.status = 'failed' AND run.deleted_at IS NULL
    ) <> 2 OR (
        SELECT count(*) FROM copilot_runs run
        JOIN _omni_copilot_rows seed ON seed.id = run.id
        WHERE run.status = 'blocked' AND run.deleted_at IS NULL
    ) <> 2 OR (
        SELECT count(*) FROM copilot_runs run
        JOIN _omni_copilot_rows seed ON seed.id = run.id
        WHERE run.status = 'cancelled' AND run.deleted_at IS NULL
    ) <> 2 THEN
        RAISE EXCEPTION 'Copilot terminal status distribution is invalid';
    END IF;

    SELECT count(*) INTO actual
    FROM copilot_feedback feedback
    JOIN _omni_copilot_rows seed ON seed.id = feedback.run_id
    WHERE feedback.organization_id = org
      AND seed.seq <= 30
      AND feedback.final_message_id IS NULL
      AND feedback.metadata->>'message_sent' = 'false'
      AND feedback.deleted_at IS NULL;
    IF actual <> 30 THEN
        RAISE EXCEPTION 'Expected 30 no-send Copilot feedback rows, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM users user_row
    JOIN _omni_agents agent ON agent.id = user_row.id
    WHERE user_row.organization_id = org
      AND user_row.email LIKE '%.invalid'
      AND user_row.is_active = false
      AND user_row.is_available = false
      AND user_row.deleted_at IS NULL;
    IF actual <> 3 THEN
        RAISE EXCEPTION 'Expected 3 inactive .invalid mock agents, found %', actual;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM teams team
        WHERE team.id = md5('klinik-relive-omnichannel-v1-team')::uuid
          AND team.organization_id = org
          AND team.is_active = false
          AND team.deleted_at IS NULL
    ) OR (
        SELECT count(*)
        FROM team_members member
        JOIN _omni_agents agent ON agent.id = member.user_id
        WHERE member.team_id = md5('klinik-relive-omnichannel-v1-team')::uuid
          AND member.role = 'agent'
          AND member.deleted_at IS NULL
    ) <> 3 THEN
        RAISE EXCEPTION 'Inactive mock team or its 3 agent memberships are incomplete';
    END IF;

    SELECT count(*) INTO actual
    FROM agent_transfers transfer
    JOIN _omni_transfer_rows seed ON seed.id = transfer.id
    WHERE transfer.organization_id = org
      AND transfer.status IN ('resumed', 'expired')
      AND transfer.deleted_at IS NULL;
    IF actual <> 24 THEN
        RAISE EXCEPTION 'Expected 24 terminal agent transfers, found %', actual;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM agent_transfers transfer
        JOIN _omni_transfer_rows seed ON seed.id = transfer.id
        WHERE transfer.status = 'active'
           OR transfer.resumed_at IS NULL AND transfer.status = 'resumed'
    ) OR EXISTS (
        SELECT 1
        FROM agent_transfers transfer
        JOIN _omni_transfer_rows seed ON seed.id = transfer.id
        GROUP BY transfer.source
        HAVING count(*) <> 6
    ) THEN
        RAISE EXCEPTION 'Transfer terminal/source distribution is invalid';
    END IF;

    SELECT count(*) INTO actual
    FROM user_availability_logs log
    JOIN _omni_agents agent ON agent.id = log.user_id
    WHERE log.organization_id = org
      AND log.is_available = false
      AND log.ended_at IS NOT NULL;
    IF actual <> 12 THEN
        RAISE EXCEPTION 'Expected 12 closed availability logs, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM widgets widget
    WHERE widget.id IN (
        SELECT md5('klinik-relive-omnichannel-v1-widget-' || seq)::uuid
        FROM _omni_widget_defs
    )
      AND widget.organization_id = org
      AND widget.is_shared = true
      AND widget.is_default = false
      AND widget.deleted_at IS NULL;
    IF actual <> 7 THEN
        RAISE EXCEPTION 'Expected 7 shared analytics widgets, found %', actual;
    END IF;

    SELECT count(*) INTO actual
    FROM audit_logs audit
    JOIN _omni_audit_resources resource
      ON audit.id = md5('klinik-relive-omnichannel-v1-audit-' ||
        resource.resource_type || '-' || resource.seq)::uuid
    WHERE audit.organization_id = org
      AND audit.action = 'created';
    IF actual <> 40 THEN
        RAISE EXCEPTION 'Expected 40 inert audit history rows, found %', actual;
    END IF;

    SELECT count(*) INTO current_mock_messages
    FROM messages
    WHERE organization_id = org
      AND metadata->>'mock_dataset' IN (
          'klinik-relive-crm-v2',
          'klinik-relive-omnichannel-v1'
      )
      AND created_at >= date_trunc('month', CURRENT_TIMESTAMP)
      AND created_at <= CURRENT_TIMESTAMP
      AND deleted_at IS NULL;

    SELECT count(*) INTO previous_mock_messages
    FROM messages
    WHERE organization_id = org
      AND metadata->>'mock_dataset' IN (
          'klinik-relive-crm-v2',
          'klinik-relive-omnichannel-v1'
      )
      AND created_at >= date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
      AND created_at < date_trunc('month', CURRENT_TIMESTAMP)
      AND deleted_at IS NULL;

    IF current_mock_messages <> 150 OR previous_mock_messages <> 60 THEN
        RAISE EXCEPTION
            'Expected combined dashboard message totals 150 current / 60 previous, found % / %',
            current_mock_messages, previous_mock_messages;
    END IF;

    SELECT count(*) INTO current_sessions
    FROM _omni_session_map map
    JOIN chatbot_sessions session ON session.id = map.session_id
    WHERE session.created_at >= date_trunc('month', CURRENT_TIMESTAMP)
      AND session.created_at <= CURRENT_TIMESTAMP
      AND session.deleted_at IS NULL;

    SELECT count(*) INTO previous_sessions
    FROM _omni_session_map map
    JOIN chatbot_sessions session ON session.id = map.session_id
    WHERE session.created_at >= date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
      AND session.created_at < date_trunc('month', CURRENT_TIMESTAMP)
      AND session.deleted_at IS NULL;

    IF current_sessions <> 15 OR previous_sessions <> 9 THEN
        RAISE EXCEPTION
            'Expected chatbot dashboard totals 15 current / 9 previous, found % / %',
            current_sessions, previous_sessions;
    END IF;

    SELECT count(*) INTO active_contacts
    FROM generate_series(1, 40) AS series(seq)
    JOIN contacts contact
      ON contact.organization_id = org
     AND contact.phone_number =
        '6000000000' || lpad(series.seq::text, 2, '0')
     AND contact.last_message_at >= date_trunc('month', CURRENT_TIMESTAMP)
     AND contact.deleted_at IS NULL;

    IF active_contacts <> 40 THEN
        RAISE EXCEPTION 'Expected 40 current-month mock contacts, found %', active_contacts;
    END IF;

    RAISE NOTICE
        'DRY RUN PASS: accounts=6, conversations=30, messages=135, parts=135, events=135, unread=12';
    RAISE NOTICE
        'DRY RUN PASS: rules=12 disabled, chatbot_flows=8 disabled, contexts=12, sessions=24, session_messages=144, ai_responses=6';
    RAISE NOTICE
        'DRY RUN PASS: wa_flows=8 local DRAFT, copilot_runs=48, feedback=30, transfers=24, widgets=7, audits=40';
    RAISE NOTICE
        'EXPECTED DASHBOARD: messages current=150 previous=60; sessions current=15 previous=9; active_contacts=40';
END $$;

SELECT
    before_counts.outbox_jobs AS outbox_jobs_before,
    after_counts.outbox_jobs AS outbox_jobs_after,
    before_counts.inbound_events AS inbound_events_before,
    after_counts.inbound_events AS inbound_events_after,
    before_counts.automation_event_receipts AS automation_receipts_before,
    after_counts.automation_event_receipts AS automation_receipts_after,
    before_counts.scheduled_jobs AS scheduled_jobs_before,
    after_counts.scheduled_jobs AS scheduled_jobs_after,
    before_counts.outbox_events AS outbox_events_before,
    after_counts.outbox_events AS outbox_events_after,
    before_counts.customer_activity_events AS customer_activity_events_before,
    after_counts.customer_activity_events AS customer_activity_events_after
FROM _omni_safety_before before_counts
CROSS JOIN _omni_safety_after after_counts;

-- Deliberate dry-run terminator. Do not replace without reviewing every
-- assertion and the safety counter output above in the intended environment.
ROLLBACK;
