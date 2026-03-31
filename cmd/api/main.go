package main

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/febrian/areyouai/internal/config"
	"github.com/febrian/areyouai/internal/httpapi"
	"github.com/febrian/areyouai/internal/repository/postgres"

	_ "github.com/lib/pq"
)

func main() {
	cfg := config.Load()
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
		handler = httpapi.NewRouterWithStoreAndAdmin(
			store,
			cfg.ViewerHeartbeatTimeout,
			cfg.ClosedRoomGraceDelay,
			cfg.MaxClosedRetention,
			cfg.AdminToken,
		)
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
	}

	log.Printf("api listening on %s", cfg.APIAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
}
