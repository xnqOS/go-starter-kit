package outbox

import "gorm.io/gorm"

// CreateTableV1 creates the email_outbox table and its indexes on SQLite or
// PostgreSQL. Call it from one of the project's own versioned migrations.
//
// It is frozen: applied migrations are checksummed, so this function never
// changes. A later kit version that needs a schema change adds MigrateV2 (and
// so on) for projects to call from a new migration.
//
// One row per email. idempotency_key is unique, so the same email is never
// queued twice. payload is the encrypted message; it is cleared once the email
// is sent, dead, or past expires_at. claim_token and lease_expires_at belong
// to the worker holding a sending row. receipt_table/receipt_id name the
// business row whose sent_at / delivery_failed_at records the final outcome.
// maybe_accepted_at is when the earliest attempt the provider may have
// accepted was claimed; a provider's deduplication window counts from there.
func CreateTableV1(tx *gorm.DB) error {
	for _, statement := range schemaV1 {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

// schemaV1 is checksummed by TestSchemaV1IsFrozen.
var schemaV1 = []string{
	`CREATE TABLE email_outbox (
			id TEXT PRIMARY KEY,
			idempotency_key TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL DEFAULT '',
			recipient TEXT NOT NULL,
			payload TEXT,
			status TEXT NOT NULL CHECK (status IN ('queued', 'sending', 'sent', 'unknown', 'failed', 'dead')),
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMP NOT NULL,
			claim_token TEXT NOT NULL DEFAULT '',
			lease_expires_at TIMESTAMP,
			provider TEXT NOT NULL DEFAULT '',
			provider_message_id TEXT NOT NULL DEFAULT '',
			last_error_code TEXT NOT NULL DEFAULT '',
			receipt_table TEXT NOT NULL DEFAULT '',
			receipt_id TEXT NOT NULL DEFAULT '',
			expires_at TIMESTAMP,
			maybe_accepted_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			sent_at TIMESTAMP
		)`,
	`CREATE INDEX email_outbox_due ON email_outbox (status, next_attempt_at)`,
	`CREATE INDEX email_outbox_recipient ON email_outbox (recipient, status)`,
	`CREATE INDEX email_outbox_message ON email_outbox (provider_message_id)`,
}
