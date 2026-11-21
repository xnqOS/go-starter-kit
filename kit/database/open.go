package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open shares the ORM and repository API across both SQL dialects. SQLite uses
// the official GORM driver (CGO); PostgreSQL uses pgx through database/sql.
func Open(ctx context.Context, storage, databaseURL string) (*gorm.DB, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	var driver, dsn string
	switch storage {
	case "sqlite":
		var err error
		dsn, err = sqliteDSN(databaseURL)
		if err != nil {
			return nil, err
		}
		driver = "sqlite3"
	case "postgres":
		driver, dsn = "pgx", databaseURL
	default:
		return nil, fmt.Errorf("SQL storage must be sqlite or postgres")
	}
	pool, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid %s connection configuration", storage)
	}
	if storage == "sqlite" {
		// DSN settings apply to reopened connections too. BEGIN IMMEDIATE
		// serializes migration processes; one connection serializes local writes.
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
	} else {
		pool.SetMaxOpenConns(20)
		pool.SetMaxIdleConns(5)
		pool.SetConnMaxLifetime(30 * time.Minute)
		pool.SetConnMaxIdleTime(5 * time.Minute)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		// Driver errors can include credentials; do not wrap them.
		return nil, fmt.Errorf("cannot connect to %s; check DATABASE_URL and database availability", storage)
	}
	var dialect gorm.Dialector
	if storage == "sqlite" {
		dialect = sqlite.New(sqlite.Config{Conn: pool})
	} else {
		dialect = postgres.New(postgres.Config{Conn: pool})
	}
	db, err := gorm.Open(dialect, &gorm.Config{
		DisableAutomaticPing: true,
		// HTTP middleware logs application errors through slog. Do not expose
		// SQL parameter values through a separate default ORM logger.
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("cannot initialize %s dialect", storage)
	}
	return db, nil
}

func Close(db *gorm.DB) error {
	pool, err := db.DB()
	if err != nil {
		return err
	}
	return pool.Close()
}
