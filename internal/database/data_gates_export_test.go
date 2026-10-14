package database

import "gorm.io/gorm"

// ObserveMigrationDataGatesForTest is available only to this package's external
// tests. It uses the real connection/search-path/lock wrapper; no callback or
// injected result can replace the production observer or its authority checks.
func ObserveMigrationDataGatesForTest(db *gorm.DB, runtimeRole string) error {
	return withMigrationSession(db, func(session *gorm.DB) error {
		return observeMigrationDataGates(session, runtimeRole)
	})
}
