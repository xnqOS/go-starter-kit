package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/xnqOS/go-starter-kit/kit/database"
	"github.com/xnqOS/go-starter-kit/kit/httpx"
	"starter-kit/internal/config"
)

type App struct {
	Handler *gin.Engine
	db      *sql.DB
	server  *http.Server
	logger  *slog.Logger
	storage string
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	var db *gorm.DB
	var pool *sql.DB
	if cfg.Storage != "memory" {
		var err error
		db, err = database.Open(ctx, cfg.Storage, cfg.DatabaseURL)
		if err != nil {
			return nil, err
		}
		pool, err = db.DB()
		if err != nil {
			_ = database.Close(db)
			return nil, err
		}
	}
	router := gin.New()
	if err := router.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		if pool != nil {
			_ = pool.Close()
		}
		return nil, fmt.Errorf("invalid trusted proxy configuration: %w", err)
	}
	router.Use(middleware(logger))
	router.HandleMethodNotAllowed = true
	router.NoRoute(func(c *gin.Context) {
		httpx.Reject(c, http.StatusNotFound, "not_found", "route not found")
	})
	router.NoMethod(func(c *gin.Context) {
		httpx.Reject(c, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})
	router.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	router.GET("/ready", func(c *gin.Context) {
		if pool != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			if err := pool.PingContext(ctx); err != nil {
				httpx.Reject(c, http.StatusServiceUnavailable, "not_ready", "database unavailable")
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	registerModules(router.Group("/api/v1"), db)
	return &App{
		Handler: router,
		db:      pool,
		logger:  logger,
		storage: cfg.Storage,
		server: &http.Server{
			Addr: cfg.HTTPAddr, Handler: router,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() { result <- a.server.ListenAndServe() }()
	a.logger.Info("API started", "address", a.server.Addr, "storage", a.storage)
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			_ = a.server.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		return nil
	}
}

func (a *App) Close() error {
	if a.db != nil {
		return a.db.Close()
	}
	return nil
}
