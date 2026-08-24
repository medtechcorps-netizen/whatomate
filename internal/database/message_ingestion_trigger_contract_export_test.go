package database

import (
	"fmt"

	"gorm.io/gorm"
)

// PrepareFutureMessageCursorContractForTest creates only disposable test
// fixtures. The production compatibility bridge has no schema installer.
func PrepareFutureMessageCursorContractForTest(db *gorm.DB) error {
	if err := db.Exec(`
		ALTER TABLE public.messages
			ADD COLUMN IF NOT EXISTS ingested_at timestamptz;
		ALTER TABLE public.messages
			ALTER COLUMN ingested_at SET DEFAULT clock_timestamp();
		ALTER TABLE public.conversation_reads
			ADD COLUMN IF NOT EXISTS last_read_ingested_at timestamptz
	`).Error; err != nil {
		return fmt.Errorf("prepare future message-cursor test columns: %w", err)
	}
	for name, statement := range map[string]string{
		"message ingestion order":     messageIngestionOrderFunctionSQL,
		"conversation read order":     conversationReadIngestionOrderFunctionSQL,
		"deleted message read cursor": deletedMessageReadCursorCleanupFunctionSQL,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("create %s test function: %w", name, err)
		}
	}
	return nil
}

// InstallFutureMessageTriggersForTest installs either or both exact future
// Message triggers after PrepareFutureMessageCursorContractForTest.
func InstallFutureMessageTriggersForTest(db *gorm.DB, ingestionOrder, deleteCleanup bool) error {
	if ingestionOrder {
		if err := db.Exec(`
			CREATE TRIGGER trg_messages_ingestion_order
			BEFORE INSERT OR UPDATE OF inbox_conversation_id ON public.messages
			FOR EACH ROW
			EXECUTE FUNCTION public.rereply_set_message_ingestion_order()
		`).Error; err != nil {
			return fmt.Errorf("install future message ingestion-order test trigger: %w", err)
		}
	}
	if deleteCleanup {
		if err := db.Exec(`
			CREATE TRIGGER trg_messages_cleanup_read_cursors
			BEFORE DELETE ON public.messages
			FOR EACH ROW
			EXECUTE FUNCTION public.rereply_cleanup_deleted_message_read_cursors()
		`).Error; err != nil {
			return fmt.Errorf("install future deleted-message cleanup test trigger: %w", err)
		}
	}
	return nil
}

// InstallFutureConversationReadTriggerForTest installs the exact future
// ConversationRead trigger after PrepareFutureMessageCursorContractForTest.
func InstallFutureConversationReadTriggerForTest(db *gorm.DB) error {
	if err := db.Exec(`
		CREATE TRIGGER trg_conversation_reads_ingestion_order
		BEFORE INSERT OR UPDATE OF last_read_message_id, last_read_at
		ON public.conversation_reads
		FOR EACH ROW
		EXECUTE FUNCTION public.rereply_set_conversation_read_ingestion_order()
	`).Error; err != nil {
		return fmt.Errorf("install future conversation-read test trigger: %w", err)
	}
	return nil
}

// InstallFutureMessageCursorTriggersForTest installs the complete future
// trigger profile.
func InstallFutureMessageCursorTriggersForTest(db *gorm.DB) error {
	if err := InstallFutureMessageTriggersForTest(db, true, true); err != nil {
		return err
	}
	return InstallFutureConversationReadTriggerForTest(db)
}
