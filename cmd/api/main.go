package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/febrian/areyouai/internal/config"
	"github.com/febrian/areyouai/internal/httpapi"
	"github.com/febrian/areyouai/internal/repository/postgres"
	"github.com/febrian/areyouai/internal/service/a2a"
	"github.com/febrian/areyouai/internal/worker/purge"
	"github.com/febrian/areyouai/internal/worker/webhooks"

	_ "github.com/lib/pq"
)

func main() {
	cfg := config.Load()
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var (
		handler http.Handler
		db      *sql.DB
		err     error
	)

	if cfg.PostgresDSN != "" {
		db, err = sql.Open("postgres", cfg.PostgresDSN)
		if err != nil {
			log.Fatalf("open postgres: %v", err)
		}
		defer db.Close()

		if err := db.Ping(); err != nil {
			log.Fatalf("ping postgres: %v", err)
		}

		store := postgres.NewStore(db)
		var runtimeService *a2a.Service
		handler, runtimeService = httpapi.NewRouterWithStoreAndAdminRuntime(
			store,
			cfg.ViewerHeartbeatTimeout,
			cfg.ClosedRoomGraceDelay,
			cfg.MaxClosedRetention,
			cfg.AdminToken,
		)
		if cfg.WebhookWorkerEnabled {
			worker := webhooks.New(store, webhooks.Config{
				PollInterval:    cfg.WebhookPollInterval,
				BatchSize:       cfg.WebhookBatchSize,
				DeliveryTimeout: cfg.WebhookDeliveryTimeout,
				ClaimStaleAfter: cfg.WebhookClaimStaleAfter,
				MaxAttempts:     cfg.WebhookMaxAttempts,
				BaseBackoff:     cfg.WebhookBaseBackoff,
				MaxBackoff:      cfg.WebhookMaxBackoff,
				SecretKey:       cfg.WebhookSecretKey,
				SecretKeyset:    cfg.WebhookSecretKeyset,
			})
			go func() {
				if err := worker.Run(rootCtx); err != nil {
					log.Printf("webhook worker stopped with error: %v", err)
				}
			}()
			log.Printf(
				"webhook worker enabled poll_interval=%s batch_size=%d delivery_timeout=%s claim_stale_after=%s max_attempts=%d",
				cfg.WebhookPollInterval,
				cfg.WebhookBatchSize,
				cfg.WebhookDeliveryTimeout,
				cfg.WebhookClaimStaleAfter,
				cfg.WebhookMaxAttempts,
			)
		} else {
			log.Printf("webhook worker disabled")
		}
		if cfg.PurgeWorkerEnabled {
			if runtimeService == nil {
				log.Printf("purge worker disabled (runtime service unavailable)")
			} else {
				worker := purge.New(runtimeService, purge.Config{
					PollInterval: cfg.PurgePollInterval,
					BatchSize:    cfg.PurgeBatchSize,
				})
				go func() {
					if err := worker.Run(rootCtx); err != nil {
						log.Printf("purge worker stopped with error: %v", err)
					}
				}()
				log.Printf(
					"purge worker enabled poll_interval=%s batch_size=%d",
					cfg.PurgePollInterval,
					cfg.PurgeBatchSize,
				)
			}
		} else {
			log.Printf("purge worker disabled")
		}
		log.Printf("api storage mode: postgres")
	} else {
		handler = httpapi.NewRouterWithOptions(
			cfg.ViewerHeartbeatTimeout,
			cfg.ClosedRoomGraceDelay,
			cfg.MaxClosedRetention,
		)
		log.Printf("api storage mode: in-memory")
	}

	server := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("api listening on %s", cfg.APIAddr)
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.ListenAndServe()
	}()

	select {
	case <-rootCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("server shutdown failed: %v", err)
		}
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}
}
