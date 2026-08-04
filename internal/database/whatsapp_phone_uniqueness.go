package database

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const GlobalWhatsAppPhoneIDIndex = "uq_whatsapp_accounts_phone_id_active"

type whatsAppPhoneDuplicate struct {
	PhoneID string
	Count   int64
}

type whatsAppPhoneIndexDefinition struct {
	IsUnique  bool
	IsValid   bool
	KeyCount  int
	KeyColumn string
	Predicate string
}

// EnsureGlobalWhatsAppPhoneIDUniqueness installs the cross-workspace ownership
// invariant for active WhatsApp accounts. It intentionally refuses to guess an
// owner when legacy duplicates exist; an operator must resolve those rows before
// the deployment can continue.
func EnsureGlobalWhatsAppPhoneIDUniqueness(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("install global WhatsApp phone ownership index: database is required")
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SET LOCAL lock_timeout = '30s'`).Error; err != nil {
			return fmt.Errorf("configure WhatsApp phone ownership migration lock: %w", err)
		}
		if err := tx.Exec(`LOCK TABLE public.whatsapp_accounts IN SHARE MODE`).Error; err != nil {
			return fmt.Errorf("lock WhatsApp accounts for ownership migration: %w", err)
		}

		var duplicates []whatsAppPhoneDuplicate
		if err := tx.Raw(`
			SELECT phone_id, COUNT(*) AS count
			FROM public.whatsapp_accounts
			WHERE deleted_at IS NULL
			GROUP BY phone_id
			HAVING COUNT(*) > 1
			ORDER BY COUNT(*) DESC, phone_id
			LIMIT 10
		`).Scan(&duplicates).Error; err != nil {
			return fmt.Errorf("check duplicate active WhatsApp phone ownership: %w", err)
		}
		if len(duplicates) > 0 {
			samples := make([]string, 0, len(duplicates))
			for _, duplicate := range duplicates {
				samples = append(samples, fmt.Sprintf("%q (%d active rows)", duplicate.PhoneID, duplicate.Count))
			}
			return fmt.Errorf(
				"cannot install global WhatsApp phone ownership index: resolve duplicate active phone IDs first: %s",
				strings.Join(samples, ", "),
			)
		}

		var existing whatsAppPhoneIndexDefinition
		result := tx.Raw(`
			SELECT
				i.indisunique AS is_unique,
				i.indisvalid AS is_valid,
				i.indnkeyatts AS key_count,
				a.attname AS key_column,
				COALESCE(pg_get_expr(i.indpred, i.indrelid), '') AS predicate
			FROM pg_catalog.pg_class AS idx
			JOIN pg_catalog.pg_namespace AS ns ON ns.oid = idx.relnamespace
			JOIN pg_catalog.pg_index AS i ON i.indexrelid = idx.oid
			JOIN pg_catalog.pg_class AS tbl ON tbl.oid = i.indrelid
			LEFT JOIN pg_catalog.pg_attribute AS a
				ON a.attrelid = tbl.oid
				AND a.attnum = i.indkey[0]
			WHERE ns.nspname = 'public'
			  AND tbl.relname = 'whatsapp_accounts'
			  AND idx.relname = ?
		`, GlobalWhatsAppPhoneIDIndex).Scan(&existing)
		if result.Error != nil {
			return fmt.Errorf("inspect global WhatsApp phone ownership index: %w", result.Error)
		}
		if result.RowsAffected > 0 {
			predicate := strings.Join(strings.Fields(strings.ToLower(existing.Predicate)), " ")
			if !existing.IsUnique || !existing.IsValid || existing.KeyCount != 1 || existing.KeyColumn != "phone_id" ||
				predicate != "(deleted_at is null)" {
				return fmt.Errorf(
					"index %s already exists with an unsafe definition; inspect and repair it before retrying",
					GlobalWhatsAppPhoneIDIndex,
				)
			}
			return nil
		}

		if err := tx.Exec(fmt.Sprintf(`
			CREATE UNIQUE INDEX %s
			ON public.whatsapp_accounts(phone_id)
			WHERE deleted_at IS NULL
		`, GlobalWhatsAppPhoneIDIndex)).Error; err != nil {
			return fmt.Errorf("create global WhatsApp phone ownership index: %w", err)
		}
		return nil
	})
}
