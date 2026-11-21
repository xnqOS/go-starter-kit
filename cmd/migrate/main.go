package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/database"
	"starter-kit/internal/config"
	"starter-kit/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Storage == "memory" {
		fmt.Println("Memory storage does not require migrations.")
		return nil
	}
	list, err := migrations.List()
	if err != nil {
		return err
	}
	db, err := database.Open(ctx, cfg.Storage, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close(db)
	if err := database.Migrate(ctx, db, list); err != nil {
		return err
	}
	fmt.Println("Database migrations are up to date.")
	return nil
}
