package main

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// newMigrator opens a dedicated db connection for the migrator.
// Calling m.Close() closes that connection; it does not affect any
// other *sql.DB in the process.
func newMigrator(dsn string) (*migrate.Migrate, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open migration db: %w", err)
	}
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create migration source: %w", err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create migrator: %w", err)
	}
	return m, nil
}

// RunMigrations applies all pending up-migrations.
func RunMigrations(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migration failed: %w", err)
	}
	return nil
}

// MigrationVersion returns the currently applied schema version.
// Returns (0, false, migrate.ErrNilVersion) when no migrations have run yet.
func MigrationVersion(dsn string) (version uint, dirty bool, err error) {
	m, err := newMigrator(dsn)
	if err != nil {
		return 0, false, err
	}
	defer m.Close()
	return m.Version()
}
