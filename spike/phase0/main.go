// Phase 0.1 spike — de-risk the agent's storage path.
//
// Proves the DoD: agent (via nucleus-go) connects to a pgwire target, runs a
// migration, reads/writes a row. Parameterized by DATABASE_URL so the same code
// targets Postgres (proven default) and Nucleus (dogfood target).
//
// Run:
//
//	DATABASE_URL=postgres://tyler:tyler@localhost:5435/gss?sslmode=disable go run .
//	DATABASE_URL=nucleus://... go run .
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// User is a slice of the v1 data model (PLAN.md §5).
type User struct {
	ID           int64     `json:"id" db:"id"`
	Email        string    `json:"email" db:"email"`
	PasswordHash string    `json:"-" db:"password_hash"`
	Role         string `json:"role" db:"role"`
	CreatedAt    string `json:"created_at" db:"created_at"`
}

var migrations = []nucleus.Migration{
	{
		Version: 1,
		Name:    "create_users",
		Up: `CREATE TABLE IF NOT EXISTS users (
			id            BIGSERIAL PRIMARY KEY,
			email         TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'owner',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		Down: `DROP TABLE IF EXISTS users`,
	},
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		logger.Error("DATABASE_URL required")
		os.Exit(1)
	}

	ctx := context.Background()

	// 1. Connect + feature auto-detect.
	db, err := nucleus.Connect(ctx, url)
	if err != nil {
		logger.Error("connect failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	f := db.Features()
	fmt.Printf("[connect] ok\n")
	fmt.Printf("[detect]  IsNucleus=%v  Version=%q\n", f.IsNucleus, f.Version)
	fmt.Printf("[detect]  KV=%v TS=%v (these gate our metrics/config stores)\n", f.HasKV, f.HasTS)

	if err := db.Ping(ctx); err != nil {
		logger.Error("ping failed", "error", err)
		os.Exit(1)
	}
	fmt.Printf("[ping]    ok\n")

	// 2. Migrate.
	if err := db.Migrate(ctx, migrations); err != nil {
		logger.Error("migrate failed", "error", err)
		os.Exit(1)
	}
	fmt.Printf("[migrate] ok (1 migration)\n")

	// 3. Write a row.
	email := fmt.Sprintf("spike-%d@test", time.Now().UnixNano())
	created, err := nucleus.QueryOne[User](ctx, db.SQL(),
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 RETURNING id, email, password_hash, role, created_at`,
		email, "x$hashed")
	if err != nil {
		logger.Error("insert failed", "error", err)
		os.Exit(1)
	}
	fmt.Printf("[write]   ok id=%d email=%s role=%s\n", created.ID, created.Email, created.Role)

	// 4. Read it back.
	got, err := nucleus.QueryOne[User](ctx, db.SQL(),
		`SELECT id, email, password_hash, role, created_at FROM users WHERE id = $1`, created.ID)
	if err != nil {
		logger.Error("select failed", "error", err)
		os.Exit(1)
	}
	fmt.Printf("[read]    ok id=%d email=%s createdAt=%s\n", got.ID, got.Email, got.CreatedAt)

	// 5. List all.
	all, err := nucleus.Query[User](ctx, db.SQL(),
		`SELECT id, email, password_hash, role, created_at FROM users ORDER BY id`)
	if err != nil {
		logger.Error("list failed", "error", err)
		os.Exit(1)
	}
	fmt.Printf("[list]    ok count=%d\n", len(all))

	fmt.Printf("\nDoD MET: connect + migrate + read/write all succeeded on %s\n", targetName(f.IsNucleus))
}

func targetName(isNucleus bool) string {
	if isNucleus {
		return "NUCLEUS"
	}
	return "POSTGRES"
}
