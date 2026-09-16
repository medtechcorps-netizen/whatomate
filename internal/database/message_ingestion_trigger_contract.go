package database

// These function definitions are shared by the migration installer and the
// platform-compliance allowlist. Keeping one source of truth makes the startup
// verifier compare the exact function body that migrations install.
const messageIngestionOrderFunctionSQL = `
	CREATE OR REPLACE FUNCTION rereply_set_message_ingestion_order()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $function$
	DECLARE
		next_ingested_at timestamptz;
	BEGIN
		IF NEW.inbox_conversation_id IS NOT NULL AND NEW.direction = 'incoming' THEN
			PERFORM 1
				FROM inbox_conversations
				WHERE organization_id = NEW.organization_id
				  AND id = NEW.inbox_conversation_id
				FOR UPDATE;
			IF NOT FOUND THEN
				RAISE EXCEPTION USING
					ERRCODE = '23503',
					MESSAGE = 'message inbox conversation is outside its organization';
			END IF;
			IF TG_OP = 'INSERT' OR
			   OLD.inbox_conversation_id IS DISTINCT FROM NEW.inbox_conversation_id THEN
				-- clock_timestamp() can move backwards and PostgreSQL timestamps can
				-- tie at microsecond precision. The conversation row above serializes
				-- this allocation; advance beyond every already allocated ingestion
				-- key and every active reader boundary so a later commit can never
				-- sort behind a completed acknowledgement. Existing NULL ingestion
				-- rows intentionally retain their provider/display created_at fallback
				-- until the later batched backfill.
				SELECT GREATEST(
					clock_timestamp(),
					COALESCE((
						SELECT MAX(existing_message.ingested_at) + interval '1 microsecond'
						FROM messages AS existing_message
						WHERE existing_message.organization_id = NEW.organization_id
						  AND existing_message.inbox_conversation_id = NEW.inbox_conversation_id
						  AND existing_message.ingested_at IS NOT NULL
					), '-infinity'::timestamptz),
					COALESCE((
						SELECT MAX(COALESCE(
							conversation_read.last_read_ingested_at,
							read_message.ingested_at,
							read_message.created_at,
							conversation_read.last_read_at
						)) + interval '1 microsecond'
						FROM conversation_reads AS conversation_read
						LEFT JOIN messages AS read_message
						  ON read_message.id = conversation_read.last_read_message_id
						 AND read_message.organization_id = conversation_read.organization_id
						 AND read_message.inbox_conversation_id = conversation_read.conversation_id
						WHERE conversation_read.organization_id = NEW.organization_id
						  AND conversation_read.conversation_id = NEW.inbox_conversation_id
					), '-infinity'::timestamptz)
				)
				INTO next_ingested_at;
				NEW.ingested_at = next_ingested_at;
			END IF;
		ELSIF TG_OP = 'INSERT' AND NEW.ingested_at IS NULL THEN
			NEW.ingested_at = clock_timestamp();
		END IF;
		RETURN NEW;
	END
	$function$
`

const conversationReadIngestionOrderFunctionSQL = `
	CREATE OR REPLACE FUNCTION rereply_set_conversation_read_ingestion_order()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $function$
	DECLARE
		message_ingested_at timestamptz;
		old_ingested_at timestamptz;
	BEGIN
		-- A referential-action UPDATE runs while the deleting transaction
		-- already owns the message row. Handle it before acquiring the
		-- conversation lock so normal conversation->message readers cannot
		-- deadlock against message->conversation FK cleanup.
		IF TG_OP = 'UPDATE' AND
		   NEW.last_read_message_id IS NULL AND
		   OLD.last_read_message_id IS NOT NULL THEN
			IF pg_trigger_depth() >= 2 AND
			   current_setting('rereply.message_cursor_cleanup', true) =
			   CAST(OLD.last_read_message_id AS text) THEN
				-- The PG14-compatible message BEFORE DELETE trigger clears only
				-- this nullable FK column before the tenant-composite RESTRICT
				-- constraint is checked. Preserve the durable ordering boundary
				-- without acquiring Conversation behind the deleting Message row.
				NEW.last_read_external_id = OLD.last_read_external_id;
				NEW.last_read_ingested_at = COALESCE(
					OLD.last_read_ingested_at,
					OLD.last_read_at
				);
				NEW.last_read_at = OLD.last_read_at;
				RETURN NEW;
			END IF;
			-- Older replicas may already own ConversationRead before entering
			-- this trigger. Never wait on a Message that a hard delete owns:
			-- its cleanup is waiting for this same cursor row. A 55P03 abort is
			-- retryable and releases the row without a deadlock cycle.
			PERFORM 1
			FROM messages AS retained_message
			WHERE retained_message.id = OLD.last_read_message_id
			  AND retained_message.organization_id = OLD.organization_id
			  AND retained_message.inbox_conversation_id = OLD.conversation_id
			FOR KEY SHARE NOWAIT;
			IF NOT FOUND THEN
				-- ON DELETE SET NULL must be allowed to clear the FK, but
				-- retains the former ingestion boundary. A NULL tie-break is
				-- deliberately conservative and can only re-show equal-time
				-- rows; it cannot hide an unread row.
				NEW.last_read_external_id = OLD.last_read_external_id;
				NEW.last_read_ingested_at = COALESCE(
					OLD.last_read_ingested_at,
					OLD.last_read_at
				);
				NEW.last_read_at = OLD.last_read_at;
				RETURN NEW;
			END IF;
		END IF;

		PERFORM 1
		FROM inbox_conversations
		WHERE organization_id = NEW.organization_id
		  AND id = NEW.conversation_id
		-- New code already owns this row before touching ConversationRead.
		-- A rolling old writer owns ConversationRead first, so it must fail
		-- fast rather than wait and invert the new Conversation -> Message ->
		-- ConversationRead lock order.
		FOR UPDATE NOWAIT;
		IF NOT FOUND THEN
			RAISE EXCEPTION USING
				ERRCODE = '23503',
				MESSAGE = 'conversation read is outside its organization';
		END IF;

		IF NEW.last_read_message_id IS NULL THEN
			IF TG_OP = 'UPDATE' THEN
				-- An older replica can select an empty conversation before an
				-- inbound writer commits, then reach this trigger afterwards.
				-- A targetless update must not jump past that unseen row.
				NEW.last_read_message_id = OLD.last_read_message_id;
				NEW.last_read_external_id = OLD.last_read_external_id;
				NEW.last_read_ingested_at = OLD.last_read_ingested_at;
				NEW.last_read_at = OLD.last_read_at;
			ELSE
				-- A first read of a genuinely empty conversation establishes a
				-- safe pre-history cursor; future (or concurrently committed)
				-- incoming messages remain unread.
				IF NEW.last_read_ingested_at IS NULL THEN
					-- Old readers do not know the ingestion column and still compare
					-- provider/display created_at with last_read_at. Preserve their
					-- safety during the rolling window as well.
					NEW.last_read_at = 'epoch'::timestamptz;
				END IF;
				NEW.last_read_ingested_at = 'epoch'::timestamptz;
			END IF;
		ELSE
			-- The new handler locks Message before ConversationRead. NOWAIT also
			-- protects rolling old writers that reach this trigger without that
			-- application-side prefix.
			SELECT COALESCE(message.ingested_at, message.created_at)
			INTO message_ingested_at
			FROM messages AS message
			WHERE message.id = NEW.last_read_message_id
			  AND message.organization_id = NEW.organization_id
			  AND message.inbox_conversation_id = NEW.conversation_id
			FOR KEY SHARE NOWAIT;
			IF NOT FOUND THEN
				RAISE EXCEPTION USING
					ERRCODE = '23503',
					MESSAGE = 'conversation read message is outside its conversation';
			END IF;
			NEW.last_read_ingested_at = message_ingested_at;
		END IF;

		IF TG_OP = 'UPDATE' THEN
			old_ingested_at := COALESCE(OLD.last_read_ingested_at, OLD.last_read_at);
			IF OLD.last_read_ingested_at IS NULL AND OLD.last_read_message_id IS NOT NULL THEN
				SELECT COALESCE(message.ingested_at, message.created_at)
				INTO old_ingested_at
				FROM messages AS message
				WHERE message.id = OLD.last_read_message_id
				  AND message.organization_id = OLD.organization_id
				  AND message.inbox_conversation_id = OLD.conversation_id
				FOR KEY SHARE NOWAIT;
			END IF;
			IF NEW.last_read_ingested_at < old_ingested_at OR (
				NEW.last_read_ingested_at = old_ingested_at AND
				OLD.last_read_message_id IS NOT NULL AND (
					NEW.last_read_message_id IS NULL OR
					NEW.last_read_message_id < OLD.last_read_message_id
				)
			) THEN
				NEW.last_read_message_id = OLD.last_read_message_id;
				NEW.last_read_external_id = OLD.last_read_external_id;
				NEW.last_read_ingested_at = old_ingested_at;
				NEW.last_read_at = OLD.last_read_at;
			END IF;
		END IF;
		RETURN NEW;
	END
	$function$
`

const deletedMessageReadCursorCleanupFunctionSQL = `
	CREATE OR REPLACE FUNCTION rereply_cleanup_deleted_message_read_cursors()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $function$
	BEGIN
		PERFORM set_config(
			'rereply.message_cursor_cleanup',
			CAST(OLD.id AS text),
			true
		);
		UPDATE conversation_reads
		SET last_read_message_id = NULL,
			updated_at = clock_timestamp()
		WHERE organization_id = OLD.organization_id
		  AND last_read_message_id = OLD.id;
		PERFORM set_config('rereply.message_cursor_cleanup', '', true);
		RETURN OLD;
	EXCEPTION WHEN OTHERS THEN
		PERFORM set_config('rereply.message_cursor_cleanup', '', true);
		RAISE;
	END
	$function$
`
