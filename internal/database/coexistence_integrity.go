package database

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// WhatsAppWAMIDOwnerMetadataKey and its canonical JSON object are written by
// the database trigger. The marker records historical ownership: later
// conversation/account reclassification cannot demote an owner or silently
// promote a provider-neutral message that was not admitted as an owner.
const (
	WhatsAppWAMIDOwnerMetadataKey  = "rereply_whatsapp_wamid_owner_v1"
	WhatsAppWAMIDOwnerMetadataJSON = `{"rereply_whatsapp_wamid_owner_v1":true}`
)

// ContactHasBlockingIdentityReviewHold is the transaction-safe, fail-closed
// policy read shared by AI, send, resume, and admission fences. Callers must
// treat a non-nil error as blocked; this function also returns true on error so
// it cannot accidentally be consumed as an allow result.
func ContactHasBlockingIdentityReviewHold(
	db *gorm.DB,
	organizationID, contactID uuid.UUID,
) (bool, error) {
	if db == nil || organizationID == uuid.Nil || contactID == uuid.Nil {
		return true, fmt.Errorf("identity-review policy requires database, organization, and contact")
	}
	var count int64
	if err := db.Table("whatsapp_identity_review_members AS member").
		Joins("JOIN whatsapp_identity_review_holds AS hold ON hold.organization_id = member.organization_id AND hold.id = member.hold_id").
		Where("member.organization_id = ? AND member.contact_id = ? AND hold.disposition = ?",
			organizationID, contactID, models.WhatsAppIdentityReviewDispositionOpen).
		Count(&count).Error; err != nil {
		return true, fmt.Errorf("read blocking identity-review hold: %w", err)
	}
	return count > 0, nil
}

// WhatsAppIdentityReviewWAMIDClaimed checks both durable admission stores while
// the caller owns the shared organization fence. A true result means the first
// durable owner wins and must not be promoted, replayed, moved, or recreated.
func WhatsAppIdentityReviewWAMIDClaimed(
	db *gorm.DB,
	organizationID uuid.UUID,
	wamid string,
) (bool, error) {
	wamid = strings.TrimSpace(wamid)
	if db == nil || organizationID == uuid.Nil || wamid == "" {
		return true, fmt.Errorf("identity-review WAMID lookup requires database, organization, and WAMID")
	}
	var messageCount int64
	if err := db.Unscoped().Model(&models.Message{}).
		Where(`messages.organization_id = ?
			AND BTRIM(messages.whats_app_message_id) = ?
			AND (
				messages.inbox_conversation_id IS NULL
				OR COALESCE(messages.metadata, '{}'::jsonb) @> ?::jsonb
			)`, organizationID, wamid, WhatsAppWAMIDOwnerMetadataJSON).
		Count(&messageCount).Error; err != nil {
		return true, fmt.Errorf("read Message WAMID owner: %w", err)
	}
	var stagedCount int64
	if err := db.Model(&models.InboundEvent{}).
		Where("organization_id = ? AND protocol = ? AND BTRIM(provider_event_id) = ?",
			organizationID, models.WhatsAppIdentityReviewInboundProtocol, wamid).
		Count(&stagedCount).Error; err != nil {
		return true, fmt.Errorf("read staged WAMID owner: %w", err)
	}
	return messageCount+stagedCount > 0, nil
}

const whatsappIdentityReviewHoldGuardFunctionBody = `BEGIN
	IF TG_OP = 'INSERT' THEN
		IF NEW.disposition = 'open'
		   AND NEW.version = 1
		   AND NEW.decision_target_contact_id IS NULL
		   AND NEW.decision_resolved_by_id IS NULL
		   AND NEW.decision_resolved_at IS NULL
		   AND NEW.decision_request_id IS NULL
		   AND NEW.decision_request_digest = ''
		   AND NEW.decision_chain_digest = ''
		   AND NEW.superseded_by_hold_id IS NULL
		   AND NEW.superseded_by_generation IS NULL
		   AND NEW.cycle_superseded_at IS NULL
		   AND NEW.superseded_by_onboarding_cycle IS NULL
		THEN
			RETURN NEW;
		END IF;
		RAISE EXCEPTION USING
			ERRCODE = '23514',
			MESSAGE = 'whatsapp identity-review holds must be inserted in the exact open state';
	END IF;
	IF TG_OP = 'DELETE' THEN
		RAISE EXCEPTION 'whatsapp identity-review holds cannot be deleted';
	END IF;
	IF OLD.disposition = 'open'
	   AND OLD.version = 1
	   AND NEW.version = 2
	   AND NEW.updated_at >= OLD.updated_at
	   AND NEW.id IS NOT DISTINCT FROM OLD.id
	   AND NEW.created_at IS NOT DISTINCT FROM OLD.created_at
	   AND NEW.organization_id IS NOT DISTINCT FROM OLD.organization_id
	   AND NEW.whats_app_account_id IS NOT DISTINCT FROM OLD.whats_app_account_id
	   AND NEW.onboarding_cycle IS NOT DISTINCT FROM OLD.onboarding_cycle
	   AND NEW.protocol_version IS NOT DISTINCT FROM OLD.protocol_version
	   AND NEW.supported IS NOT DISTINCT FROM OLD.supported
	   AND NEW.direct_primary_bsuid IS NOT DISTINCT FROM OLD.direct_primary_bsuid
	   AND NEW.parent_bsuid IS NOT DISTINCT FROM OLD.parent_bsuid
	   AND NEW.phone IS NOT DISTINCT FROM OLD.phone
	   AND NEW.principal_generation IS NOT DISTINCT FROM OLD.principal_generation
	   AND NEW.semantic_claim_digest IS NOT DISTINCT FROM OLD.semantic_claim_digest
	   AND NEW.selector_body_digest IS NOT DISTINCT FROM OLD.selector_body_digest
	   AND NEW.verified_event_digest IS NOT DISTINCT FROM OLD.verified_event_digest
	   AND NEW.verified_event_provenance IS NOT DISTINCT FROM OLD.verified_event_provenance
	   AND NEW.member_count IS NOT DISTINCT FROM OLD.member_count
	   AND NEW.member_digest IS NOT DISTINCT FROM OLD.member_digest
	   AND OLD.decision_target_contact_id IS NULL
	   AND OLD.decision_resolved_by_id IS NULL
	   AND OLD.decision_resolved_at IS NULL
	   AND OLD.decision_request_id IS NULL
	   AND OLD.decision_request_digest = ''
	   AND OLD.decision_chain_digest = ''
	   AND OLD.superseded_by_hold_id IS NULL
	   AND OLD.superseded_by_generation IS NULL
	   AND OLD.cycle_superseded_at IS NULL
	   AND OLD.superseded_by_onboarding_cycle IS NULL
	   AND (
		(
			NEW.disposition = 'superseded_by_cycle'
			AND NEW.decision_target_contact_id IS NULL
			AND NEW.decision_resolved_by_id IS NULL
			AND NEW.decision_resolved_at IS NULL
			AND NEW.decision_request_id IS NULL
			AND NEW.decision_request_digest = ''
			AND NEW.decision_chain_digest = ''
			AND NEW.superseded_by_hold_id IS NULL
			AND NEW.superseded_by_generation IS NULL
			AND NEW.cycle_superseded_at IS NOT NULL
			AND NEW.superseded_by_onboarding_cycle > NEW.onboarding_cycle
		)
		OR
		(
			NEW.decision_resolved_by_id IS NOT NULL
			AND NEW.decision_resolved_at IS NOT NULL
			AND NEW.decision_request_id IS NOT NULL
			AND NEW.decision_request_digest ~ '^[0-9a-f]{64}$'
			AND NEW.decision_chain_digest ~ '^[0-9a-f]{64}$'
			AND NEW.cycle_superseded_at IS NULL
			AND NEW.superseded_by_onboarding_cycle IS NULL
			AND (
				(
					NEW.disposition = 'future_routing'
					AND NEW.supported
					AND NEW.decision_target_contact_id IS NOT NULL
					AND NEW.superseded_by_hold_id IS NULL
					AND NEW.superseded_by_generation IS NULL
				)
				OR
				(
					NEW.disposition = 'superseded_by_latest'
					AND NEW.supported
					AND NEW.decision_target_contact_id IS NULL
					AND NEW.superseded_by_hold_id IS NOT NULL
					AND NEW.superseded_by_generation > NEW.principal_generation
					AND EXISTS (
						SELECT 1
						FROM public.whatsapp_identity_review_holds AS latest
						WHERE latest.organization_id = NEW.organization_id
						  AND latest.id = NEW.superseded_by_hold_id
						  AND latest.whats_app_account_id = NEW.whats_app_account_id
						  AND latest.onboarding_cycle = NEW.onboarding_cycle
						  AND latest.direct_primary_bsuid = NEW.direct_primary_bsuid
						  AND latest.principal_generation = NEW.superseded_by_generation
						  AND latest.disposition = 'future_routing'
						  AND latest.decision_request_id = NEW.decision_request_id
						  AND latest.decision_request_digest = NEW.decision_request_digest
						  AND latest.decision_chain_digest = NEW.decision_chain_digest
					)
				)
			)
		)
	   )
	THEN
		RETURN NEW;
	END IF;
	RAISE EXCEPTION 'whatsapp identity-review hold is immutable outside its first exact decision transition';
END;`

const whatsappIdentityReviewCycleSupersessionFunctionBody = `DECLARE
	closure_time timestamptz := pg_catalog.clock_timestamp();
BEGIN
	IF NEW.onboarding_cycle = OLD.onboarding_cycle THEN
		RETURN NEW;
	END IF;
	IF NEW.onboarding_cycle <> OLD.onboarding_cycle + 1 THEN
		RAISE EXCEPTION USING
			ERRCODE = '23514',
			MESSAGE = 'WhatsApp onboarding cycle must advance by exactly one';
	END IF;
	UPDATE public.whatsapp_identity_review_holds
	SET version = 2,
		disposition = 'superseded_by_cycle',
		cycle_superseded_at = closure_time,
		superseded_by_onboarding_cycle = NEW.onboarding_cycle,
		updated_at = closure_time
	WHERE organization_id = NEW.organization_id
	  AND whats_app_account_id = NEW.whats_app_account_id
	  AND onboarding_cycle < NEW.onboarding_cycle
	  AND disposition = 'open';
	RETURN NEW;
END;`

const whatsappIdentityReviewMemberGuardFunctionBody = `BEGIN
	RAISE EXCEPTION 'whatsapp identity-review membership is immutable';
END;`

const whatsappIdentityReviewCompletenessFunctionBody = `DECLARE
	target_organization_id uuid;
	target_hold_id uuid;
	expected_count bigint;
	expected_digest text;
	actual_count bigint;
	actual_digest text;
BEGIN
	IF TG_OP = 'DELETE' THEN
		target_organization_id := OLD.organization_id;
		IF TG_TABLE_NAME = 'whatsapp_identity_review_holds' THEN
			target_hold_id := OLD.id;
		ELSE
			target_hold_id := OLD.hold_id;
		END IF;
	ELSE
		target_organization_id := NEW.organization_id;
		IF TG_TABLE_NAME = 'whatsapp_identity_review_holds' THEN
			target_hold_id := NEW.id;
		ELSE
			target_hold_id := NEW.hold_id;
		END IF;
	END IF;
	SELECT hold.member_count, hold.member_digest
	INTO expected_count, expected_digest
	FROM public.whatsapp_identity_review_holds AS hold
	WHERE hold.organization_id = target_organization_id AND hold.id = target_hold_id;
	IF NOT FOUND THEN
		RAISE EXCEPTION 'whatsapp identity-review membership has no tenant-bound hold';
	END IF;
	SELECT pg_catalog.count(*), pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(COALESCE(
		pg_catalog.string_agg(member.contact_id::text || ':' || member.selector_reasons::text, E'\n' ORDER BY member.contact_id),
		''
	), 'UTF8')), 'hex')
	INTO actual_count, actual_digest
	FROM public.whatsapp_identity_review_members AS member
	WHERE member.organization_id = target_organization_id AND member.hold_id = target_hold_id;
	IF actual_count <> expected_count OR actual_digest <> expected_digest THEN
		RAISE EXCEPTION 'whatsapp identity-review membership is incomplete or noncanonical';
	END IF;
	RETURN NULL;
END;`

const whatsappIdentityReviewInboundGuardFunctionBody = `BEGIN
	IF TG_OP = 'INSERT' THEN
		IF NEW.protocol = 'whatsapp_identity_review_v1'
		   OR NEW.event_type = 'review_pending'
		   OR NEW.review_hold_id IS NOT NULL
		THEN
			IF NEW.payload ?| ARRAY['media_url', 'media_hydrated_id']
			   OR (NEW.payload ? 'media_status' AND NEW.payload -> 'media_status' <> '"pending"'::jsonb)
			THEN
				RAISE EXCEPTION 'reserved whatsapp identity-review events must be inserted before media hydration';
			END IF;
		END IF;
		RETURN NEW;
	END IF;
	IF TG_OP = 'DELETE' THEN
		IF OLD.protocol = 'whatsapp_identity_review_v1'
		   OR OLD.event_type = 'review_pending'
		   OR OLD.review_hold_id IS NOT NULL
		THEN
			RAISE EXCEPTION 'reserved whatsapp identity-review events cannot be deleted';
		END IF;
		RETURN OLD;
	END IF;
	IF OLD.protocol = 'whatsapp_identity_review_v1'
	   AND NEW.protocol = 'whatsapp_identity_review_v1'
	   AND OLD.event_type = 'review_pending'
	   AND NEW.event_type = 'review_pending'
	   AND OLD.review_hold_id IS NOT NULL
	   AND NEW.review_hold_id IS NOT NULL
	   AND OLD.payload -> 'media_status' = '"pending"'::jsonb
	   AND NEW.payload -> 'media_status' = '"ready"'::jsonb
	   AND NOT (OLD.payload ?| ARRAY['media_url', 'media_hydrated_id'])
	   AND pg_catalog.jsonb_typeof(NEW.payload -> 'media_url') = 'string'
	   AND pg_catalog.btrim(NEW.payload ->> 'media_url') <> ''
	   AND pg_catalog.jsonb_typeof(NEW.payload -> 'media_hydrated_id') = 'string'
	   AND NEW.payload ->> 'media_hydrated_id' = OLD.payload ->> 'media_id'
	   AND NEW.payload - ARRAY['media_status', 'media_url', 'media_hydrated_id']::text[]
	       = OLD.payload - ARRAY['media_status', 'media_url', 'media_hydrated_id']::text[]
	   AND pg_catalog.to_jsonb(NEW) - ARRAY['payload', 'updated_at']::text[]
	       = pg_catalog.to_jsonb(OLD) - ARRAY['payload', 'updated_at']::text[]
	   AND NEW.updated_at >= OLD.updated_at
	THEN
		RETURN NEW;
	END IF;
	IF OLD.protocol = 'whatsapp_identity_review_v1'
	   OR OLD.event_type = 'review_pending'
	   OR OLD.review_hold_id IS NOT NULL
	   OR NEW.protocol = 'whatsapp_identity_review_v1'
	   OR NEW.event_type = 'review_pending'
	   OR NEW.review_hold_id IS NOT NULL
	THEN
		RAISE EXCEPTION 'reserved whatsapp identity-review events are immutable';
	END IF;
	RETURN NEW;
END;`

const whatsappIdentityReviewWAMIDFenceNamespace = "rereply:whatsapp_wamid_owner:v1:"

// WhatsAppIdentityReviewWAMIDFenceKey is the exact length-framed advisory-lock
// identity used by the database triggers. It is exported for concurrency tests
// and diagnostics; application paths rely on the trigger as the final fence.
func WhatsAppIdentityReviewWAMIDFenceKey(organizationID uuid.UUID, wamid string) string {
	wamid = strings.TrimSpace(wamid)
	return whatsappIdentityReviewWAMIDFenceNamespace + organizationID.String() + ":" +
		strconv.Itoa(len(wamid)) + ":" + wamid
}

// LockWhatsAppWAMIDScopes acquires a complete message batch before any tenant,
// account, contact, or message row lock. Sorting the framed keys makes the
// order deterministic when two webhook batches overlap in reverse order.
func LockWhatsAppWAMIDScopes(tx *gorm.DB, organizationID uuid.UUID, wamids ...string) error {
	if tx == nil || organizationID == uuid.Nil {
		return fmt.Errorf("WhatsApp WAMID fence requires transaction and organization")
	}
	keysByValue := make(map[string]struct{}, len(wamids))
	for _, wamid := range wamids {
		wamid = strings.TrimSpace(wamid)
		if wamid == "" {
			return fmt.Errorf("WhatsApp WAMID fence requires non-empty identifiers")
		}
		keysByValue[WhatsAppIdentityReviewWAMIDFenceKey(organizationID, wamid)] = struct{}{}
	}
	keys := make([]string, 0, len(keysByValue))
	for key := range keysByValue {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := tx.Exec(
			"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
			key,
		).Error; err != nil {
			return fmt.Errorf("lock WhatsApp WAMID admission fence: %w", err)
		}
	}
	return nil
}

const whatsappIdentityReviewMessageWAMIDOwnerFunctionBody = `DECLARE
	target_wamid text := pg_catalog.btrim(COALESCE(NEW.whats_app_message_id, ''));
	target_is_whatsapp boolean;
	old_wamid text;
	old_is_owner boolean;
BEGIN
	IF TG_OP = 'DELETE' THEN
		old_wamid := pg_catalog.btrim(COALESCE(OLD.whats_app_message_id, ''));
		old_is_owner := old_wamid <> '' AND (
			OLD.inbox_conversation_id IS NULL
			OR COALESCE(OLD.metadata, '{}'::jsonb) @> '{"` + WhatsAppWAMIDOwnerMetadataKey + `":true}'::jsonb
		);
		IF NOT old_is_owner THEN
			RETURN OLD;
		END IF;
		RAISE EXCEPTION USING
			ERRCODE = '23514',
			MESSAGE = 'WhatsApp WAMID owners cannot be hard deleted';
	END IF;

	target_is_whatsapp := NEW.inbox_conversation_id IS NULL OR EXISTS (
		SELECT 1
		FROM public.inbox_conversations AS target_conversation
		JOIN public.channel_accounts AS target_account
		  ON target_account.organization_id = target_conversation.organization_id
		 AND target_account.id = target_conversation.channel_account_id
		WHERE target_conversation.organization_id = NEW.organization_id
		  AND target_conversation.id = NEW.inbox_conversation_id
		  AND target_conversation.channel = 'whatsapp'
		  AND target_account.channel = 'whatsapp'
		  AND target_account.provider = 'meta_legacy'
	);
	IF TG_OP = 'UPDATE' THEN
		IF OLD.organization_id IS DISTINCT FROM NEW.organization_id THEN
			RAISE EXCEPTION USING
				ERRCODE = '23514',
				MESSAGE = 'WhatsApp message tenant identity is immutable';
		END IF;
		old_wamid := pg_catalog.btrim(COALESCE(OLD.whats_app_message_id, ''));
		old_is_owner := old_wamid <> '' AND (
			OLD.inbox_conversation_id IS NULL
			OR COALESCE(OLD.metadata, '{}'::jsonb) @> '{"` + WhatsAppWAMIDOwnerMetadataKey + `":true}'::jsonb
		);
		IF old_is_owner THEN
			IF old_wamid IS DISTINCT FROM target_wamid THEN
				RAISE EXCEPTION USING
					ERRCODE = '23514',
					MESSAGE = 'non-empty WhatsApp message identity is immutable';
			END IF;
			NEW.metadata := pg_catalog.jsonb_set(
				COALESCE(NEW.metadata, '{}'::jsonb),
				ARRAY['` + WhatsAppWAMIDOwnerMetadataKey + `'],
				'true'::jsonb,
				true
			);
			RETURN NEW;
		END IF;
		IF NOT target_is_whatsapp OR (
			OLD.inbox_conversation_id IS NOT DISTINCT FROM NEW.inbox_conversation_id
			AND NOT (old_wamid = '' AND target_wamid <> '')
		) THEN
			NEW.metadata := COALESCE(NEW.metadata, '{}'::jsonb) - '` + WhatsAppWAMIDOwnerMetadataKey + `';
			RETURN NEW;
		END IF;
	ELSIF NOT target_is_whatsapp THEN
		NEW.metadata := COALESCE(NEW.metadata, '{}'::jsonb) - '` + WhatsAppWAMIDOwnerMetadataKey + `';
		RETURN NEW;
	END IF;
	IF target_wamid = '' THEN
		RETURN NEW;
	END IF;
	NEW.metadata := pg_catalog.jsonb_set(
		COALESCE(NEW.metadata, '{}'::jsonb),
		ARRAY['` + WhatsAppWAMIDOwnerMetadataKey + `'],
		'true'::jsonb,
		true
	);
	PERFORM 1
	FROM public.organizations
	WHERE id = NEW.organization_id
	FOR SHARE NOWAIT;
	IF NOT FOUND THEN
		RAISE EXCEPTION USING
			ERRCODE = '23503',
			MESSAGE = 'WhatsApp message organization fence is unavailable';
	END IF;
	IF NOT pg_catalog.pg_try_advisory_xact_lock(pg_catalog.hashtextextended(
		'` + whatsappIdentityReviewWAMIDFenceNamespace + `' || NEW.organization_id::text || ':' ||
		pg_catalog.octet_length(target_wamid)::text || ':' || target_wamid,
		0
	)) THEN
		RAISE EXCEPTION USING
			ERRCODE = '55P03',
			MESSAGE = 'WhatsApp WAMID admission is busy; retry the complete transaction';
	END IF;
	IF EXISTS (
		SELECT 1
		FROM public.messages AS owner
		WHERE owner.organization_id = NEW.organization_id
		  AND owner.id <> NEW.id
		  AND pg_catalog.btrim(owner.whats_app_message_id) = target_wamid
		  AND (
			owner.inbox_conversation_id IS NULL
			OR COALESCE(owner.metadata, '{}'::jsonb) @> '{"` + WhatsAppWAMIDOwnerMetadataKey + `":true}'::jsonb
		  )
	) THEN
		RAISE EXCEPTION USING
			ERRCODE = '23505',
			CONSTRAINT = 'uq_whatsapp_wamid_message_owner',
			MESSAGE = 'WhatsApp WAMID already belongs to another message';
	END IF;
	IF EXISTS (
		SELECT 1
		FROM public.inbound_events AS event
		WHERE event.organization_id = NEW.organization_id
		  AND event.protocol = 'whatsapp_identity_review_v1'
		  AND pg_catalog.btrim(event.provider_event_id) = target_wamid
	) THEN
		RAISE EXCEPTION USING
			ERRCODE = '23505',
			CONSTRAINT = 'uq_whatsapp_wamid_cross_store_owner',
			MESSAGE = 'whatsapp WAMID already belongs to the identity-review inbox';
	END IF;
	RETURN NEW;
END;`

const whatsappIdentityReviewEventWAMIDOwnerFunctionBody = `DECLARE
	target_wamid text := pg_catalog.btrim(COALESCE(NEW.provider_event_id, ''));
BEGIN
	IF TG_OP = 'UPDATE'
	   AND OLD.organization_id IS NOT DISTINCT FROM NEW.organization_id
	   AND OLD.protocol IS NOT DISTINCT FROM NEW.protocol
	   AND pg_catalog.btrim(COALESCE(OLD.provider_event_id, '')) IS NOT DISTINCT FROM target_wamid
	THEN
		RETURN NEW;
	END IF;
	IF NEW.protocol <> 'whatsapp_identity_review_v1' OR target_wamid = '' THEN
		RETURN NEW;
	END IF;
	PERFORM 1
	FROM public.organizations
	WHERE id = NEW.organization_id
	FOR SHARE NOWAIT;
	IF NOT FOUND THEN
		RAISE EXCEPTION USING
			ERRCODE = '23503',
			MESSAGE = 'identity-review event organization fence is unavailable';
	END IF;
	IF NOT pg_catalog.pg_try_advisory_xact_lock(pg_catalog.hashtextextended(
		'` + whatsappIdentityReviewWAMIDFenceNamespace + `' || NEW.organization_id::text || ':' ||
		pg_catalog.octet_length(target_wamid)::text || ':' || target_wamid,
		0
	)) THEN
		RAISE EXCEPTION USING
			ERRCODE = '55P03',
			MESSAGE = 'WhatsApp WAMID admission is busy; retry the complete transaction';
	END IF;
	IF EXISTS (
		SELECT 1
		FROM public.messages AS message
		WHERE message.organization_id = NEW.organization_id
		  AND pg_catalog.btrim(message.whats_app_message_id) = target_wamid
		  AND (
			message.inbox_conversation_id IS NULL
			OR COALESCE(message.metadata, '{}'::jsonb) @> '{"` + WhatsAppWAMIDOwnerMetadataKey + `":true}'::jsonb
		  )
	) THEN
		RAISE EXCEPTION USING
			ERRCODE = '23505',
			CONSTRAINT = 'uq_whatsapp_wamid_cross_store_owner',
			MESSAGE = 'whatsapp WAMID already belongs to a legacy message';
	END IF;
	RETURN NEW;
END;`

const whatsappIdentityReviewContactSelectorFenceNamespace = "rereply:whatsapp_identity_review_selector:v1:"

// WhatsAppIdentityReviewContactSelectorFenceKey is the exact lock namespace
// shared by identity-review admissions and the database trigger that fences
// every contact selector/merge mutation. The UUID is canonical and fixed-width,
// so distinct tenant identities cannot produce an ambiguous key encoding.
func WhatsAppIdentityReviewContactSelectorFenceKey(organizationID uuid.UUID) string {
	return whatsappIdentityReviewContactSelectorFenceNamespace + organizationID.String()
}

const whatsappIdentityReviewContactSelectorFenceFunctionBody = `DECLARE
	first_organization_id uuid;
	second_organization_id uuid;
BEGIN
	IF TG_OP = 'UPDATE'
	   AND OLD.id IS NOT DISTINCT FROM NEW.id
	   AND OLD.organization_id IS NOT DISTINCT FROM NEW.organization_id
	   AND OLD.phone_number IS NOT DISTINCT FROM NEW.phone_number
	   AND OLD.bs_uid IS NOT DISTINCT FROM NEW.bs_uid
	   AND OLD.merged_into_id IS NOT DISTINCT FROM NEW.merged_into_id
	   AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at
	THEN
		RETURN NEW;
	END IF;

	IF TG_OP = 'INSERT' THEN
		first_organization_id := NEW.organization_id;
	ELSIF TG_OP = 'DELETE' THEN
		first_organization_id := OLD.organization_id;
	ELSIF OLD.organization_id::text <= NEW.organization_id::text THEN
		first_organization_id := OLD.organization_id;
		IF NEW.organization_id IS DISTINCT FROM OLD.organization_id THEN
			second_organization_id := NEW.organization_id;
		END IF;
	ELSE
		first_organization_id := NEW.organization_id;
		second_organization_id := OLD.organization_id;
	END IF;

	PERFORM 1
	FROM public.organizations
	WHERE id = first_organization_id
	FOR UPDATE NOWAIT;
	IF NOT FOUND THEN
		RAISE EXCEPTION USING
			ERRCODE = '23503',
			MESSAGE = 'contact selector organization fence is unavailable';
	END IF;
	IF NOT pg_catalog.pg_try_advisory_xact_lock(pg_catalog.hashtextextended(
		'` + whatsappIdentityReviewContactSelectorFenceNamespace + `' || first_organization_id::text,
		0
	)) THEN
		RAISE EXCEPTION USING
			ERRCODE = '55P03',
			MESSAGE = 'contact selector admission is busy; retry the complete transaction';
	END IF;

	IF second_organization_id IS NOT NULL THEN
		PERFORM 1
		FROM public.organizations
		WHERE id = second_organization_id
		FOR UPDATE NOWAIT;
		IF NOT FOUND THEN
			RAISE EXCEPTION USING
				ERRCODE = '23503',
				MESSAGE = 'contact selector organization fence is unavailable';
		END IF;
		IF NOT pg_catalog.pg_try_advisory_xact_lock(pg_catalog.hashtextextended(
			'` + whatsappIdentityReviewContactSelectorFenceNamespace + `' || second_organization_id::text,
			0
		)) THEN
			RAISE EXCEPTION USING
				ERRCODE = '55P03',
				MESSAGE = 'contact selector admission is busy; retry the complete transaction';
		END IF;
	END IF;

	IF TG_OP = 'DELETE' THEN
		RETURN OLD;
	END IF;
	RETURN NEW;
END;`

// coexistenceIntegrityStatements installs the invariants that GORM's
// string-backed enums and independent belongs-to relations cannot express.
// In particular, the composite account foreign key prevents a tenant-owned
// state row from referring to another tenant's WhatsApp account.
func coexistenceCheckConstraint(name, table, expression string) string {
	return fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = '%s') THEN
			ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID;
		END IF;
	END $$`, name, table, name, expression)
}

var coexistenceCheckConstraintTables = []struct {
	name  string
	table string
}{
	{"chk_messages_whatsapp_message_id_trimmed", "messages"},
	{"chk_whatsapp_coexistence_onboarding_status", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_sync_status", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_contact_status", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_history_status", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_history_consent", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_lifecycle_status", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_attempts", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_history_progress", "whatsapp_coexistence_states"},
	{"chk_whatsapp_coexistence_version", "whatsapp_coexistence_states"},
	{"chk_whatsapp_identity_review_hold_authority", "whatsapp_identity_review_holds"},
	{"chk_whatsapp_identity_review_hold_decision", "whatsapp_identity_review_holds"},
	{"chk_whatsapp_identity_review_member_reason", "whatsapp_identity_review_members"},
	{"chk_inbound_events_identity_review_shape", "inbound_events"},
}

func coexistenceCheckConstraintValidationStatements() []string {
	statements := make([]string, 0, len(coexistenceCheckConstraintTables))
	for _, constraint := range coexistenceCheckConstraintTables {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE %s VALIDATE CONSTRAINT %s",
			constraint.table,
			constraint.name,
		))
	}
	return statements
}

func coexistenceNotValidForeignKeyStatement(foreignKey productTenantForeignKey) string {
	return fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint
			WHERE conname = '%s'
			  AND conrelid = 'public.%s'::regclass
		) THEN
			ALTER TABLE %s
			ADD CONSTRAINT %s
			FOREIGN KEY (%s)
			REFERENCES %s(%s)
			ON DELETE %s
			NOT VALID;
		END IF;
	END $$`,
		foreignKey.name,
		foreignKey.childTable,
		foreignKey.childTable,
		foreignKey.name,
		foreignKey.childCols,
		foreignKey.parentTable,
		foreignKey.parentCols,
		foreignKey.onDelete,
	)
}

func coexistenceIntegrityStatements() []string {
	statements := []string{
		// Detect an incumbent writer before any preparatory DDL can wait on the
		// session lock timeout.  The lock is released with this tiny statement;
		// the final activation repeats the same NOWAIT fence.
		`DO $block$ BEGIN
			LOCK TABLE public.contacts, public.channel_accounts, public.inbox_conversations,
				public.messages, public.inbound_events IN EXCLUSIVE MODE NOWAIT;
		END $block$`,
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_catalog.pg_index
				WHERE indexrelid = pg_catalog.to_regclass('public.uq_whatsapp_accounts_id_org')
				  AND NOT indisvalid
			) THEN
				DROP INDEX public.uq_whatsapp_accounts_id_org;
			END IF;
		END $$`,
		`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_whatsapp_accounts_id_org
			ON whatsapp_accounts(id, organization_id)`,
		// PostgreSQL leaves an INVALID relation behind if a concurrent unique
		// index build is interrupted or discovers a race-time duplicate. Remove
		// only that unusable artifact so a later migration can rebuild it.
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_index
				WHERE indexrelid = to_regclass('uq_messages_live_wamid')
				  AND NOT indisvalid
			) THEN
				DROP INDEX uq_messages_live_wamid;
			END IF;
		END $$`,
		// Live legacy WhatsApp delivery and Coexistence history/echo delivery can
		// race for the same WAMID. Provider-neutral channel messages also reuse the
		// legacy column for their external IDs, so constrain only legacy WhatsApp
		// rows (which have no inbox_conversation_id) rather than assuming IDs are
		// globally unique across unrelated providers.
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM messages
				WHERE deleted_at IS NULL
				  AND inbox_conversation_id IS NULL
				  AND BTRIM(whats_app_message_id) <> ''
				GROUP BY organization_id, BTRIM(whats_app_message_id)
				HAVING COUNT(*) > 1
			) THEN
				RAISE EXCEPTION 'cannot enforce unique live WhatsApp message IDs; resolve duplicate WAMIDs before retrying';
			END IF;
		END $$`,
		// Meta WAMIDs are immutable message identities. Scope them by tenant, not
		// by the mutable local account display name, so account renames cannot
		// turn an echo/edit/revoke/history replay into a second message.
		`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_messages_live_wamid
			ON messages(organization_id, (BTRIM(whats_app_message_id)))
			WHERE deleted_at IS NULL
			  AND inbox_conversation_id IS NULL
			  AND BTRIM(whats_app_message_id) <> ''`,
		coexistenceCheckConstraint(
			"chk_messages_whatsapp_message_id_trimmed",
			"messages",
			`(
				inbox_conversation_id IS NOT NULL
				AND NOT COALESCE(metadata, '{}'::jsonb) @> '{"`+WhatsAppWAMIDOwnerMetadataKey+`":true}'::jsonb
			) OR whats_app_message_id = BTRIM(whats_app_message_id)`,
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_onboarding_status",
			"whatsapp_coexistence_states",
			"onboarding_status IN ('pending', 'connected', 'syncing', 'ready', 'failed', 'expired', 'offboarded')",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_sync_status",
			"whatsapp_coexistence_states",
			"sync_status IN ('not_requested', 'pending', 'requesting', 'requested', 'in_progress', 'completed', 'failed', 'declined', 'expired')",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_contact_status",
			"whatsapp_coexistence_states",
			"contact_sync_status IN ('not_requested', 'pending', 'requesting', 'requested', 'in_progress', 'completed', 'failed', 'declined', 'expired')",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_history_status",
			"whatsapp_coexistence_states",
			"history_sync_status IN ('not_requested', 'pending', 'requesting', 'requested', 'in_progress', 'completed', 'failed', 'declined', 'expired')",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_history_consent",
			"whatsapp_coexistence_states",
			"history_consent IN ('unknown', 'granted', 'declined')",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_lifecycle_status",
			"whatsapp_coexistence_states",
			"lifecycle_status IN ('unknown', 'connected', 'disconnected', 'offboarded')",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_attempts",
			"whatsapp_coexistence_states",
			"contact_sync_attempts >= 0 AND history_sync_attempts >= 0",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_history_progress",
			"whatsapp_coexistence_states",
			"history_progress_percent BETWEEN 0 AND 100 AND (history_last_phase IS NULL OR history_last_phase >= 0) AND (history_last_chunk_order IS NULL OR history_last_chunk_order >= 0)",
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_coexistence_version",
			"whatsapp_coexistence_states",
			"version >= 1 AND onboarding_cycle >= 1",
		),
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_catalog.pg_index
				WHERE indexrelid = pg_catalog.to_regclass('public.uq_contacts_id_org')
				  AND NOT indisvalid
			) THEN
				DROP INDEX public.uq_contacts_id_org;
			END IF;
		END $$`,
		`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_contacts_id_org
			ON contacts(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_whatsapp_identity_review_holds_org_id
			ON whatsapp_identity_review_holds(organization_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_whatsapp_identity_review_unsupported_semantic_claim
			ON whatsapp_identity_review_holds(
				organization_id, whats_app_account_id, onboarding_cycle,
				protocol_version, semantic_claim_digest
			) WHERE NOT supported`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_whatsapp_identity_review_generation
			ON whatsapp_identity_review_holds(
				organization_id, whats_app_account_id, onboarding_cycle,
				direct_primary_bsuid, principal_generation
			) WHERE supported`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_whatsapp_identity_review_decision_request
			ON whatsapp_identity_review_holds(organization_id, decision_request_id)
			WHERE decision_request_id IS NOT NULL AND disposition = 'future_routing'`,
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_catalog.pg_index
				WHERE indexrelid = pg_catalog.to_regclass('public.uq_inbound_events_identity_review_wamid')
				  AND NOT indisvalid
			) THEN
				DROP INDEX public.uq_inbound_events_identity_review_wamid;
			END IF;
		END $$`,
		`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_inbound_events_identity_review_wamid
			ON inbound_events(organization_id, (BTRIM(provider_event_id)))
			WHERE protocol = 'whatsapp_identity_review_v1'`,
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_catalog.pg_index
				WHERE indexrelid = pg_catalog.to_regclass('public.idx_inbound_events_protocol')
				  AND NOT indisvalid
			) THEN
				DROP INDEX public.idx_inbound_events_protocol;
			END IF;
		END $$`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbound_events_protocol
			ON inbound_events(protocol)`,
		`DO $$ BEGIN
			IF EXISTS (
				SELECT 1
				FROM pg_catalog.pg_index
				WHERE indexrelid = pg_catalog.to_regclass('public.idx_inbound_events_review_hold_id')
				  AND NOT indisvalid
			) THEN
				DROP INDEX public.idx_inbound_events_review_hold_id;
			END IF;
		END $$`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbound_events_review_hold_id
			ON inbound_events(review_hold_id)`,
		coexistenceCheckConstraint(
			"chk_whatsapp_identity_review_hold_authority",
			"whatsapp_identity_review_holds",
			`onboarding_cycle >= 1
			AND protocol_version = 1
			AND semantic_claim_digest ~ '^[0-9a-f]{64}$'
			AND selector_body_digest ~ '^[0-9a-f]{64}$'
			AND verified_event_digest ~ '^[0-9a-f]{64}$'
			AND member_digest ~ '^[0-9a-f]{64}$'
			AND verified_event_provenance = 'meta_signed_webhook'
			AND direct_primary_bsuid = BTRIM(direct_primary_bsuid)
			AND parent_bsuid = BTRIM(parent_bsuid)
			AND phone ~ '^[0-9]*$'
			AND ((supported AND direct_primary_bsuid <> '' AND principal_generation >= 1)
				 OR (NOT supported AND principal_generation = 0))`,
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_identity_review_hold_decision",
			"whatsapp_identity_review_holds",
			`(
				disposition = 'open' AND version = 1
				AND decision_target_contact_id IS NULL
				AND decision_resolved_by_id IS NULL
				AND decision_resolved_at IS NULL
				AND decision_request_id IS NULL
				AND decision_request_digest = ''
				AND decision_chain_digest = ''
				AND superseded_by_hold_id IS NULL
				AND superseded_by_generation IS NULL
				AND cycle_superseded_at IS NULL
				AND superseded_by_onboarding_cycle IS NULL
			) OR (
				disposition = 'future_routing' AND version = 2 AND supported
				AND decision_target_contact_id IS NOT NULL
				AND decision_resolved_by_id IS NOT NULL
				AND decision_resolved_at IS NOT NULL
				AND decision_request_id IS NOT NULL
				AND decision_request_digest ~ '^[0-9a-f]{64}$'
				AND decision_chain_digest ~ '^[0-9a-f]{64}$'
				AND superseded_by_hold_id IS NULL
				AND superseded_by_generation IS NULL
				AND cycle_superseded_at IS NULL
				AND superseded_by_onboarding_cycle IS NULL
			) OR (
				disposition = 'superseded_by_latest' AND version = 2 AND supported
				AND decision_target_contact_id IS NULL
				AND decision_resolved_by_id IS NOT NULL
				AND decision_resolved_at IS NOT NULL
				AND decision_request_id IS NOT NULL
				AND decision_request_digest ~ '^[0-9a-f]{64}$'
				AND decision_chain_digest ~ '^[0-9a-f]{64}$'
				AND superseded_by_hold_id IS NOT NULL
				AND superseded_by_generation > principal_generation
				AND cycle_superseded_at IS NULL
				AND superseded_by_onboarding_cycle IS NULL
			) OR (
				disposition = 'superseded_by_cycle' AND version = 2
				AND decision_target_contact_id IS NULL
				AND decision_resolved_by_id IS NULL
				AND decision_resolved_at IS NULL
				AND decision_request_id IS NULL
				AND decision_request_digest = ''
				AND decision_chain_digest = ''
				AND superseded_by_hold_id IS NULL
				AND superseded_by_generation IS NULL
				AND cycle_superseded_at IS NOT NULL
				AND superseded_by_onboarding_cycle > onboarding_cycle
			)`,
		),
		coexistenceCheckConstraint(
			"chk_whatsapp_identity_review_member_reason",
			"whatsapp_identity_review_members",
			"selector_reasons BETWEEN 1 AND 7",
		),
		coexistenceCheckConstraint(
			"chk_inbound_events_identity_review_shape",
			"inbound_events",
			`(
				protocol = '' AND review_hold_id IS NULL AND event_type <> 'review_pending'
			) OR (
				protocol = 'whatsapp_identity_review_v1'
				AND review_hold_id IS NOT NULL
				AND event_type = 'review_pending'
				AND status = 'pending'
				AND provider_event_id IS NOT NULL
				AND BTRIM(provider_event_id) <> ''
				AND provider_event_id = BTRIM(provider_event_id)
				AND dedupe_key = 'identity-review:' || provider_event_id
				AND signature_valid
				AND headers = '{}'::jsonb
				AND jsonb_typeof(payload) = 'object'
				AND payload ?& ARRAY['schema_version', 'message_type', 'content']
				AND payload -> 'schema_version' = '1'::jsonb
				AND jsonb_typeof(payload -> 'message_type') = 'string'
				AND BTRIM(payload ->> 'message_type') <> ''
				AND jsonb_typeof(payload -> 'content') = 'string'
				AND payload - ARRAY[
					'schema_version', 'message_type', 'content', 'media_id',
					'media_sha256', 'media_generation', 'media_status',
					'media_mime_type', 'media_filename', 'media_revision',
					'media_url', 'media_hydrated_id'
				]::text[] = '{}'::jsonb
				AND (
					NOT (payload ?| ARRAY[
						'media_id', 'media_sha256', 'media_generation', 'media_status',
						'media_mime_type', 'media_filename', 'media_revision',
						'media_url', 'media_hydrated_id'
					])
					OR (
						payload ?& ARRAY[
							'media_id', 'media_sha256', 'media_generation',
							'media_status', 'media_revision'
						]
						AND jsonb_typeof(payload -> 'media_id') = 'string'
						AND BTRIM(payload ->> 'media_id') <> ''
						AND jsonb_typeof(payload -> 'media_sha256') = 'string'
						AND (
							(payload ->> 'media_sha256') = ''
							OR (payload ->> 'media_sha256') ~ '^[0-9a-f]{64}$'
						)
						AND payload -> 'media_generation' = '1'::jsonb
						AND jsonb_typeof(payload -> 'media_revision') = 'string'
						AND (payload ->> 'media_revision') ~ '^[0-9a-f]{64}$'
						AND (NOT (payload ? 'media_mime_type') OR jsonb_typeof(payload -> 'media_mime_type') = 'string')
						AND (NOT (payload ? 'media_filename') OR jsonb_typeof(payload -> 'media_filename') = 'string')
						AND (
							(
								payload -> 'media_status' = '"pending"'::jsonb
								AND NOT (payload ?| ARRAY['media_url', 'media_hydrated_id'])
							)
							OR (
								payload -> 'media_status' = '"ready"'::jsonb
								AND jsonb_typeof(payload -> 'media_url') = 'string'
								AND BTRIM(payload ->> 'media_url') <> ''
								AND jsonb_typeof(payload -> 'media_hydrated_id') = 'string'
								AND payload ->> 'media_hydrated_id' = payload ->> 'media_id'
							)
						)
					)
				)
				AND processing_started_at IS NULL
				AND processed_at IS NULL
				AND next_attempt_at IS NULL
				AND attempt_count = 0
				AND error_code IS NOT NULL
				AND error_message IS NOT NULL
				AND error_code = ''
				AND error_message = ''
			)`,
		),
		`CREATE OR REPLACE FUNCTION rereply_supersede_whatsapp_identity_review_prior_cycles()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $function$` + whatsappIdentityReviewCycleSupersessionFunctionBody + `$function$`,
		// Existing rows must already satisfy the signed membership commitment
		// before immutable/deferred guards become authoritative. Preflight, replace
		// all three functions, and replace all four triggers under the same pair of
		// locks so no writer can enter between validation and enforcement. NOWAIT
		// makes a concurrent writer a prompt, retryable migration failure.
		`DO $block$ DECLARE
			invalid_membership boolean;
		BEGIN
			LOCK TABLE public.whatsapp_identity_review_holds, public.whatsapp_identity_review_members
				IN SHARE ROW EXCLUSIVE MODE NOWAIT;
			SELECT EXISTS (
				SELECT 1
				FROM public.whatsapp_identity_review_holds AS hold
				LEFT JOIN LATERAL (
					SELECT COUNT(*) AS member_count,
						encode(sha256(convert_to(COALESCE(
							string_agg(member.contact_id::text || ':' || member.selector_reasons::text, E'\n'
								ORDER BY member.contact_id),
							''
						), 'UTF8')), 'hex') AS member_digest
					FROM public.whatsapp_identity_review_members AS member
					WHERE member.organization_id = hold.organization_id
					  AND member.hold_id = hold.id
				) AS actual ON true
				WHERE actual.member_count <> hold.member_count
				   OR actual.member_digest <> hold.member_digest
			) INTO invalid_membership;
			IF invalid_membership THEN
				RAISE EXCEPTION USING
					ERRCODE = '23514',
					MESSAGE = 'cannot install WhatsApp identity-review completeness guards; repair incomplete or noncanonical membership first';
			END IF;
			EXECUTE $ddl$CREATE OR REPLACE FUNCTION rereply_guard_whatsapp_identity_review_hold()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $function$` + whatsappIdentityReviewHoldGuardFunctionBody + `$function$$ddl$;
			EXECUTE $ddl$CREATE OR REPLACE FUNCTION rereply_guard_whatsapp_identity_review_member()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $function$` + whatsappIdentityReviewMemberGuardFunctionBody + `$function$$ddl$;
			EXECUTE $ddl$CREATE OR REPLACE FUNCTION rereply_verify_whatsapp_identity_review_complete()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $function$` + whatsappIdentityReviewCompletenessFunctionBody + `$function$$ddl$;
			DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_holds_guard
				ON public.whatsapp_identity_review_holds;
			CREATE TRIGGER trg_whatsapp_identity_review_holds_guard
				BEFORE INSERT OR UPDATE OR DELETE ON public.whatsapp_identity_review_holds
				FOR EACH ROW EXECUTE FUNCTION rereply_guard_whatsapp_identity_review_hold();
			DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_members_guard
				ON public.whatsapp_identity_review_members;
			CREATE TRIGGER trg_whatsapp_identity_review_members_guard
				BEFORE UPDATE OR DELETE ON public.whatsapp_identity_review_members
				FOR EACH ROW EXECUTE FUNCTION rereply_guard_whatsapp_identity_review_member();
			DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_holds_complete
				ON public.whatsapp_identity_review_holds;
			CREATE CONSTRAINT TRIGGER trg_whatsapp_identity_review_holds_complete
				AFTER INSERT OR UPDATE ON public.whatsapp_identity_review_holds
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION rereply_verify_whatsapp_identity_review_complete();
			DROP TRIGGER IF EXISTS trg_whatsapp_identity_review_members_complete
				ON public.whatsapp_identity_review_members;
			CREATE CONSTRAINT TRIGGER trg_whatsapp_identity_review_members_complete
				AFTER INSERT OR UPDATE OR DELETE ON public.whatsapp_identity_review_members
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION rereply_verify_whatsapp_identity_review_complete();
		END $block$`,
		`DO $block$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_trigger
				WHERE tgname = 'trg_whatsapp_coexistence_identity_review_cycle'
				  AND tgrelid = 'whatsapp_coexistence_states'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER trg_whatsapp_coexistence_identity_review_cycle
				AFTER UPDATE OF onboarding_cycle ON whatsapp_coexistence_states
				FOR EACH ROW EXECUTE FUNCTION rereply_supersede_whatsapp_identity_review_prior_cycles();
			END IF;
		END $block$`,
		`DO $block$ DECLARE
			owner_trigger_installed boolean;
			identity_review_trigger_count integer;
			correctly_bound_trigger_count integer;
		BEGIN
			-- This is the sole old-core activation boundary.  All four triggers
			-- become visible together or the statement rolls back completely, so
			-- an older compatibility binary never observes a mixed profile.
			LOCK TABLE public.contacts, public.channel_accounts, public.inbox_conversations,
				public.messages, public.inbound_events IN EXCLUSIVE MODE NOWAIT;
			SELECT
				COUNT(*)::integer,
				COUNT(*) FILTER (WHERE (
					(tgname = 'rereply_identity_review_contact_selector_fence'
					 AND tgrelid = 'public.contacts'::regclass
					 AND trigger_function.proname = 'rereply_lock_whatsapp_identity_review_contact_selector'
					 AND tgtype = 31)
					OR (tgname = 'rereply_identity_review_message_wamid_owner'
					 AND tgrelid = 'public.messages'::regclass
					 AND trigger_function.proname = 'rereply_guard_message_identity_review_wamid_owner'
					 AND tgtype = 31)
					OR (tgname = 'rereply_identity_review_event_wamid_owner'
					 AND tgrelid = 'public.inbound_events'::regclass
					 AND trigger_function.proname = 'rereply_guard_identity_review_event_wamid_owner'
					 AND tgtype = 23)
					OR (tgname = 'trg_inbound_events_identity_review_guard'
					 AND tgrelid = 'public.inbound_events'::regclass
					 AND trigger_function.proname = 'rereply_guard_whatsapp_identity_review_inbound_event'
					 AND tgtype = 31)
				) AND tgenabled = 'O'
				  AND tgnargs = 0
				  AND tgargs = ''::bytea
				  AND tgqual IS NULL
				  AND tgattr = ''::int2vector
				  AND trigger_function_namespace.nspname = 'public'
				)::integer
			INTO identity_review_trigger_count, correctly_bound_trigger_count
			FROM pg_catalog.pg_trigger AS trigger
			JOIN pg_catalog.pg_proc AS trigger_function
			  ON trigger_function.oid = trigger.tgfoid
			JOIN pg_catalog.pg_namespace AS trigger_function_namespace
			  ON trigger_function_namespace.oid = trigger_function.pronamespace
			WHERE NOT tgisinternal
			  AND tgname IN (
				'rereply_identity_review_contact_selector_fence',
				'rereply_identity_review_message_wamid_owner',
				'rereply_identity_review_event_wamid_owner',
				'trg_inbound_events_identity_review_guard'
			  );
			IF identity_review_trigger_count <> correctly_bound_trigger_count
			   OR correctly_bound_trigger_count NOT IN (0, 4) THEN
				RAISE EXCEPTION 'identity-review old-core trigger activation requires an exact absent or complete profile';
			END IF;
			owner_trigger_installed := correctly_bound_trigger_count = 4;
			IF NOT owner_trigger_installed AND EXISTS (
				SELECT 1
				FROM public.messages AS message
				WHERE COALESCE(message.metadata, '{}'::jsonb) ? '` + WhatsAppWAMIDOwnerMetadataKey + `'
			) THEN
				RAISE EXCEPTION 'reserved WhatsApp WAMID owner marker predates its authority trigger';
			END IF;
			IF NOT owner_trigger_installed THEN
			IF EXISTS (
				SELECT 1
				FROM public.messages AS message
				WHERE BTRIM(message.whats_app_message_id) <> ''
				  AND (
					COALESCE(message.metadata, '{}'::jsonb) @> '{"` + WhatsAppWAMIDOwnerMetadataKey + `":true}'::jsonb
					OR message.inbox_conversation_id IS NULL
					OR EXISTS (
						SELECT 1
						FROM public.inbox_conversations AS owner_conversation
						JOIN public.channel_accounts AS owner_account
						  ON owner_account.organization_id = owner_conversation.organization_id
						 AND owner_account.id = owner_conversation.channel_account_id
						WHERE owner_conversation.organization_id = message.organization_id
						  AND owner_conversation.id = message.inbox_conversation_id
						  AND owner_conversation.channel = 'whatsapp'
						  AND owner_account.channel = 'whatsapp'
						  AND owner_account.provider = 'meta_legacy'
					)
				  )
				GROUP BY message.organization_id, BTRIM(message.whats_app_message_id)
				HAVING COUNT(*) > 1
			) THEN
				RAISE EXCEPTION 'cannot enforce WhatsApp message ownership; resolve duplicate tenant WAMIDs before retrying';
			END IF;
			IF EXISTS (
				SELECT 1
				FROM public.messages AS message
				JOIN public.inbound_events AS event
				  ON event.organization_id = message.organization_id
				 AND event.protocol = 'whatsapp_identity_review_v1'
				 AND BTRIM(event.provider_event_id) = BTRIM(message.whats_app_message_id)
				WHERE BTRIM(message.whats_app_message_id) <> ''
				  AND (
					COALESCE(message.metadata, '{}'::jsonb) @> '{"` + WhatsAppWAMIDOwnerMetadataKey + `":true}'::jsonb
					OR message.inbox_conversation_id IS NULL
					OR EXISTS (
						SELECT 1
						FROM public.inbox_conversations AS owner_conversation
						JOIN public.channel_accounts AS owner_account
						  ON owner_account.organization_id = owner_conversation.organization_id
						 AND owner_account.id = owner_conversation.channel_account_id
						WHERE owner_conversation.organization_id = message.organization_id
						  AND owner_conversation.id = message.inbox_conversation_id
						  AND owner_conversation.channel = 'whatsapp'
						  AND owner_account.channel = 'whatsapp'
						  AND owner_account.provider = 'meta_legacy'
					)
				  )
			) THEN
				RAISE EXCEPTION 'cannot enforce cross-store WhatsApp admission ownership; resolve duplicate WAMIDs before retrying';
			END IF;
			UPDATE public.messages AS message
			SET metadata = pg_catalog.jsonb_set(
				COALESCE(message.metadata, '{}'::jsonb),
				ARRAY['` + WhatsAppWAMIDOwnerMetadataKey + `'],
				'true'::jsonb,
				true
			)
			WHERE BTRIM(message.whats_app_message_id) <> ''
			  AND NOT COALESCE(message.metadata, '{}'::jsonb) @> '{"` + WhatsAppWAMIDOwnerMetadataKey + `":true}'::jsonb
			  AND (
				message.inbox_conversation_id IS NULL
				OR EXISTS (
					SELECT 1
					FROM public.inbox_conversations AS owner_conversation
					JOIN public.channel_accounts AS owner_account
					  ON owner_account.organization_id = owner_conversation.organization_id
					 AND owner_account.id = owner_conversation.channel_account_id
					WHERE owner_conversation.organization_id = message.organization_id
					  AND owner_conversation.id = message.inbox_conversation_id
					  AND owner_conversation.channel = 'whatsapp'
					  AND owner_account.channel = 'whatsapp'
					  AND owner_account.provider = 'meta_legacy'
				)
			  );
			END IF;
			EXECUTE $ddl$CREATE OR REPLACE FUNCTION rereply_guard_whatsapp_identity_review_inbound_event()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $function$` + whatsappIdentityReviewInboundGuardFunctionBody + `$function$$ddl$;
			EXECUTE $ddl$CREATE OR REPLACE FUNCTION rereply_lock_whatsapp_identity_review_contact_selector()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $function$` + whatsappIdentityReviewContactSelectorFenceFunctionBody + `$function$$ddl$;
			EXECUTE $ddl$CREATE OR REPLACE FUNCTION rereply_guard_message_identity_review_wamid_owner()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $function$` + whatsappIdentityReviewMessageWAMIDOwnerFunctionBody + `$function$$ddl$;
			EXECUTE $ddl$CREATE OR REPLACE FUNCTION rereply_guard_identity_review_event_wamid_owner()
				RETURNS trigger
				LANGUAGE plpgsql
				AS $function$` + whatsappIdentityReviewEventWAMIDOwnerFunctionBody + `$function$$ddl$;
			DROP TRIGGER IF EXISTS trg_contacts_identity_review_selector_fence ON public.contacts;
			IF NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_trigger
				WHERE tgname = 'rereply_identity_review_contact_selector_fence'
				  AND tgrelid = 'public.contacts'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER rereply_identity_review_contact_selector_fence
				BEFORE INSERT OR UPDATE OR DELETE ON public.contacts
				FOR EACH ROW EXECUTE FUNCTION rereply_lock_whatsapp_identity_review_contact_selector();
			END IF;
			DROP TRIGGER IF EXISTS trg_messages_identity_review_wamid_owner ON public.messages;
			IF NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_trigger
				WHERE tgname = 'rereply_identity_review_message_wamid_owner'
				  AND tgrelid = 'public.messages'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER rereply_identity_review_message_wamid_owner
				BEFORE INSERT OR UPDATE OR DELETE ON public.messages
				FOR EACH ROW EXECUTE FUNCTION rereply_guard_message_identity_review_wamid_owner();
			END IF;
			DROP TRIGGER IF EXISTS trg_inbound_events_identity_review_wamid_owner ON public.inbound_events;
			IF NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_trigger
				WHERE tgname = 'rereply_identity_review_event_wamid_owner'
				  AND tgrelid = 'public.inbound_events'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER rereply_identity_review_event_wamid_owner
				BEFORE INSERT OR UPDATE ON public.inbound_events
				FOR EACH ROW EXECUTE FUNCTION rereply_guard_identity_review_event_wamid_owner();
			END IF;
			IF NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_trigger
				WHERE tgname = 'trg_inbound_events_identity_review_guard'
				  AND tgrelid = 'public.inbound_events'::regclass
				  AND NOT tgisinternal
			) THEN
				CREATE TRIGGER trg_inbound_events_identity_review_guard
				BEFORE INSERT OR UPDATE OR DELETE ON public.inbound_events
				FOR EACH ROW EXECUTE FUNCTION rereply_guard_whatsapp_identity_review_inbound_event();
			END IF;
		END $block$`,
	}
	if len(statements) == 0 {
		panic("coexistence integrity migration has no activation boundary")
	}
	activation := statements[len(statements)-1]
	statements = statements[:len(statements)-1]
	foreignKeys := []productTenantForeignKey{{
		name:        "fk_whatsapp_coexistence_account_tenant",
		childTable:  "whatsapp_coexistence_states",
		childCols:   "whats_app_account_id, organization_id",
		parentTable: "whatsapp_accounts",
		parentCols:  "id, organization_id",
		onDelete:    "CASCADE",
	}, {
		name:        "fk_whatsapp_identity_review_hold_account_tenant",
		childTable:  "whatsapp_identity_review_holds",
		childCols:   "whats_app_account_id, organization_id",
		parentTable: "whatsapp_accounts",
		parentCols:  "id, organization_id",
		onDelete:    "RESTRICT",
	}, {
		name:        "fk_whatsapp_identity_review_member_hold_tenant",
		childTable:  "whatsapp_identity_review_members",
		childCols:   "organization_id, hold_id",
		parentTable: "whatsapp_identity_review_holds",
		parentCols:  "organization_id, id",
		onDelete:    "RESTRICT",
	}, {
		name:        "fk_whatsapp_identity_review_member_contact_tenant",
		childTable:  "whatsapp_identity_review_members",
		childCols:   "contact_id, organization_id",
		parentTable: "contacts",
		parentCols:  "id, organization_id",
		onDelete:    "RESTRICT",
	}, {
		name:        "fk_whatsapp_identity_review_target_member",
		childTable:  "whatsapp_identity_review_holds",
		childCols:   "organization_id, id, decision_target_contact_id",
		parentTable: "whatsapp_identity_review_members",
		parentCols:  "organization_id, hold_id, contact_id",
		onDelete:    "RESTRICT",
	}, {
		name:        "fk_whatsapp_identity_review_superseded_tenant",
		childTable:  "whatsapp_identity_review_holds",
		childCols:   "organization_id, superseded_by_hold_id",
		parentTable: "whatsapp_identity_review_holds",
		parentCols:  "organization_id, id",
		onDelete:    "RESTRICT",
	}, {
		name:        "fk_inbound_events_identity_review_hold_tenant",
		childTable:  "inbound_events",
		childCols:   "organization_id, review_hold_id",
		parentTable: "whatsapp_identity_review_holds",
		parentCols:  "organization_id, id",
		onDelete:    "RESTRICT",
	}}
	for _, foreignKey := range foreignKeys {
		if foreignKey.name == "fk_inbound_events_identity_review_hold_tenant" {
			statements = append(statements, coexistenceNotValidForeignKeyStatement(foreignKey))
			continue
		}
		statements = append(statements, productTenantForeignKeyStatement(foreignKey))
	}
	// ADD CONSTRAINT NOT VALID takes only the brief catalog-change lock.  Each
	// validation is a separate statement, so PostgreSQL validates historical
	// rows with SHARE UPDATE EXCLUSIVE rather than retaining ACCESS EXCLUSIVE
	// for the duration of a full-table scan.
	statements = append(statements, coexistenceCheckConstraintValidationStatements()...)
	statements = append(statements,
		"ALTER TABLE public.inbound_events VALIDATE CONSTRAINT fk_inbound_events_identity_review_hold_tenant",
	)
	statements = append(statements, activation)
	return statements
}
