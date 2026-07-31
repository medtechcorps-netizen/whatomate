\set ON_ERROR_STOP on

BEGIN;
SET LOCAL app.current_organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a';

CREATE TEMP TABLE _kr_ctx AS
SELECT
    'c73f761f-5154-4fe1-9a13-06bae570277a'::uuid AS organization_id,
    (SELECT id FROM users WHERE email = 'admintest@rereply.com' LIMIT 1) AS owner_id,
    (SELECT id FROM crm_pipelines
      WHERE organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND name = 'Patient journey'
        AND deleted_at IS NULL
      LIMIT 1) AS pipeline_id,
    (SELECT id FROM booking_services
      WHERE organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND name = 'Initial assessment'
        AND deleted_at IS NULL
      LIMIT 1) AS initial_service_id,
    (SELECT id FROM booking_services
      WHERE organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND name = 'Follow-up appointment'
        AND deleted_at IS NULL
      LIMIT 1) AS followup_service_id,
    (SELECT id FROM booking_resources
      WHERE organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND name = '[MOCK] Dr Hana Yusuf'
        AND deleted_at IS NULL
      LIMIT 1) AS resource_id,
    (SELECT id FROM package_definitions
      WHERE organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND name = '[MOCK] Metabolic Reset 6 Sessions'
        AND deleted_at IS NULL
      LIMIT 1) AS metabolic_definition_id,
    (SELECT pe.id
      FROM package_entitlements pe
      JOIN package_definitions pd
        ON pd.id = pe.package_definition_id
       AND pd.organization_id = pe.organization_id
      WHERE pe.organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND pd.name = '[MOCK] Metabolic Reset 6 Sessions'
        AND pe.deleted_at IS NULL
        AND pd.deleted_at IS NULL
      LIMIT 1) AS metabolic_entitlement_id,
    (SELECT id FROM package_definitions
      WHERE organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND name = '[MOCK] Follow-up Care 4 Sessions'
        AND deleted_at IS NULL
      LIMIT 1) AS followup_definition_id,
    (SELECT pe.id
      FROM package_entitlements pe
      JOIN package_definitions pd
        ON pd.id = pe.package_definition_id
       AND pd.organization_id = pe.organization_id
      WHERE pe.organization_id = 'c73f761f-5154-4fe1-9a13-06bae570277a'
        AND pd.name = '[MOCK] Follow-up Care 4 Sessions'
        AND pe.deleted_at IS NULL
        AND pd.deleted_at IS NULL
      LIMIT 1) AS followup_entitlement_id;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM _kr_ctx
        WHERE owner_id IS NULL
           OR pipeline_id IS NULL
           OR initial_service_id IS NULL
           OR followup_service_id IS NULL
           OR resource_id IS NULL
           OR metabolic_definition_id IS NULL
           OR metabolic_entitlement_id IS NULL
           OR followup_definition_id IS NULL
           OR followup_entitlement_id IS NULL
    ) THEN
        RAISE EXCEPTION 'Klinik Relive seed foundations are incomplete';
    END IF;
    IF (SELECT count(*) FROM crm_pipeline_stages
        WHERE organization_id = (SELECT organization_id FROM _kr_ctx)
          AND pipeline_id = (SELECT pipeline_id FROM _kr_ctx)
          AND name IN (
              'New enquiry',
              'Qualified',
              'Appointment booked',
              'Attended',
              'Follow-up',
              'Converted',
              'Not proceeding'
          )
          AND deleted_at IS NULL) <> 7 THEN
        RAISE EXCEPTION 'Patient journey stages are incomplete';
    END IF;
END $$;

INSERT INTO tags (organization_id, name, color, created_at, updated_at)
SELECT organization_id, tag_name, color, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
FROM _kr_ctx
CROSS JOIN (
    VALUES
        ('[MOCK] VIP', 'purple'),
        ('[MOCK] Referral', 'green'),
        ('[MOCK] High Intent', 'red'),
        ('[MOCK] No Show', 'yellow'),
        ('[MOCK] Renewal Window', 'blue')
) AS seed(tag_name, color)
ON CONFLICT (organization_id, name)
DO UPDATE SET color = EXCLUDED.color, updated_at = CURRENT_TIMESTAMP;

CREATE TEMP TABLE _mock_contacts (
    idx integer PRIMARY KEY,
    profile_name text NOT NULL,
    city text NOT NULL,
    language text NOT NULL,
    acquisition_source text NOT NULL,
    preferred_time text NOT NULL
);

INSERT INTO _mock_contacts VALUES
    (13, 'Aisyah Omar', 'Kuala Lumpur', 'Malay', 'WhatsApp', 'weekday mornings'),
    (14, 'Kelvin Goh', 'Petaling Jaya', 'English', 'Referral', 'weekday evenings'),
    (15, 'Nabila Ismail', 'Shah Alam', 'Malay', 'Campaign', 'Saturday mornings'),
    (16, 'Marcus Tan', 'Subang Jaya', 'English', 'Walk-in', 'weekday afternoons'),
    (17, 'Kavitha Raj', 'Kuala Lumpur', 'English', 'API partner', 'weekday mornings'),
    (18, 'Syafiq Rahman', 'Kajang', 'Malay', 'Referral', 'weekday evenings'),
    (19, 'Grace Lee', 'Petaling Jaya', 'English', 'Campaign', 'Saturday afternoons'),
    (20, 'Faizal Hamid', 'Cheras', 'Malay', 'WhatsApp', 'weekday mornings'),
    (21, 'Joanne Lim', 'Kuala Lumpur', 'English', 'Walk-in', 'weekday afternoons'),
    (22, 'Haris Abdullah', 'Bangi', 'Malay', 'Import', 'weekday evenings'),
    (23, 'Nadia Yusof', 'Shah Alam', 'Malay', 'Referral', 'Saturday mornings'),
    (24, 'Ethan Chong', 'Petaling Jaya', 'English', 'Campaign', 'weekday afternoons'),
    (25, 'Liyana Farid', 'Subang Jaya', 'Malay', 'WhatsApp', 'weekday mornings'),
    (26, 'Dev Anand', 'Kuala Lumpur', 'English', 'API partner', 'weekday evenings'),
    (27, 'Rachel Teo', 'Petaling Jaya', 'English', 'Other', 'Saturday afternoons'),
    (28, 'Zul Ariffin', 'Kajang', 'Malay', 'Referral', 'weekday mornings'),
    (29, 'Hannah Ng', 'Kuala Lumpur', 'English', 'WhatsApp', 'weekday afternoons'),
    (30, 'Imran Shah', 'Shah Alam', 'Malay', 'Walk-in', 'weekday evenings'),
    (31, 'Bella Wong', 'Petaling Jaya', 'English', 'Campaign', 'Saturday mornings'),
    (32, 'Jasmin Kaur', 'Subang Jaya', 'English', 'Referral', 'weekday afternoons'),
    (33, 'Adam Low', 'Kuala Lumpur', 'English', 'API partner', 'weekday mornings'),
    (34, 'Nurul Izzati', 'Bangi', 'Malay', 'WhatsApp', 'weekday evenings'),
    (35, 'Ryan Chew', 'Petaling Jaya', 'English', 'Import', 'Saturday afternoons'),
    (36, 'Sarah Aziz', 'Shah Alam', 'Malay', 'Referral', 'weekday mornings'),
    (37, 'Kumar Velu', 'Kuala Lumpur', 'English', 'Walk-in', 'weekday afternoons'),
    (38, 'Elaine Yap', 'Petaling Jaya', 'English', 'Campaign', 'weekday evenings'),
    (39, 'Firdaus Rosli', 'Kajang', 'Malay', 'Other', 'Saturday mornings'),
    (40, 'Michelle Ho', 'Subang Jaya', 'English', 'Referral', 'Saturday afternoons');

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM contacts c
        JOIN _mock_contacts m
          ON c.id = md5('klinik-relive-crm-v2-contact-' || m.idx)::uuid
        WHERE COALESCE(c.metadata->>'mock_dataset', '') NOT IN ('', 'klinik-relive-crm-v2')
    ) THEN
        RAISE EXCEPTION 'A deterministic contact ID is already owned by non-mock data';
    END IF;
END $$;

INSERT INTO contacts (
    id,
    organization_id,
    phone_number,
    profile_name,
    assigned_user_id,
    last_message_at,
    last_message_preview,
    is_read,
    tags,
    metadata,
    last_inbound_at,
    marketing_opt_out,
    chatbot_reminder_sent,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-contact-' || m.idx)::uuid,
    ctx.organization_id,
    '6000000000' || lpad(m.idx::text, 2, '0'),
    '[MOCK] ' || m.profile_name,
    CASE WHEN m.idx % 4 = 0 THEN NULL ELSE ctx.owner_id END,
    CASE
        WHEN m.idx >= 36 THEN NULL
        ELSE CURRENT_TIMESTAMP - make_interval(days => ((m.idx * 2) % 20) + 1)
    END,
    CASE
        WHEN m.idx >= 36 THEN ''
        ELSE '[MOCK] Asked about ' ||
             CASE m.idx % 5
                 WHEN 0 THEN 'package options'
                 WHEN 1 THEN 'appointment availability'
                 WHEN 2 THEN 'pricing and next steps'
                 WHEN 3 THEN 'follow-up care'
                 ELSE 'wellness goals'
             END
    END,
    true,
    jsonb_build_array(
        'MOCK-DEMO',
        CASE m.idx % 5
            WHEN 0 THEN '[MOCK] VIP'
            WHEN 1 THEN '[MOCK] Referral'
            WHEN 2 THEN '[MOCK] High Intent'
            WHEN 3 THEN 'Follow-up Due'
            ELSE 'Appointment Booked'
        END,
        CASE m.idx % 4
            WHEN 0 THEN 'Package Holder'
            WHEN 1 THEN 'New Enquiry'
            WHEN 2 THEN 'Payment Due'
            ELSE 'Dormant'
        END
    ),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'mock_key', 'contact-' || lpad(m.idx::text, 2, '0'),
        'email', lower(replace(m.profile_name, ' ', '.')) || '@klinik-relive.example.invalid',
        'city', m.city,
        'language', m.language,
        'preferred_channel', 'WhatsApp',
        'preferred_contact_time', m.preferred_time,
        'acquisition_source', m.acquisition_source,
        'lead_score', 45 + ((m.idx * 7) % 51),
        'lifecycle_stage',
            CASE m.idx % 5
                WHEN 0 THEN 'Package holder'
                WHEN 1 THEN 'New inquiry'
                WHEN 2 THEN 'Appointment planned'
                WHEN 3 THEN 'Follow-up'
                ELSE 'Returning customer'
            END,
        'journey_context', jsonb_build_object(
            'interest',
                CASE m.idx % 4
                    WHEN 0 THEN 'Metabolic wellness'
                    WHEN 1 THEN 'Initial assessment'
                    WHEN 2 THEN 'Follow-up care'
                    ELSE 'Preventive wellness'
                END,
            'goal_horizon', CASE WHEN m.idx % 2 = 0 THEN '8-12 weeks' ELSE '4-8 weeks' END,
            'decision_stage', CASE WHEN m.idx % 3 = 0 THEN 'comparing' ELSE 'ready to discuss' END
        ),
        'appointment_preferences', jsonb_build_object(
            'days', CASE WHEN m.idx % 2 = 0 THEN 'Weekdays' ELSE 'Saturday' END,
            'time', m.preferred_time,
            'practitioner_preference', 'No preference'
        ),
        'commercial_profile', jsonb_build_object(
            'budget_band', CASE m.idx % 3 WHEN 0 THEN 'RM500-RM1,500' WHEN 1 THEN 'RM1,500-RM3,000' ELSE 'RM3,000+' END,
            'package_interest', (m.idx % 2 = 0),
            'payment_preference', CASE WHEN m.idx % 2 = 0 THEN 'Card' ELSE 'Bank transfer' END
        ),
        'consent_notes', jsonb_build_object(
            'demo_only', true,
            'marketing_opt_in', false,
            'source', 'Synthetic CRM showcase'
        )
    ),
    CASE
        WHEN m.idx >= 36 THEN NULL
        ELSE CURRENT_TIMESTAMP - make_interval(days => ((m.idx * 2) % 20) + 1)
    END,
    false,
    false,
    CASE
        WHEN m.idx >= 36 THEN CURRENT_TIMESTAMP - make_interval(days => 80 + ((m.idx - 36) * 8))
        ELSE CURRENT_TIMESTAMP - make_interval(days => ((m.idx * 3) % 27) + 1)
    END,
    CURRENT_TIMESTAMP
FROM _mock_contacts m
CROSS JOIN _kr_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    phone_number = EXCLUDED.phone_number,
    profile_name = EXCLUDED.profile_name,
    assigned_user_id = EXCLUDED.assigned_user_id,
    last_message_at = EXCLUDED.last_message_at,
    last_message_preview = EXCLUDED.last_message_preview,
    is_read = EXCLUDED.is_read,
    tags = EXCLUDED.tags,
    metadata = EXCLUDED.metadata,
    last_inbound_at = EXCLUDED.last_inbound_at,
    marketing_opt_out = EXCLUDED.marketing_opt_out,
    chatbot_reminder_sent = EXCLUDED.chatbot_reminder_sent,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

UPDATE contacts c
SET
    assigned_user_id = CASE WHEN right(c.phone_number, 3)::int % 4 = 0 THEN NULL ELSE ctx.owner_id END,
    last_message_at = CURRENT_TIMESTAMP - make_interval(days => ((right(c.phone_number, 3)::int * 2) % 20) + 1),
    last_inbound_at = CURRENT_TIMESTAMP - make_interval(days => ((right(c.phone_number, 3)::int * 2) % 20) + 1),
    last_message_preview = '[MOCK] Existing demo contact enriched for the CRM showcase',
    is_read = true,
    tags = jsonb_build_array(
        'MOCK-DEMO',
        CASE right(c.phone_number, 3)::int % 4
            WHEN 0 THEN '[MOCK] VIP'
            WHEN 1 THEN '[MOCK] Referral'
            WHEN 2 THEN '[MOCK] High Intent'
            ELSE 'Follow-up Due'
        END,
        CASE right(c.phone_number, 3)::int % 3
            WHEN 0 THEN 'Package Holder'
            WHEN 1 THEN 'Appointment Booked'
            ELSE 'New Enquiry'
        END
    ),
    metadata = COALESCE(c.metadata, '{}'::jsonb) || jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'mock_key', 'contact-' || right(c.phone_number, 3),
        'email', 'demo.contact' || right(c.phone_number, 3) || '@klinik-relive.example.invalid',
        'city', CASE right(c.phone_number, 3)::int % 4
            WHEN 0 THEN 'Kuala Lumpur'
            WHEN 1 THEN 'Petaling Jaya'
            WHEN 2 THEN 'Shah Alam'
            ELSE 'Subang Jaya'
        END,
        'language', CASE WHEN right(c.phone_number, 3)::int % 2 = 0 THEN 'English' ELSE 'Malay' END,
        'preferred_channel', 'WhatsApp',
        'preferred_contact_time', CASE WHEN right(c.phone_number, 3)::int % 2 = 0 THEN 'weekday mornings' ELSE 'weekday evenings' END,
        'lead_score', 55 + ((right(c.phone_number, 3)::int * 5) % 41),
        'journey_context', jsonb_build_object(
            'interest', CASE right(c.phone_number, 3)::int % 3 WHEN 0 THEN 'Metabolic wellness' WHEN 1 THEN 'Assessment' ELSE 'Follow-up care' END,
            'goal_horizon', '8-12 weeks',
            'decision_stage', 'ready to discuss'
        ),
        'appointment_preferences', jsonb_build_object(
            'days', 'Weekdays',
            'time', CASE WHEN right(c.phone_number, 3)::int % 2 = 0 THEN 'Morning' ELSE 'Evening' END
        ),
        'commercial_profile', jsonb_build_object(
            'budget_band', CASE right(c.phone_number, 3)::int % 3 WHEN 0 THEN 'RM500-RM1,500' WHEN 1 THEN 'RM1,500-RM3,000' ELSE 'RM3,000+' END,
            'package_interest', true
        ),
        'consent_notes', jsonb_build_object(
            'demo_only', true,
            'marketing_opt_in', false
        )
    ),
    updated_at = CURRENT_TIMESTAMP
FROM _kr_ctx ctx
WHERE c.organization_id = ctx.organization_id
  AND c.deleted_at IS NULL
  AND c.phone_number ~ '^6000000000(0[1-9]|1[0-2])$'
  AND c.profile_name LIKE '[MOCK]%';

INSERT INTO conversation_notes (
    id,
    organization_id,
    contact_id,
    created_by_id,
    content,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-note-primary-' || series.idx)::uuid,
    ctx.organization_id,
    c.id,
    ctx.owner_id,
    CASE series.idx % 5
        WHEN 0 THEN '[MOCK] Prefers a concise overview of package inclusions before deciding.'
        WHEN 1 THEN '[MOCK] Interested in an assessment first; follow up with available weekday slots.'
        WHEN 2 THEN '[MOCK] Asked for pricing to be broken down by session and package option.'
        WHEN 3 THEN '[MOCK] Values continuity with the same practitioner for follow-up appointments.'
        ELSE '[MOCK] Demo note: confirm goals, preferred timing and next action during follow-up.'
    END,
    CURRENT_TIMESTAMP - make_interval(days => ((series.idx * 3) % 25) + 1),
    CURRENT_TIMESTAMP - make_interval(days => ((series.idx * 3) % 25) + 1)
FROM generate_series(1, 40) AS series(idx)
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(series.idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    content = EXCLUDED.content,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO conversation_notes (
    id,
    organization_id,
    contact_id,
    created_by_id,
    content,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-note-secondary-' || series.idx)::uuid,
    ctx.organization_id,
    c.id,
    ctx.owner_id,
    CASE series.idx % 4
        WHEN 0 THEN '[MOCK] Internal handoff: review the open opportunity and align the next appointment.'
        WHEN 1 THEN '[MOCK] Demo commercial note: package interest is high; clarify renewal window.'
        WHEN 2 THEN '[MOCK] Demo service note: morning appointments are preferred where possible.'
        ELSE '[MOCK] Demo follow-up note: keep all customer-facing messages human-approved.'
    END,
    CURRENT_TIMESTAMP - make_interval(days => ((series.idx * 2) % 18) + 1),
    CURRENT_TIMESTAMP - make_interval(days => ((series.idx * 2) % 18) + 1)
FROM generate_series(1, 20) AS series(idx)
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(series.idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    content = EXCLUDED.content,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _mock_message_times AS
SELECT
    g AS seq,
    ((g - 1) % 35) + 1 AS contact_idx,
    CASE
        WHEN g <= 60 THEN
            date_trunc('month', CURRENT_TIMESTAMP)
            + make_interval(days => 2 + ((g * 7) % 27), hours => 8 + (g % 9))
        ELSE
            date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
            + make_interval(days => 2 + ((g * 5) % 24), hours => 8 + (g % 9))
    END AS occurred_at
FROM generate_series(1, 75) AS seq(g);

INSERT INTO messages (
    id,
    organization_id,
    whats_app_account,
    contact_id,
    whats_app_message_id,
    conversation_id,
    direction,
    message_type,
    content,
    template_params,
    interactive_data,
    flow_response,
    status,
    is_reply,
    metadata,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-message-' || mt.seq)::uuid,
    ctx.organization_id,
    '[MOCK] DEMO',
    c.id,
    'MOCK-KR-MSG-' || lpad(mt.seq::text, 3, '0'),
    'MOCK-KR-CONV-' || lpad(mt.contact_idx::text, 2, '0'),
    'incoming',
    'text',
    CASE mt.seq % 8
        WHEN 0 THEN '[MOCK] Could I see the available assessment slots?'
        WHEN 1 THEN '[MOCK] I would like to understand the package options.'
        WHEN 2 THEN '[MOCK] Please note that weekday mornings work best.'
        WHEN 3 THEN '[MOCK] Can the follow-up be with the same practitioner?'
        WHEN 4 THEN '[MOCK] I am comparing the four-session and six-session plans.'
        WHEN 5 THEN '[MOCK] Please share the next steps for confirming an appointment.'
        WHEN 6 THEN '[MOCK] I would like to renew after my remaining sessions.'
        ELSE '[MOCK] Thank you, I will review the information and follow up.'
    END,
    '{}'::jsonb,
    '{}'::jsonb,
    '{}'::jsonb,
    'read',
    false,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'delivery_attempted', false,
        'source', 'synthetic_inbound_history'
    ),
    mt.occurred_at,
    mt.occurred_at
FROM _mock_message_times mt
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(mt.contact_idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    content = EXCLUDED.content,
    status = EXCLUDED.status,
    metadata = EXCLUDED.metadata,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

UPDATE contacts c
SET
    last_message_at = latest.last_message_at,
    last_inbound_at = latest.last_message_at,
    last_message_preview = latest.last_message_preview,
    is_read = true,
    updated_at = CURRENT_TIMESTAMP
FROM (
    SELECT
        m.contact_id,
        max(m.created_at) AS last_message_at,
        (array_agg(m.content ORDER BY m.created_at DESC))[1] AS last_message_preview
    FROM messages m
    CROSS JOIN _kr_ctx ctx
    WHERE m.organization_id = ctx.organization_id
      AND m.metadata->>'mock_dataset' = 'klinik-relive-crm-v2'
      AND m.deleted_at IS NULL
    GROUP BY m.contact_id
) latest
WHERE c.id = latest.contact_id
  AND c.organization_id = (SELECT organization_id FROM _kr_ctx);

CREATE TEMP TABLE _mock_session_times AS
SELECT
    g AS seq,
    12 + g AS contact_idx,
    CASE
        WHEN g <= 8 THEN
            date_trunc('month', CURRENT_TIMESTAMP) + make_interval(days => 2 + (g * 3), hours => 10)
        ELSE
            date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
            + make_interval(days => 3 + ((g - 8) * 5), hours => 10)
    END AS started_at
FROM generate_series(1, 11) AS seq(g);

INSERT INTO chatbot_sessions (
    id,
    organization_id,
    contact_id,
    whats_app_account,
    phone_number,
    status,
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
    md5('klinik-relive-crm-v2-chatbot-session-' || st.seq)::uuid,
    ctx.organization_id,
    c.id,
    '[MOCK] DEMO',
    c.phone_number,
    CASE WHEN st.seq % 5 = 0 THEN 'timeout' ELSE 'completed' END,
    'demo_complete',
    CASE WHEN st.seq % 4 = 0 THEN 1 ELSE 0 END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'delivery_attempted', false,
        'intent', CASE st.seq % 3 WHEN 0 THEN 'booking' WHEN 1 THEN 'package inquiry' ELSE 'follow-up' END,
        'outcome', CASE WHEN st.seq % 5 = 0 THEN 'timed out safely' ELSE 'handoff completed' END
    ),
    st.started_at,
    st.started_at + interval '15 minutes',
    st.started_at + interval '20 minutes',
    st.started_at,
    st.started_at + interval '20 minutes'
FROM _mock_session_times st
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(st.contact_idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    status = EXCLUDED.status,
    current_step = EXCLUDED.current_step,
    session_data = EXCLUDED.session_data,
    started_at = EXCLUDED.started_at,
    last_activity_at = EXCLUDED.last_activity_at,
    completed_at = EXCLUDED.completed_at,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO templates (
    id,
    organization_id,
    whats_app_account,
    name,
    display_name,
    language,
    category,
    status,
    quality_rating,
    body_content,
    buttons,
    sample_values,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-template')::uuid,
    organization_id,
    '[MOCK] DEMO',
    'mock_klinik_relive_demo_summary',
    '[MOCK] Klinik Relive Demo Summary',
    'en',
    'UTILITY',
    'APPROVED',
    'UNKNOWN',
    '[MOCK] Synthetic campaign record for dashboard demonstration only. Nothing was sent.',
    '[]'::jsonb,
    '[]'::jsonb,
    owner_id,
    owner_id,
    CURRENT_TIMESTAMP - interval '60 days',
    CURRENT_TIMESTAMP
FROM _kr_ctx
ON CONFLICT (id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    body_content = EXCLUDED.body_content,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _mock_campaign_times AS
SELECT
    g AS seq,
    CASE
        WHEN g <= 4 THEN
            date_trunc('month', CURRENT_TIMESTAMP) + make_interval(days => 3 + (g * 5), hours => 9)
        ELSE
            date_trunc('month', CURRENT_TIMESTAMP) - interval '1 month'
            + make_interval(days => 4 + ((g - 4) * 7), hours => 9)
    END AS created_at
FROM generate_series(1, 6) AS seq(g);

INSERT INTO bulk_message_campaigns (
    id,
    organization_id,
    whats_app_account,
    name,
    template_id,
    status,
    total_recipients,
    sent_count,
    delivered_count,
    read_count,
    failed_count,
    started_at,
    completed_at,
    created_by,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-campaign-' || ct.seq)::uuid,
    ctx.organization_id,
    '[MOCK] DEMO',
    '[MOCK] Historical demo campaign ' || lpad(ct.seq::text, 2, '0'),
    md5('klinik-relive-crm-v2-template')::uuid,
    'completed',
    20 + (ct.seq * 4),
    20 + (ct.seq * 4),
    18 + (ct.seq * 4),
    14 + (ct.seq * 3),
    2,
    ct.created_at + interval '5 minutes',
    ct.created_at + interval '20 minutes',
    ctx.owner_id,
    ctx.owner_id,
    ct.created_at,
    ct.created_at + interval '20 minutes'
FROM _mock_campaign_times ct
CROSS JOIN _kr_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    status = EXCLUDED.status,
    total_recipients = EXCLUDED.total_recipients,
    sent_count = EXCLUDED.sent_count,
    delivered_count = EXCLUDED.delivered_count,
    read_count = EXCLUDED.read_count,
    failed_count = EXCLUDED.failed_count,
    started_at = EXCLUDED.started_at,
    completed_at = EXCLUDED.completed_at,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _mock_leads (
    seq integer PRIMARY KEY,
    contact_idx integer NOT NULL,
    title text NOT NULL,
    final_stage_name text NOT NULL,
    status text NOT NULL,
    source text NOT NULL,
    value_minor bigint NOT NULL,
    created_days integer NOT NULL,
    outcome_days integer,
    lost_reason text
);

INSERT INTO _mock_leads VALUES
    (1, 13, '[MOCK] Nutrition Reset Consultation', 'New enquiry', 'open', 'whatsapp', 80000, 2, NULL, NULL),
    (2, 14, '[MOCK] Wellness Screening', 'New enquiry', 'open', 'referral', 145000, 4, NULL, NULL),
    (3, 15, '[MOCK] Recovery Assessment', 'New enquiry', 'open', 'campaign', 220000, 6, NULL, NULL),
    (4, 16, '[MOCK] Executive Health Review', 'New enquiry', 'open', 'walk_in', 320000, 8, NULL, NULL),
    (5, 17, '[MOCK] Skin Consultation Plan', 'Qualified', 'open', 'api', 240000, 9, NULL, NULL),
    (6, 18, '[MOCK] Postnatal Care Plan', 'Qualified', 'open', 'referral', 360000, 10, NULL, NULL),
    (7, 19, '[MOCK] Metabolic Reset Starter', 'Qualified', 'open', 'campaign', 450000, 11, NULL, NULL),
    (8, 20, '[MOCK] Hormone Health Program', 'Appointment booked', 'open', 'whatsapp', 180000, 12, NULL, NULL),
    (9, 21, '[MOCK] IV Wellness Package', 'Appointment booked', 'open', 'walk_in', 275000, 13, NULL, NULL),
    (10, 22, '[MOCK] Weight Care Consultation', 'Appointment booked', 'open', 'import', 520000, 14, NULL, NULL),
    (11, 23, '[MOCK] Follow-up Therapy Plan', 'Attended', 'open', 'referral', 300000, 15, NULL, NULL),
    (12, 24, '[MOCK] 8-Week Vitality Program', 'Attended', 'open', 'campaign', 480000, 16, NULL, NULL),
    (13, 25, '[MOCK] Comprehensive Wellness Package', 'Attended', 'open', 'whatsapp', 650000, 17, NULL, NULL),
    (14, 26, '[MOCK] Recovery Session Bundle', 'Follow-up', 'open', 'api', 340000, 18, NULL, NULL),
    (15, 27, '[MOCK] Preventive Care Renewal', 'Follow-up', 'open', 'other', 470000, 19, NULL, NULL),
    (16, 28, '[MOCK] Metabolic Maintenance Package', 'Follow-up', 'open', 'referral', 580000, 20, NULL, NULL),
    (17, 29, '[MOCK] Completed Starter Assessment', 'Converted', 'won', 'whatsapp', 85000, 28, 3, NULL),
    (18, 30, '[MOCK] Completed Follow-up Plan', 'Converted', 'won', 'referral', 120000, 27, 5, NULL),
    (19, 31, '[MOCK] Completed Wellness Screening', 'Converted', 'won', 'walk_in', 145000, 26, 7, NULL),
    (20, 32, '[MOCK] Completed IV Care Package', 'Converted', 'won', 'campaign', 180000, 25, 9, NULL),
    (21, 33, '[MOCK] Completed Recovery Program', 'Converted', 'won', 'api', 225000, 24, 11, NULL),
    (22, 34, '[MOCK] Completed Metabolic Plan', 'Converted', 'won', 'referral', 278000, 23, 13, NULL),
    (23, 35, '[MOCK] Completed Postnatal Package', 'Converted', 'won', 'whatsapp', 360000, 22, 15, NULL),
    (24, 36, '[MOCK] Completed Preventive Package', 'Converted', 'won', 'other', 420000, 21, 17, NULL),
    (25, 37, '[MOCK] Completed Vitality Program', 'Converted', 'won', 'import', 495000, 20, 4, NULL),
    (26, 38, '[MOCK] Completed Skin Program', 'Converted', 'won', 'campaign', 540000, 19, 6, NULL),
    (27, 39, '[MOCK] Completed Hormone Program', 'Converted', 'won', 'referral', 620000, 18, 8, NULL),
    (28, 40, '[MOCK] Completed Executive Wellness', 'Converted', 'won', 'walk_in', 780000, 17, 10, NULL),
    (29, 1, '[MOCK] Completed Renewal Journey', 'Converted', 'won', 'whatsapp', 240000, 16, 12, NULL),
    (30, 2, '[MOCK] Completed Assessment Journey', 'Converted', 'won', 'api', 165000, 15, 2, NULL),
    (31, 3, '[MOCK] Deferred Wellness Program', 'Not proceeding', 'lost', 'campaign', 95000, 25, 4, 'Timing was not suitable; revisit next quarter.'),
    (32, 4, '[MOCK] Deferred Package Upgrade', 'Not proceeding', 'lost', 'referral', 180000, 23, 6, 'Chose to continue with current plan.'),
    (33, 5, '[MOCK] Deferred Executive Review', 'Not proceeding', 'lost', 'walk_in', 320000, 21, 8, 'Budget approval was postponed.'),
    (34, 6, '[MOCK] Deferred Metabolic Package', 'Not proceeding', 'lost', 'whatsapp', 450000, 19, 10, 'Requested follow-up after travel.'),
    (35, 7, '[MOCK] Deferred Recovery Bundle', 'Not proceeding', 'lost', 'other', 275000, 17, 12, 'Preferred a single-session option.'),
    (36, 8, '[MOCK] Deferred Vitality Plan', 'Not proceeding', 'lost', 'import', 520000, 15, 3, 'No response after the agreed follow-up window.');

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM crm_leads l
        JOIN _mock_leads ml
          ON l.id = md5('klinik-relive-crm-v2-lead-' || ml.seq)::uuid
        WHERE COALESCE(l.metadata->>'mock_dataset', '') NOT IN ('', 'klinik-relive-crm-v2')
    ) THEN
        RAISE EXCEPTION 'A deterministic lead ID is already owned by non-mock data';
    END IF;
END $$;

INSERT INTO crm_leads (
    id,
    organization_id,
    contact_id,
    pipeline_id,
    stage_id,
    title,
    status,
    owner_user_id,
    source,
    source_reference,
    value_minor,
    currency,
    next_action_at,
    expected_close_date,
    last_activity_at,
    won_at,
    lost_at,
    lost_reason,
    idempotency_key,
    metadata,
    version,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-lead-' || ml.seq)::uuid,
    ctx.organization_id,
    c.id,
    ctx.pipeline_id,
    stage.id,
    ml.title,
    ml.status,
    ctx.owner_id,
    ml.source,
    '[MOCK]-KR-SRC-' || lpad(ml.seq::text, 3, '0'),
    ml.value_minor,
    'MYR',
    CASE WHEN ml.status = 'open'
         THEN CURRENT_TIMESTAMP + make_interval(days => 1 + (ml.seq % 10))
         ELSE NULL END,
    CASE WHEN ml.status = 'open'
         THEN CURRENT_TIMESTAMP + make_interval(days => 7 + (ml.seq % 24))
         ELSE CURRENT_TIMESTAMP - make_interval(days => ml.outcome_days) END,
    CASE WHEN ml.status = 'open'
         THEN CURRENT_TIMESTAMP - make_interval(days => ml.seq % 5)
         ELSE CURRENT_TIMESTAMP - make_interval(days => ml.outcome_days) END,
    CASE WHEN ml.status = 'won'
         THEN CURRENT_TIMESTAMP - make_interval(days => ml.outcome_days)
         ELSE NULL END,
    CASE WHEN ml.status = 'lost'
         THEN CURRENT_TIMESTAMP - make_interval(days => ml.outcome_days)
         ELSE NULL END,
    COALESCE(ml.lost_reason, ''),
    '[MOCK]-KR-LEAD-' || lpad(ml.seq::text, 3, '0'),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'mock_key', 'lead-' || lpad(ml.seq::text, 3, '0'),
        'service_interest', CASE ml.seq % 4
            WHEN 0 THEN 'Metabolic wellness'
            WHEN 1 THEN 'Initial assessment'
            WHEN 2 THEN 'Follow-up care'
            ELSE 'Preventive wellness'
        END,
        'lead_score', 48 + ((ml.seq * 7) % 50),
        'branch', CASE ml.seq % 3 WHEN 0 THEN 'Kuala Lumpur' WHEN 1 THEN 'Petaling Jaya' ELSE 'Shah Alam' END,
        'campaign', CASE WHEN ml.source = 'campaign' THEN '[MOCK] Wellness July' ELSE 'Organic / direct' END,
        'budget_band', CASE ml.seq % 3 WHEN 0 THEN 'RM500-RM1,500' WHEN 1 THEN 'RM1,500-RM3,000' ELSE 'RM3,000+' END,
        'preferred_contact_time', CASE WHEN ml.seq % 2 = 0 THEN 'Morning' ELSE 'Evening' END,
        'demo_outcome', ml.status
    ),
    1,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => ml.created_days),
    CURRENT_TIMESTAMP
FROM _mock_leads ml
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(ml.contact_idx::text, 2, '0')
 AND c.deleted_at IS NULL
JOIN crm_pipeline_stages stage
  ON stage.organization_id = ctx.organization_id
 AND stage.pipeline_id = ctx.pipeline_id
 AND stage.name = ml.final_stage_name
 AND stage.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    pipeline_id = EXCLUDED.pipeline_id,
    stage_id = EXCLUDED.stage_id,
    title = EXCLUDED.title,
    status = EXCLUDED.status,
    owner_user_id = EXCLUDED.owner_user_id,
    source = EXCLUDED.source,
    source_reference = EXCLUDED.source_reference,
    value_minor = EXCLUDED.value_minor,
    currency = EXCLUDED.currency,
    next_action_at = EXCLUDED.next_action_at,
    expected_close_date = EXCLUDED.expected_close_date,
    last_activity_at = EXCLUDED.last_activity_at,
    won_at = EXCLUDED.won_at,
    lost_at = EXCLUDED.lost_at,
    lost_reason = EXCLUDED.lost_reason,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_by_id = EXCLUDED.updated_by_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

WITH ranked AS (
    SELECT
        l.id,
        row_number() OVER (ORDER BY l.created_at, l.id) AS rn
    FROM crm_leads l
    CROSS JOIN _kr_ctx ctx
    WHERE l.organization_id = ctx.organization_id
      AND l.title LIKE '[MOCK]%'
      AND l.deleted_at IS NULL
      AND l.id NOT IN (
          SELECT md5('klinik-relive-crm-v2-lead-' || seq)::uuid
          FROM _mock_leads
      )
)
UPDATE crm_leads l
SET
    owner_user_id = COALESCE(l.owner_user_id, ctx.owner_id),
    source = CASE ranked.rn % 7
        WHEN 0 THEN 'whatsapp'
        WHEN 1 THEN 'referral'
        WHEN 2 THEN 'walk_in'
        WHEN 3 THEN 'campaign'
        WHEN 4 THEN 'api'
        WHEN 5 THEN 'import'
        ELSE 'other'
    END,
    source_reference = CASE
        WHEN COALESCE(l.source_reference, '') = ''
        THEN '[MOCK]-KR-LEGACY-' || lpad(ranked.rn::text, 3, '0')
        ELSE l.source_reference
    END,
    next_action_at = CASE WHEN l.status = 'open'
        THEN COALESCE(l.next_action_at, CURRENT_TIMESTAMP + make_interval(days => 1 + (ranked.rn::int % 7)))
        ELSE l.next_action_at END,
    expected_close_date = CASE WHEN l.status = 'open'
        THEN COALESCE(l.expected_close_date, CURRENT_TIMESTAMP + make_interval(days => 10 + ranked.rn::int))
        ELSE l.expected_close_date END,
    last_activity_at = COALESCE(l.last_activity_at, CURRENT_TIMESTAMP - make_interval(days => ranked.rn::int % 4)),
    metadata = COALESCE(l.metadata, '{}'::jsonb) || jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'enriched_existing_demo', true,
        'lead_score', 60 + ((ranked.rn * 4) % 36),
        'preferred_contact_time', CASE WHEN ranked.rn % 2 = 0 THEN 'Morning' ELSE 'Evening' END,
        'budget_band', CASE ranked.rn % 3 WHEN 0 THEN 'RM500-RM1,500' WHEN 1 THEN 'RM1,500-RM3,000' ELSE 'RM3,000+' END
    ),
    version = GREATEST(l.version, 2),
    updated_by_id = ctx.owner_id,
    updated_at = CURRENT_TIMESTAMP
FROM ranked
CROSS JOIN _kr_ctx ctx
WHERE l.id = ranked.id;

CREATE TEMP TABLE _stage_path (
    sequence integer PRIMARY KEY,
    stage_name text NOT NULL
);

INSERT INTO _stage_path VALUES
    (1, 'New enquiry'),
    (2, 'Qualified'),
    (3, 'Appointment booked'),
    (4, 'Attended'),
    (5, 'Follow-up');

INSERT INTO crm_stage_history (
    id,
    organization_id,
    lead_id,
    from_stage_id,
    to_stage_id,
    changed_by_id,
    reason,
    metadata,
    changed_at
)
SELECT
    md5('klinik-relive-crm-v2-history-' || ml.seq || '-' || path.sequence)::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-lead-' || ml.seq)::uuid,
    previous_stage.id,
    current_stage.id,
    ctx.owner_id,
    CASE
        WHEN path.sequence = 1 THEN '[MOCK] Synthetic inquiry entered the demo pipeline.'
        ELSE '[MOCK] Demo qualification milestone completed.'
    END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'sequence', path.sequence
    ),
    CURRENT_TIMESTAMP - make_interval(days => ml.created_days)
        + make_interval(hours => (path.sequence - 1) * 18)
FROM _mock_leads ml
CROSS JOIN _kr_ctx ctx
JOIN _stage_path path
  ON path.sequence <= CASE
      WHEN ml.status IN ('won', 'lost') THEN 5
      WHEN ml.final_stage_name = 'New enquiry' THEN 1
      WHEN ml.final_stage_name = 'Qualified' THEN 2
      WHEN ml.final_stage_name = 'Appointment booked' THEN 3
      WHEN ml.final_stage_name = 'Attended' THEN 4
      ELSE 5
  END
JOIN crm_pipeline_stages current_stage
  ON current_stage.organization_id = ctx.organization_id
 AND current_stage.pipeline_id = ctx.pipeline_id
 AND current_stage.name = path.stage_name
 AND current_stage.deleted_at IS NULL
LEFT JOIN _stage_path previous_path
  ON previous_path.sequence = path.sequence - 1
LEFT JOIN crm_pipeline_stages previous_stage
  ON previous_stage.organization_id = ctx.organization_id
 AND previous_stage.pipeline_id = ctx.pipeline_id
 AND previous_stage.name = previous_path.stage_name
 AND previous_stage.deleted_at IS NULL
ON CONFLICT (id) DO NOTHING;

INSERT INTO crm_stage_history (
    id,
    organization_id,
    lead_id,
    from_stage_id,
    to_stage_id,
    changed_by_id,
    reason,
    metadata,
    changed_at
)
SELECT
    md5('klinik-relive-crm-v2-history-' || ml.seq || '-terminal')::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-lead-' || ml.seq)::uuid,
    followup_stage.id,
    terminal_stage.id,
    ctx.owner_id,
    CASE
        WHEN ml.status = 'won' THEN '[MOCK] Demo journey converted successfully.'
        ELSE '[MOCK] Demo journey marked not proceeding: ' || ml.lost_reason
    END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'terminal_status', ml.status
    ),
    CURRENT_TIMESTAMP - make_interval(days => ml.outcome_days)
FROM _mock_leads ml
CROSS JOIN _kr_ctx ctx
JOIN crm_pipeline_stages followup_stage
  ON followup_stage.organization_id = ctx.organization_id
 AND followup_stage.pipeline_id = ctx.pipeline_id
 AND followup_stage.name = 'Follow-up'
 AND followup_stage.deleted_at IS NULL
JOIN crm_pipeline_stages terminal_stage
  ON terminal_stage.organization_id = ctx.organization_id
 AND terminal_stage.pipeline_id = ctx.pipeline_id
 AND terminal_stage.name = ml.final_stage_name
 AND terminal_stage.deleted_at IS NULL
WHERE ml.status IN ('won', 'lost')
ON CONFLICT (id) DO NOTHING;

INSERT INTO crm_stage_history (
    id,
    organization_id,
    lead_id,
    from_stage_id,
    to_stage_id,
    changed_by_id,
    reason,
    metadata,
    changed_at
)
SELECT
    md5('klinik-relive-crm-v2-existing-history-' || l.id)::uuid,
    ctx.organization_id,
    l.id,
    NULL,
    l.stage_id,
    ctx.owner_id,
    '[MOCK] Existing demo opportunity recorded in its current stage.',
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'enriched_existing_demo', true
    ),
    l.created_at
FROM crm_leads l
CROSS JOIN _kr_ctx ctx
WHERE l.organization_id = ctx.organization_id
  AND l.title LIKE '[MOCK]%'
  AND l.deleted_at IS NULL
  AND l.id NOT IN (
      SELECT md5('klinik-relive-crm-v2-lead-' || seq)::uuid
      FROM _mock_leads
  )
  AND NOT EXISTS (
      SELECT 1
      FROM crm_stage_history h
      WHERE h.organization_id = l.organization_id
        AND h.lead_id = l.id
  )
ON CONFLICT (id) DO NOTHING;

CREATE TEMP TABLE _mock_tasks (
    seq integer PRIMARY KEY,
    contact_idx integer NOT NULL,
    lead_seq integer,
    title text NOT NULL,
    description text NOT NULL,
    status text NOT NULL,
    priority text NOT NULL,
    due_days integer,
    completed_days integer
);

INSERT INTO _mock_tasks VALUES
    (1, 14, 2, '[MOCK] Call Kelvin about consultation goals', 'Confirm goals and preferred appointment time.', 'open', 'urgent', -5, NULL),
    (2, 15, 3, '[MOCK] Confirm Nabila assessment readiness', 'Review outstanding questions before booking.', 'open', 'high', -4, NULL),
    (3, 16, 4, '[MOCK] Review Marcus wellness priorities', 'Prepare a concise program comparison.', 'open', 'urgent', -3, NULL),
    (4, 17, 5, '[MOCK] Send Kavitha internal package summary', 'Internal task only; customer communication remains human-approved.', 'open', 'high', -2, NULL),
    (5, 18, 6, '[MOCK] Check Syafiq pre-visit checklist', 'Verify that the assessment checklist is complete.', 'open', 'normal', -1, NULL),
    (6, 19, 7, '[MOCK] Record Grace preferred follow-up channel', 'Document preference before the next human follow-up.', 'open', 'normal', NULL, NULL),
    (7, 21, 9, '[MOCK] Book Joanne follow-up slot', 'Offer suitable follow-up times.', 'open', 'high', 1, NULL),
    (8, 22, 10, '[MOCK] Share Haris pricing breakdown internally', 'Prepare a package comparison for agent review.', 'open', 'normal', 3, NULL),
    (9, 23, 11, '[MOCK] Confirm Nadia availability', 'Confirm preferred weekday window.', 'open', 'normal', 5, NULL),
    (10, 20, 8, '[MOCK] Recover Faizal missed booking', 'Review the no-show recovery playbook and propose a new time.', 'in_progress', 'urgent', -2, NULL),
    (11, 24, 12, '[MOCK] Prepare Ethan care summary', 'Consolidate the demo journey before follow-up.', 'in_progress', 'high', 2, NULL),
    (12, 25, 13, '[MOCK] Validate Liyana invoice details', 'Check that demo invoice and package details align.', 'in_progress', 'normal', 4, NULL),
    (13, 26, 14, '[MOCK] Coordinate Dev package start', 'Prepare internal onboarding checklist.', 'in_progress', 'normal', 6, NULL),
    (14, 27, 15, '[MOCK] Complete Rachel consultation recap', 'Demo completed task.', 'completed', 'normal', -12, 11),
    (15, 28, 16, '[MOCK] Complete Zul package comparison', 'Demo completed task.', 'completed', 'high', -11, 10),
    (16, 29, 17, '[MOCK] Complete Hannah post-visit review', 'Demo completed task.', 'completed', 'normal', -10, 9),
    (17, 30, 18, '[MOCK] Complete Imran assessment checklist', 'Demo completed task.', 'completed', 'low', -9, 8),
    (18, 31, 19, '[MOCK] Complete Bella care plan summary', 'Demo completed task.', 'completed', 'normal', -8, 7),
    (19, 32, 20, '[MOCK] Complete Jasmin appointment notes', 'Demo completed task.', 'completed', 'normal', -7, 6),
    (20, 33, 21, '[MOCK] Complete Adam renewal review', 'Demo completed task.', 'completed', 'high', -6, 5),
    (21, 34, 22, '[MOCK] Complete Nurul package balance check', 'Demo completed task.', 'completed', 'normal', -5, 4),
    (22, 35, 23, '[MOCK] Cancel duplicate Ryan callback', 'Duplicate demo task cancelled safely.', 'cancelled', 'low', 2, NULL),
    (23, 13, 1, '[MOCK] Cancel superseded Aisyah checklist', 'Superseded demo task cancelled safely.', 'cancelled', 'low', 3, NULL);

INSERT INTO follow_up_tasks (
    id,
    organization_id,
    contact_id,
    lead_id,
    title,
    description,
    status,
    priority,
    owner_user_id,
    due_at,
    remind_at,
    completed_at,
    completed_by_id,
    source,
    idempotency_key,
    metadata,
    version,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-task-' || mt.seq)::uuid,
    ctx.organization_id,
    c.id,
    CASE WHEN mt.lead_seq IS NULL THEN NULL
         ELSE md5('klinik-relive-crm-v2-lead-' || mt.lead_seq)::uuid END,
    mt.title,
    mt.description,
    mt.status,
    mt.priority,
    ctx.owner_id,
    CASE WHEN mt.due_days IS NULL THEN NULL
         ELSE CURRENT_TIMESTAMP + make_interval(days => mt.due_days) END,
    NULL,
    CASE WHEN mt.completed_days IS NULL THEN NULL
         ELSE CURRENT_TIMESTAMP - make_interval(days => mt.completed_days) END,
    CASE WHEN mt.status = 'completed' THEN ctx.owner_id ELSE NULL END,
    'mock_demo',
    '[MOCK]-KR-TASK-' || lpad(mt.seq::text, 3, '0'),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'customer_message_required', false,
        'task_category', CASE mt.seq % 4 WHEN 0 THEN 'booking' WHEN 1 THEN 'commercial' WHEN 2 THEN 'care continuity' ELSE 'admin' END
    ),
    1,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => 16 + (mt.seq % 10)),
    CURRENT_TIMESTAMP
FROM _mock_tasks mt
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(mt.contact_idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    lead_id = EXCLUDED.lead_id,
    title = EXCLUDED.title,
    description = EXCLUDED.description,
    status = EXCLUDED.status,
    priority = EXCLUDED.priority,
    owner_user_id = EXCLUDED.owner_user_id,
    due_at = EXCLUDED.due_at,
    remind_at = NULL,
    completed_at = EXCLUDED.completed_at,
    completed_by_id = EXCLUDED.completed_by_id,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_by_id = EXCLUDED.updated_by_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _mock_bookings (
    seq integer PRIMARY KEY,
    contact_idx integer NOT NULL,
    status text NOT NULL,
    days_ago integer NOT NULL,
    service_name text NOT NULL,
    notes text NOT NULL
);

INSERT INTO _mock_bookings VALUES
    (1, 13, 'no_show', 22, 'Initial assessment', '[MOCK] Initial no-show later recovered by a completed visit.'),
    (2, 13, 'completed', 12, 'Follow-up appointment', '[MOCK] Recovery visit completed successfully.'),
    (3, 14, 'no_show', 19, 'Initial assessment', '[MOCK] No-show awaiting recovery.'),
    (4, 15, 'no_show', 16, 'Follow-up appointment', '[MOCK] No-show awaiting recovery.'),
    (5, 16, 'no_show', 13, 'Initial assessment', '[MOCK] No-show awaiting recovery.'),
    (6, 17, 'completed', 25, 'Initial assessment', '[MOCK] Completed historical assessment.'),
    (7, 18, 'completed', 24, 'Follow-up appointment', '[MOCK] Completed historical follow-up.'),
    (8, 19, 'completed', 23, 'Initial assessment', '[MOCK] Completed historical assessment.'),
    (9, 20, 'completed', 21, 'Follow-up appointment', '[MOCK] Completed historical follow-up.'),
    (10, 21, 'completed', 20, 'Initial assessment', '[MOCK] Completed historical assessment.'),
    (11, 22, 'completed', 18, 'Follow-up appointment', '[MOCK] Completed historical follow-up.'),
    (12, 23, 'completed', 17, 'Initial assessment', '[MOCK] Completed historical assessment.'),
    (13, 24, 'completed', 15, 'Follow-up appointment', '[MOCK] Completed historical follow-up.'),
    (14, 25, 'completed', 14, 'Initial assessment', '[MOCK] Completed historical assessment.'),
    (15, 26, 'completed', 11, 'Follow-up appointment', '[MOCK] Completed historical follow-up.'),
    (16, 27, 'completed', 9, 'Initial assessment', '[MOCK] Completed historical assessment.'),
    (17, 28, 'completed', 7, 'Follow-up appointment', '[MOCK] Completed historical follow-up.'),
    (18, 29, 'completed', 5, 'Initial assessment', '[MOCK] Completed historical assessment.'),
    (19, 30, 'cancelled', 10, 'Follow-up appointment', '[MOCK] Customer rescheduled outside the reporting window.'),
    (20, 31, 'cancelled', 8, 'Initial assessment', '[MOCK] Demo cancellation due to schedule conflict.'),
    (21, 32, 'cancelled', 6, 'Follow-up appointment', '[MOCK] Demo cancellation after package change.'),
    (22, 33, 'checked_in', 4, 'Initial assessment', '[MOCK] Demo attendee checked in; completion pending.'),
    (23, 34, 'confirmed', 3, 'Follow-up appointment', '[MOCK] Demo booking remains confirmed.'),
    (24, 35, 'reserved', 2, 'Initial assessment', '[MOCK] Demo slot remains reserved.');

INSERT INTO booking_events (
    id,
    organization_id,
    service_id,
    resource_id,
    starts_at,
    ends_at,
    capacity,
    status,
    location,
    metadata,
    version,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-booking-event-' || mb.seq)::uuid,
    ctx.organization_id,
    CASE WHEN mb.service_name = 'Initial assessment'
         THEN ctx.initial_service_id ELSE ctx.followup_service_id END,
    ctx.resource_id,
    CURRENT_TIMESTAMP - make_interval(days => mb.days_ago, hours => 2),
    CURRENT_TIMESTAMP - make_interval(days => mb.days_ago, hours => 1),
    1,
    CASE
        WHEN mb.status IN ('completed', 'no_show') THEN 'completed'
        WHEN mb.status = 'cancelled' THEN 'cancelled'
        ELSE 'scheduled'
    END,
    'Consultation Room 1',
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'calendar_label', '[MOCK] Historical clinic schedule'
    ),
    1,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => mb.days_ago + 3),
    CURRENT_TIMESTAMP
FROM _mock_bookings mb
CROSS JOIN _kr_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    service_id = EXCLUDED.service_id,
    resource_id = EXCLUDED.resource_id,
    starts_at = EXCLUDED.starts_at,
    ends_at = EXCLUDED.ends_at,
    status = EXCLUDED.status,
    location = EXCLUDED.location,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_by_id = EXCLUDED.updated_by_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO bookings (
    id,
    organization_id,
    event_id,
    contact_id,
    status,
    quantity,
    source,
    notes,
    booked_by_id,
    confirmed_at,
    checked_in_at,
    completed_at,
    no_show_at,
    cancelled_at,
    cancelled_by_id,
    cancellation_reason,
    idempotency_key,
    metadata,
    version,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-booking-' || mb.seq)::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-booking-event-' || mb.seq)::uuid,
    c.id,
    mb.status,
    1,
    CASE mb.seq % 4 WHEN 0 THEN 'agent' WHEN 1 THEN 'whatsapp' WHEN 2 THEN 'api' ELSE 'import' END,
    mb.notes,
    ctx.owner_id,
    CASE WHEN mb.status = 'reserved' THEN NULL
         ELSE CURRENT_TIMESTAMP - make_interval(days => mb.days_ago + 2) END,
    CASE WHEN mb.status IN ('checked_in', 'completed')
         THEN CURRENT_TIMESTAMP - make_interval(days => mb.days_ago, hours => 2) + interval '5 minutes'
         ELSE NULL END,
    CASE WHEN mb.status = 'completed'
         THEN CURRENT_TIMESTAMP - make_interval(days => mb.days_ago, hours => 1)
         ELSE NULL END,
    CASE WHEN mb.status = 'no_show'
         THEN CURRENT_TIMESTAMP - make_interval(days => mb.days_ago, hours => 1)
         ELSE NULL END,
    CASE WHEN mb.status = 'cancelled'
         THEN CURRENT_TIMESTAMP - make_interval(days => mb.days_ago + 1)
         ELSE NULL END,
    CASE WHEN mb.status = 'cancelled' THEN ctx.owner_id ELSE NULL END,
    CASE WHEN mb.status = 'cancelled' THEN '[MOCK] Demo schedule change.' ELSE '' END,
    '[MOCK]-KR-BOOKING-' || lpad(mb.seq::text, 3, '0'),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'notification_sent', false,
        'payment_collected', false
    ),
    1,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => mb.days_ago + 3),
    CURRENT_TIMESTAMP
FROM _mock_bookings mb
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(mb.contact_idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    event_id = EXCLUDED.event_id,
    contact_id = EXCLUDED.contact_id,
    status = EXCLUDED.status,
    quantity = EXCLUDED.quantity,
    source = EXCLUDED.source,
    notes = EXCLUDED.notes,
    booked_by_id = EXCLUDED.booked_by_id,
    confirmed_at = EXCLUDED.confirmed_at,
    checked_in_at = EXCLUDED.checked_in_at,
    completed_at = EXCLUDED.completed_at,
    no_show_at = EXCLUDED.no_show_at,
    cancelled_at = EXCLUDED.cancelled_at,
    cancelled_by_id = EXCLUDED.cancelled_by_id,
    cancellation_reason = EXCLUDED.cancellation_reason,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_by_id = EXCLUDED.updated_by_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _mock_packages (
    seq integer PRIMARY KEY,
    contact_idx integer NOT NULL,
    package_name text NOT NULL,
    status text NOT NULL,
    granted integer,
    reserved integer,
    consumed integer,
    available integer,
    starts_days_ago integer,
    expires_days integer,
    amount_minor bigint NOT NULL
);

INSERT INTO _mock_packages VALUES
    (1, 14, '[MOCK] Follow-up Care 4 Sessions', 'active', 4, 0, 3, 1, 25, 5, 120000),
    (2, 15, '[MOCK] Follow-up Care 4 Sessions', 'active', 4, 0, 2, 2, 22, 12, 120000),
    (3, 16, '[MOCK] Metabolic Reset 6 Sessions', 'active', 6, 0, 4, 2, 20, 35, 240000),
    (4, 17, '[MOCK] Follow-up Care 4 Sessions', 'active', 4, 0, 1, 3, 18, 50, 120000),
    (5, 18, '[MOCK] Metabolic Reset 6 Sessions', 'active', 6, 0, 2, 4, 16, 40, 240000),
    (6, 19, '[MOCK] Metabolic Reset 6 Sessions', 'active', 6, 0, 0, 6, 14, 70, 240000),
    (7, 20, '[MOCK] Follow-up Care 4 Sessions', 'active', 4, 0, 1, 3, 12, 80, 120000),
    (8, 21, '[MOCK] Follow-up Care 4 Sessions', 'active', 4, 0, 0, 4, 10, 60, 120000),
    (9, 22, '[MOCK] Follow-up Care 4 Sessions', 'expired', 4, 0, 4, 0, 80, -5, 120000),
    (10, 23, '[MOCK] Metabolic Reset 6 Sessions', 'exhausted', 6, 0, 6, 0, 60, 20, 240000),
    (11, 24, '[MOCK] Follow-up Care 4 Sessions', 'cancelled', 4, 0, 0, 4, 8, 50, 120000),
    (12, 25, '[MOCK] Metabolic Reset 6 Sessions', 'pending', NULL, NULL, NULL, NULL, NULL, NULL, 240000);

INSERT INTO contact_packages (
    id,
    organization_id,
    contact_id,
    package_definition_id,
    status,
    starts_at,
    expires_at,
    purchase_amount_minor,
    currency,
    source,
    idempotency_key,
    metadata,
    version,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-contact-package-' || mp.seq)::uuid,
    ctx.organization_id,
    c.id,
    CASE WHEN mp.package_name = '[MOCK] Metabolic Reset 6 Sessions'
         THEN ctx.metabolic_definition_id ELSE ctx.followup_definition_id END,
    mp.status,
    CASE WHEN mp.starts_days_ago IS NULL THEN NULL
         ELSE CURRENT_TIMESTAMP - make_interval(days => mp.starts_days_ago) END,
    CASE WHEN mp.expires_days IS NULL THEN NULL
         ELSE CURRENT_TIMESTAMP + make_interval(days => mp.expires_days) END,
    mp.amount_minor,
    'MYR',
    'mock_demo',
    '[MOCK]-KR-PACKAGE-' || lpad(mp.seq::text, 3, '0'),
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'collection_attempted', false,
        'package_label', mp.package_name
    ),
    1,
    ctx.owner_id,
    ctx.owner_id,
    CASE WHEN mp.starts_days_ago IS NULL
         THEN CURRENT_TIMESTAMP - interval '2 days'
         ELSE CURRENT_TIMESTAMP - make_interval(days => mp.starts_days_ago) END,
    CURRENT_TIMESTAMP
FROM _mock_packages mp
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(mp.contact_idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    package_definition_id = EXCLUDED.package_definition_id,
    status = EXCLUDED.status,
    starts_at = EXCLUDED.starts_at,
    expires_at = EXCLUDED.expires_at,
    purchase_amount_minor = EXCLUDED.purchase_amount_minor,
    currency = EXCLUDED.currency,
    source = EXCLUDED.source,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_by_id = EXCLUDED.updated_by_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO credit_balances (
    id,
    organization_id,
    contact_package_id,
    package_entitlement_id,
    granted,
    reserved,
    consumed,
    available,
    version,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-credit-balance-' || mp.seq)::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-contact-package-' || mp.seq)::uuid,
    CASE WHEN mp.package_name = '[MOCK] Metabolic Reset 6 Sessions'
         THEN ctx.metabolic_entitlement_id ELSE ctx.followup_entitlement_id END,
    mp.granted,
    mp.reserved,
    mp.consumed,
    mp.available,
    1,
    CASE WHEN mp.starts_days_ago IS NULL
         THEN CURRENT_TIMESTAMP - interval '2 days'
         ELSE CURRENT_TIMESTAMP - make_interval(days => mp.starts_days_ago) END,
    CURRENT_TIMESTAMP
FROM _mock_packages mp
CROSS JOIN _kr_ctx ctx
WHERE mp.granted IS NOT NULL
ON CONFLICT (id) DO UPDATE SET
    granted = EXCLUDED.granted,
    reserved = EXCLUDED.reserved,
    consumed = EXCLUDED.consumed,
    available = EXCLUDED.available,
    version = EXCLUDED.version,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO credit_ledger_entries (
    id,
    organization_id,
    contact_package_id,
    package_entitlement_id,
    type,
    delta,
    balance_after,
    idempotency_key,
    reason,
    actor_user_id,
    metadata,
    occurred_at,
    created_at
)
SELECT
    md5('klinik-relive-crm-v2-credit-grant-' || mp.seq)::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-contact-package-' || mp.seq)::uuid,
    CASE WHEN mp.package_name = '[MOCK] Metabolic Reset 6 Sessions'
         THEN ctx.metabolic_entitlement_id ELSE ctx.followup_entitlement_id END,
    'grant',
    mp.granted,
    mp.granted,
    '[MOCK]-KR-CREDIT-GRANT-' || lpad(mp.seq::text, 3, '0'),
    '[MOCK] Synthetic package grant for the CRM showcase.',
    ctx.owner_id,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true
    ),
    CASE WHEN mp.starts_days_ago IS NULL
         THEN CURRENT_TIMESTAMP - interval '2 days'
         ELSE CURRENT_TIMESTAMP - make_interval(days => mp.starts_days_ago) END,
    CURRENT_TIMESTAMP
FROM _mock_packages mp
CROSS JOIN _kr_ctx ctx
WHERE mp.granted IS NOT NULL
ON CONFLICT (id) DO NOTHING;

INSERT INTO credit_ledger_entries (
    id,
    organization_id,
    contact_package_id,
    package_entitlement_id,
    type,
    delta,
    balance_after,
    idempotency_key,
    reason,
    actor_user_id,
    metadata,
    occurred_at,
    created_at
)
SELECT
    md5('klinik-relive-crm-v2-credit-redeem-' || mp.seq)::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-contact-package-' || mp.seq)::uuid,
    CASE WHEN mp.package_name = '[MOCK] Metabolic Reset 6 Sessions'
         THEN ctx.metabolic_entitlement_id ELSE ctx.followup_entitlement_id END,
    CASE WHEN mp.status = 'expired' THEN 'expire' ELSE 'redeem' END,
    -mp.consumed,
    mp.available,
    '[MOCK]-KR-CREDIT-USE-' || lpad(mp.seq::text, 3, '0'),
    CASE WHEN mp.status = 'expired'
         THEN '[MOCK] Synthetic package expiry for the CRM showcase.'
         ELSE '[MOCK] Synthetic session usage for the CRM showcase.' END,
    ctx.owner_id,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true
    ),
    CURRENT_TIMESTAMP - make_interval(days => GREATEST(1, COALESCE(mp.starts_days_ago, 2) / 2)),
    CURRENT_TIMESTAMP
FROM _mock_packages mp
CROSS JOIN _kr_ctx ctx
WHERE mp.consumed IS NOT NULL
  AND mp.consumed > 0
ON CONFLICT (id) DO NOTHING;

INSERT INTO payment_provider_accounts (
    id,
    organization_id,
    name,
    provider,
    external_account_id,
    environment,
    api_key_encrypted,
    api_secret_encrypted,
    webhook_secret_encrypted,
    public_config,
    is_active,
    metadata,
    version,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-payment-account')::uuid,
    organization_id,
    '[MOCK] Offline Demo Ledger',
    'manual',
    'MOCK-KR-OFFLINE-LEDGER',
    'test',
    '',
    '',
    '',
    jsonb_build_object('mode', 'ledger_only', 'provider_connection', false),
    false,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'credentials_present', false,
        'collection_attempted', false
    ),
    1,
    owner_id,
    owner_id,
    CURRENT_TIMESTAMP - interval '40 days',
    CURRENT_TIMESTAMP
FROM _kr_ctx
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    provider = EXCLUDED.provider,
    external_account_id = EXCLUDED.external_account_id,
    environment = EXCLUDED.environment,
    api_key_encrypted = '',
    api_secret_encrypted = '',
    webhook_secret_encrypted = '',
    public_config = EXCLUDED.public_config,
    is_active = false,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

CREATE TEMP TABLE _mock_invoices (
    seq integer PRIMARY KEY,
    contact_idx integer NOT NULL,
    invoice_number text NOT NULL,
    status text NOT NULL,
    total_minor bigint NOT NULL,
    paid_minor bigint NOT NULL,
    issued_days integer NOT NULL,
    due_days integer NOT NULL,
    description text NOT NULL
);

INSERT INTO _mock_invoices VALUES
    (1, 21, 'MOCK-KR-2026-101', 'paid', 85000, 85000, 25, -18, '[MOCK] Initial assessment'),
    (2, 22, 'MOCK-KR-2026-102', 'paid', 120000, 120000, 23, -16, '[MOCK] Follow-up care package'),
    (3, 23, 'MOCK-KR-2026-103', 'paid', 145000, 145000, 21, -14, '[MOCK] Wellness screening'),
    (4, 24, 'MOCK-KR-2026-104', 'paid', 180000, 180000, 19, -12, '[MOCK] IV wellness plan'),
    (5, 25, 'MOCK-KR-2026-105', 'paid', 225000, 225000, 17, -10, '[MOCK] Recovery program'),
    (6, 26, 'MOCK-KR-2026-106', 'paid', 278000, 278000, 15, -8, '[MOCK] Metabolic care plan'),
    (7, 27, 'MOCK-KR-2026-107', 'open', 300000, 100000, 14, -3, '[MOCK] Partially paid vitality package'),
    (8, 28, 'MOCK-KR-2026-108', 'open', 90000, 0, 12, -5, '[MOCK] Overdue follow-up visit'),
    (9, 29, 'MOCK-KR-2026-109', 'open', 150000, 0, 10, -2, '[MOCK] Overdue care plan'),
    (10, 30, 'MOCK-KR-2026-110', 'open', 120000, 0, 3, 6, '[MOCK] Current follow-up package');

INSERT INTO commerce_invoices (
    id,
    organization_id,
    contact_id,
    invoice_number,
    idempotency_key,
    status,
    currency,
    subtotal_minor,
    discount_minor,
    tax_minor,
    total_minor,
    paid_minor,
    due_minor,
    issued_at,
    due_at,
    paid_at,
    metadata,
    version,
    created_by_id,
    updated_by_id,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-invoice-' || mi.seq)::uuid,
    ctx.organization_id,
    c.id,
    mi.invoice_number,
    '[MOCK]-KR-INVOICE-' || lpad(mi.seq::text, 3, '0'),
    mi.status,
    'MYR',
    mi.total_minor,
    0,
    0,
    mi.total_minor,
    mi.paid_minor,
    mi.total_minor - mi.paid_minor,
    CURRENT_TIMESTAMP - make_interval(days => mi.issued_days),
    CURRENT_TIMESTAMP + make_interval(days => mi.due_days),
    CASE WHEN mi.status = 'paid'
         THEN CURRENT_TIMESTAMP - make_interval(days => GREATEST(1, mi.issued_days - 3))
         ELSE NULL END,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'collection_attempted', false,
        'invoice_kind', CASE WHEN mi.paid_minor > 0 AND mi.status = 'open' THEN 'partial_demo' ELSE mi.status END
    ),
    1,
    ctx.owner_id,
    ctx.owner_id,
    CURRENT_TIMESTAMP - make_interval(days => mi.issued_days),
    CURRENT_TIMESTAMP
FROM _mock_invoices mi
CROSS JOIN _kr_ctx ctx
JOIN contacts c
  ON c.organization_id = ctx.organization_id
 AND c.phone_number = '6000000000' || lpad(mi.contact_idx::text, 2, '0')
 AND c.deleted_at IS NULL
ON CONFLICT (id) DO UPDATE SET
    contact_id = EXCLUDED.contact_id,
    invoice_number = EXCLUDED.invoice_number,
    status = EXCLUDED.status,
    subtotal_minor = EXCLUDED.subtotal_minor,
    discount_minor = EXCLUDED.discount_minor,
    tax_minor = EXCLUDED.tax_minor,
    total_minor = EXCLUDED.total_minor,
    paid_minor = EXCLUDED.paid_minor,
    due_minor = EXCLUDED.due_minor,
    issued_at = EXCLUDED.issued_at,
    due_at = EXCLUDED.due_at,
    paid_at = EXCLUDED.paid_at,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_by_id = EXCLUDED.updated_by_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO invoice_lines (
    id,
    organization_id,
    invoice_id,
    description,
    quantity,
    unit_amount_minor,
    subtotal_minor,
    tax_minor,
    total_minor,
    metadata,
    version,
    created_at,
    updated_at
)
SELECT
    md5('klinik-relive-crm-v2-invoice-line-' || mi.seq)::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-invoice-' || mi.seq)::uuid,
    mi.description,
    1,
    mi.total_minor,
    mi.total_minor,
    0,
    mi.total_minor,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true
    ),
    1,
    CURRENT_TIMESTAMP - make_interval(days => mi.issued_days),
    CURRENT_TIMESTAMP
FROM _mock_invoices mi
CROSS JOIN _kr_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    description = EXCLUDED.description,
    quantity = EXCLUDED.quantity,
    unit_amount_minor = EXCLUDED.unit_amount_minor,
    subtotal_minor = EXCLUDED.subtotal_minor,
    tax_minor = EXCLUDED.tax_minor,
    total_minor = EXCLUDED.total_minor,
    metadata = EXCLUDED.metadata,
    version = EXCLUDED.version,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

INSERT INTO payment_transactions (
    id,
    organization_id,
    provider_account_id,
    invoice_id,
    type,
    provider_transaction_id,
    provider_event_id,
    idempotency_key,
    amount_minor,
    currency,
    status,
    metadata,
    occurred_at,
    created_at
)
SELECT
    md5('klinik-relive-crm-v2-payment-' || mi.seq)::uuid,
    ctx.organization_id,
    md5('klinik-relive-crm-v2-payment-account')::uuid,
    md5('klinik-relive-crm-v2-invoice-' || mi.seq)::uuid,
    'charge',
    'MOCK-KR-TXN-' || lpad(mi.seq::text, 3, '0'),
    'MOCK-KR-EVENT-' || lpad(mi.seq::text, 3, '0'),
    '[MOCK]-KR-PAYMENT-' || lpad(mi.seq::text, 3, '0'),
    mi.paid_minor,
    'MYR',
    'succeeded',
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true,
        'ledger_only', true,
        'provider_contacted', false,
        'collection_attempted', false,
        'reference', '[MOCK] Offline demo receipt ' || lpad(mi.seq::text, 3, '0')
    ),
    CURRENT_TIMESTAMP - make_interval(days => GREATEST(1, mi.issued_days - 3)),
    CURRENT_TIMESTAMP - make_interval(days => GREATEST(1, mi.issued_days - 3))
FROM _mock_invoices mi
CROSS JOIN _kr_ctx ctx
WHERE mi.paid_minor > 0
ON CONFLICT (id) DO NOTHING;

CREATE TEMP TABLE _mock_widgets (
    seq integer PRIMARY KEY,
    name text NOT NULL,
    description text NOT NULL,
    data_source text NOT NULL,
    metric text NOT NULL,
    field_name text NOT NULL,
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

INSERT INTO _mock_widgets VALUES
    (1, '[MOCK] Open Opportunities', 'Current open Klinik Relive opportunities.', 'crm_leads', 'count', '', '[{"field":"status","operator":"equals","value":"open"}]', 'number', '', '', 'blue', 'small', 0, 6, 3, 3),
    (2, '[MOCK] Lead Status Funnel', 'Open, converted and not-proceeding opportunity mix.', 'crm_leads', 'count', '', '[]', 'chart', 'pie', 'status', 'purple', 'medium', 3, 6, 6, 5),
    (3, '[MOCK] Lead Source Mix', 'Where Klinik Relive demo opportunities originated.', 'crm_leads', 'count', '', '[]', 'chart', 'bar', 'source', 'green', 'medium', 9, 6, 3, 5),
    (4, '[MOCK] Lead Creation Trend', 'Opportunity activity over the selected period.', 'crm_leads', 'count', '', '[]', 'chart', 'line', 'status', 'blue', 'large', 0, 11, 8, 5),
    (5, '[MOCK] Recent Leads', 'Latest Klinik Relive opportunities.', 'crm_leads', 'count', '', '[]', 'table', '', '', 'gray', 'medium', 8, 11, 4, 5),
    (6, '[MOCK] Booking Outcomes', 'Completed, no-show, cancelled and active booking states.', 'bookings', 'count', '', '[]', 'chart', 'pie', 'status', 'orange', 'medium', 0, 16, 4, 5),
    (7, '[MOCK] Package Lifecycle', 'Active and terminal package states.', 'packages', 'count', '', '[]', 'chart', 'bar', 'status', 'green', 'medium', 4, 16, 4, 5),
    (8, '[MOCK] Invoice Status', 'Paid and outstanding invoice mix.', 'invoices', 'count', '', '[]', 'chart', 'pie', 'status', 'red', 'medium', 8, 16, 4, 5),
    (9, '[MOCK] Payment Activity', 'Credentials-free demo ledger activity.', 'payments', 'count', '', '[]', 'chart', 'line', 'type', 'purple', 'large', 0, 21, 8, 5),
    (10, '[MOCK] Recent Invoices', 'Latest Klinik Relive demo invoices.', 'invoices', 'count', '', '[]', 'table', '', '', 'gray', 'medium', 8, 21, 4, 5);

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
    md5('klinik-relive-crm-v2-widget-' || mw.seq)::uuid,
    ctx.organization_id,
    ctx.owner_id,
    mw.name,
    mw.description,
    mw.data_source,
    mw.metric,
    mw.field_name,
    mw.filters,
    mw.display_type,
    mw.chart_type,
    mw.group_by_field,
    true,
    mw.color,
    mw.size,
    20 + mw.seq,
    mw.grid_x,
    mw.grid_y,
    mw.grid_w,
    mw.grid_h,
    jsonb_build_object(
        'mock_dataset', 'klinik-relive-crm-v2',
        'is_mock', true
    ),
    true,
    false,
    CURRENT_TIMESTAMP,
    CURRENT_TIMESTAMP
FROM _mock_widgets mw
CROSS JOIN _kr_ctx ctx
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    data_source = EXCLUDED.data_source,
    metric = EXCLUDED.metric,
    field = EXCLUDED.field,
    filters = EXCLUDED.filters,
    display_type = EXCLUDED.display_type,
    chart_type = EXCLUDED.chart_type,
    group_by_field = EXCLUDED.group_by_field,
    show_change = EXCLUDED.show_change,
    color = EXCLUDED.color,
    size = EXCLUDED.size,
    display_order = EXCLUDED.display_order,
    grid_x = EXCLUDED.grid_x,
    grid_y = EXCLUDED.grid_y,
    grid_w = EXCLUDED.grid_w,
    grid_h = EXCLUDED.grid_h,
    config = EXCLUDED.config,
    is_shared = EXCLUDED.is_shared,
    is_default = EXCLUDED.is_default,
    updated_at = EXCLUDED.updated_at,
    deleted_at = NULL;

DO $$
DECLARE
    org uuid := 'c73f761f-5154-4fe1-9a13-06bae570277a';
    contacts_count bigint;
    open_count bigint;
    won_count bigint;
    lost_count bigint;
    open_value bigint;
    booking_total bigint;
    booking_completed bigint;
    booking_no_show bigint;
    booking_cancelled bigint;
    collected bigint;
    outstanding bigint;
    overdue_tasks bigint;
    overdue_invoices bigint;
    active_packages bigint;
    low_packages bigint;
    expiring_packages bigint;
    needs_follow_up bigint;
    no_show_recovery bigint;
    dormant_contacts bigint;
    open_opportunity_contacts bigint;
BEGIN
    SELECT count(*) INTO contacts_count
    FROM contacts
    WHERE organization_id = org AND deleted_at IS NULL;

    SELECT
        count(*) FILTER (WHERE status = 'open'),
        count(*) FILTER (WHERE status = 'won' AND won_at >= CURRENT_TIMESTAMP - interval '30 days'),
        count(*) FILTER (WHERE status = 'lost' AND lost_at >= CURRENT_TIMESTAMP - interval '30 days'),
        COALESCE(sum(value_minor) FILTER (WHERE status = 'open'), 0)
    INTO open_count, won_count, lost_count, open_value
    FROM crm_leads
    WHERE organization_id = org AND deleted_at IS NULL;

    SELECT
        count(*),
        count(*) FILTER (WHERE b.status = 'completed'),
        count(*) FILTER (WHERE b.status = 'no_show'),
        count(*) FILTER (WHERE b.status = 'cancelled')
    INTO booking_total, booking_completed, booking_no_show, booking_cancelled
    FROM bookings b
    JOIN booking_events be
      ON be.id = b.event_id
     AND be.organization_id = b.organization_id
     AND be.deleted_at IS NULL
    WHERE b.organization_id = org
      AND b.deleted_at IS NULL
      AND be.starts_at >= CURRENT_TIMESTAMP - interval '30 days'
      AND be.starts_at <= CURRENT_TIMESTAMP;

    SELECT COALESCE(sum(amount_minor), 0)
    INTO collected
    FROM payment_transactions
    WHERE organization_id = org
      AND status = 'succeeded'
      AND type = 'charge'
      AND occurred_at >= CURRENT_TIMESTAMP - interval '30 days';

    SELECT COALESCE(sum(due_minor), 0)
    INTO outstanding
    FROM commerce_invoices
    WHERE organization_id = org
      AND deleted_at IS NULL
      AND status = 'open'
      AND due_minor > 0;

    SELECT count(*) INTO overdue_tasks
    FROM follow_up_tasks
    WHERE organization_id = org
      AND deleted_at IS NULL
      AND status IN ('open', 'in_progress')
      AND due_at IS NOT NULL
      AND due_at < CURRENT_TIMESTAMP;

    SELECT count(*) INTO overdue_invoices
    FROM commerce_invoices
    WHERE organization_id = org
      AND deleted_at IS NULL
      AND status = 'open'
      AND due_minor > 0
      AND due_at < CURRENT_TIMESTAMP;

    SELECT count(*) INTO active_packages
    FROM contact_packages
    WHERE organization_id = org
      AND deleted_at IS NULL
      AND status = 'active';

    SELECT count(*) INTO low_packages
    FROM contact_packages cp
    WHERE cp.organization_id = org
      AND cp.deleted_at IS NULL
      AND cp.status = 'active'
      AND EXISTS (
          SELECT 1
          FROM credit_balances cb
          JOIN package_entitlements pe
            ON pe.id = cb.package_entitlement_id
           AND pe.organization_id = cb.organization_id
           AND pe.deleted_at IS NULL
          WHERE cb.organization_id = cp.organization_id
            AND cb.contact_package_id = cp.id
            AND cb.deleted_at IS NULL
            AND pe.is_unlimited = false
          GROUP BY cb.contact_package_id
          HAVING COALESCE(sum(cb.available), 0) <= 2
      );

    SELECT count(*) INTO expiring_packages
    FROM contact_packages
    WHERE organization_id = org
      AND deleted_at IS NULL
      AND status = 'active'
      AND expires_at > CURRENT_TIMESTAMP
      AND expires_at <= CURRENT_TIMESTAMP + interval '14 days';

    SELECT count(DISTINCT c.id) INTO needs_follow_up
    FROM contacts c
    WHERE c.organization_id = org
      AND c.deleted_at IS NULL
      AND EXISTS (
          SELECT 1
          FROM follow_up_tasks ft
          WHERE ft.organization_id = c.organization_id
            AND ft.contact_id = c.id
            AND ft.deleted_at IS NULL
            AND ft.status IN ('open', 'in_progress')
            AND (ft.due_at IS NULL OR ft.due_at <= CURRENT_TIMESTAMP)
      );

    SELECT count(DISTINCT c.id) INTO no_show_recovery
    FROM contacts c
    WHERE c.organization_id = org
      AND c.deleted_at IS NULL
      AND EXISTS (
          SELECT 1
          FROM bookings missed
          JOIN booking_events missed_event
            ON missed_event.id = missed.event_id
           AND missed_event.organization_id = missed.organization_id
           AND missed_event.deleted_at IS NULL
          WHERE missed.organization_id = c.organization_id
            AND missed.contact_id = c.id
            AND missed.deleted_at IS NULL
            AND missed.status = 'no_show'
            AND missed_event.starts_at >= CURRENT_TIMESTAMP - interval '90 days'
            AND NOT EXISTS (
                SELECT 1
                FROM bookings recovered
                JOIN booking_events recovered_event
                  ON recovered_event.id = recovered.event_id
                 AND recovered_event.organization_id = recovered.organization_id
                 AND recovered_event.deleted_at IS NULL
                WHERE recovered.organization_id = missed.organization_id
                  AND recovered.contact_id = missed.contact_id
                  AND recovered.deleted_at IS NULL
                  AND recovered.status IN ('reserved', 'confirmed', 'waitlisted', 'checked_in', 'completed')
                  AND recovered_event.starts_at > missed_event.starts_at
            )
      );

    SELECT count(*) INTO dormant_contacts
    FROM contacts c
    WHERE c.organization_id = org
      AND c.deleted_at IS NULL
      AND COALESCE(c.last_message_at, c.created_at) < CURRENT_TIMESTAMP - interval '60 days'
      AND NOT EXISTS (
          SELECT 1
          FROM bookings future_booking
          JOIN booking_events future_event
            ON future_event.id = future_booking.event_id
           AND future_event.organization_id = future_booking.organization_id
           AND future_event.deleted_at IS NULL
          WHERE future_booking.organization_id = c.organization_id
            AND future_booking.contact_id = c.id
            AND future_booking.deleted_at IS NULL
            AND future_booking.status IN ('reserved', 'confirmed', 'waitlisted', 'checked_in')
            AND future_event.starts_at >= CURRENT_TIMESTAMP
      );

    SELECT count(DISTINCT c.id) INTO open_opportunity_contacts
    FROM contacts c
    WHERE c.organization_id = org
      AND c.deleted_at IS NULL
      AND EXISTS (
          SELECT 1
          FROM crm_leads l
          WHERE l.organization_id = c.organization_id
            AND l.contact_id = c.id
            AND l.deleted_at IS NULL
            AND l.status = 'open'
      );

    IF contacts_count <> 40 THEN
        RAISE EXCEPTION 'Expected 40 contacts, found %', contacts_count;
    END IF;
    IF open_count <> 24 OR won_count <> 14 OR lost_count <> 6 OR open_value <> 7240000 THEN
        RAISE EXCEPTION 'Unexpected funnel: open %, won %, lost %, value %',
            open_count, won_count, lost_count, open_value;
    END IF;
    IF booking_total <> 24 OR booking_completed <> 14 OR booking_no_show <> 4 OR booking_cancelled <> 3 THEN
        RAISE EXCEPTION 'Unexpected bookings: total %, completed %, no-show %, cancelled %',
            booking_total, booking_completed, booking_no_show, booking_cancelled;
    END IF;
    IF collected <> 1133000 OR outstanding <> 780000 THEN
        RAISE EXCEPTION 'Unexpected commerce totals: collected %, outstanding %',
            collected, outstanding;
    END IF;
    IF overdue_tasks < 6 OR overdue_invoices < 3
       OR active_packages < 10 OR low_packages < 3 OR expiring_packages < 2 THEN
        RAISE EXCEPTION 'Missing minimum attention signals: tasks %, invoices %, active packages %, low %, expiring %',
            overdue_tasks, overdue_invoices, active_packages, low_packages, expiring_packages;
    END IF;
    IF needs_follow_up < 7 OR no_show_recovery < 3
       OR dormant_contacts < 5 OR open_opportunity_contacts < 24 THEN
        RAISE EXCEPTION 'Missing minimum segment counts: follow-up %, no-show %, dormant %, open opportunity %',
            needs_follow_up, no_show_recovery, dormant_contacts, open_opportunity_contacts;
    END IF;
END $$;

SELECT
    'VALIDATION'
    || '|contacts=' || (SELECT count(*) FROM contacts c WHERE c.organization_id = ctx.organization_id AND c.deleted_at IS NULL)
    || '|leads=' || (SELECT count(*) FROM crm_leads l WHERE l.organization_id = ctx.organization_id AND l.deleted_at IS NULL)
    || '|tasks=' || (SELECT count(*) FROM follow_up_tasks t WHERE t.organization_id = ctx.organization_id AND t.deleted_at IS NULL)
    || '|bookings=' || (SELECT count(*) FROM bookings b WHERE b.organization_id = ctx.organization_id AND b.deleted_at IS NULL)
    || '|packages=' || (SELECT count(*) FROM contact_packages cp WHERE cp.organization_id = ctx.organization_id AND cp.deleted_at IS NULL)
    || '|invoices=' || (SELECT count(*) FROM commerce_invoices ci WHERE ci.organization_id = ctx.organization_id AND ci.deleted_at IS NULL)
    || '|payments=' || (SELECT count(*) FROM payment_transactions pt WHERE pt.organization_id = ctx.organization_id)
    || '|widgets=' || (SELECT count(*) FROM widgets w WHERE w.organization_id = ctx.organization_id AND w.deleted_at IS NULL)
FROM _kr_ctx ctx;

ROLLBACK;
