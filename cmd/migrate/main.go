package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/lib/pq"
)

func main() {
	var (
		dir    = flag.String("dir", "migrations", "migrations directory")
		action = flag.String("action", "up", "one of: up, down, status")
	)
	flag.Parse()

	dsn := os.Getenv("POSTGRES_DSN")
	if strings.TrimSpace(dsn) == "" {
		log.Fatal("POSTGRES_DSN is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	switch *action {
	case "up":
		if err := ensureMigrationsTable(db); err != nil {
			log.Fatalf("ensure migrations table: %v", err)
		}
		if err := applyUp(db, *dir); err != nil {
			log.Fatalf("apply up: %v", err)
		}
		log.Println("migrations up complete")
	case "down":
		if err := ensureMigrationsTable(db); err != nil {
			log.Fatalf("ensure migrations table: %v", err)
		}
		if err := applyDown(db, *dir); err != nil {
			log.Fatalf("apply down: %v", err)
		}
		log.Println("migration down complete")
	case "status":
		if err := ensureMigrationsTable(db); err != nil {
			log.Fatalf("ensure migrations table: %v", err)
		}
		if err := printStatus(db, *dir); err != nil {
			log.Fatalf("status: %v", err)
		}
	default:
		log.Fatalf("unsupported action %q", *action)
	}
}

func ensureMigrationsTable(db *sql.DB) error {
	query := `CREATE TABLE IF NOT EXISTS schema_migrations (
  version TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`
	_, err := db.Exec(query)
	return err
}

func applyUp(db *sql.DB, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	for _, file := range files {
		version := versionFromPath(file, ".up.sql")
		applied, err := isApplied(db, version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		log.Printf("applying %s", file)
		if err := execSQLFile(db, file); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version) VALUES($1)`, version); err != nil {
			return err
		}
	}
	return nil
}

func applyDown(db *sql.DB, dir string) error {
	var version string
	err := db.QueryRow(`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if err != nil {
		if err == sql.ErrNoRows {
			log.Println("no applied migrations")
			return nil
		}
		return err
	}

	file := filepath.Join(dir, version+".down.sql")
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("down migration missing for %s: %w", version, err)
	}

	log.Printf("reverting %s", file)
	if err := execSQLFile(db, file); err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM schema_migrations WHERE version = $1`, version)
	return err
}

func printStatus(db *sql.DB, dir string) error {
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	defer rows.Close()

	appliedSet := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		appliedSet[v] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	for _, file := range files {
		version := versionFromPath(file, ".up.sql")
		state := "pending"
		if appliedSet[version] {
			state = "applied"
		}
		fmt.Printf("%s %s\n", state, version)
	}
	return nil
}

func isApplied(db *sql.DB, version string) (bool, error) {
	var hit int
	err := db.QueryRow(`SELECT 1 FROM schema_migrations WHERE version = $1 LIMIT 1`, version).Scan(&hit)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func versionFromPath(path, suffix string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, suffix)
}

func execSQLFile(db *sql.DB, file string) error {
	content, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	_, err = db.Exec(string(content))
	return err
}
