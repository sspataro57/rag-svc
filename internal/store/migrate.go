package store

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // registers the "postgres" driver for postgres:// DSNs
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Migrate applies all pending up-migrations against the database at dsn.
// Returns nil if the database is already up to date.
func Migrate(dsn string) error {
	return runMigrate(dsn, func(m *migrate.Migrate) error {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate up: %w", err)
		}
		return nil
	})
}

// MigrateDown rolls back the most recently applied migration. Useful for
// local development; not exposed through the CLI yet.
func MigrateDown(dsn string) error {
	return runMigrate(dsn, func(m *migrate.Migrate) error {
		if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate down: %w", err)
		}
		return nil
	})
}

func runMigrate(dsn string, fn func(*migrate.Migrate) error) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrate: sub fs: %w", err)
	}
	src, err := iofs.New(sub, ".")
	if err != nil {
		return fmt.Errorf("migrate: iofs source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("migrate: open: %w", err)
	}
	defer func() {
		// Close returns both source and database errors; they're already
		// covered by the operation result.
		_, _ = m.Close()
	}()
	return fn(m)
}
