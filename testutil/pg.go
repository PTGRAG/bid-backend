//go:build integration

// Package testutil provides the integration-test harness shared by the
// admin-backend payment/ticket/wallet test suites.
//
// The harness is hidden behind the `integration` build tag so that the default
// `go test ./...` invocation never has to pull testcontainers or talk to
// Docker. Run the integration suite with:
//
//	go test -tags=integration ./...
//
// One Postgres container is started per test package (via TestMain in each
// package that needs it); individual tests get a clean schema by calling
// Reset() at the top, which truncates every table in dependency order rather
// than re-running migrations (faster and avoids goose's per-call locking).
package testutil

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bid-hub/admin-backend/platform/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// PG bundles everything an integration test needs to talk to the test
// database: the *gorm.DB used by all repositories, plus a Reset hook for
// per-test isolation. Created via MustStartPostgres.
type PG struct {
	DB        *gorm.DB
	container *tcpostgres.PostgresContainer
}

var (
	// One container per process; reused by every test in the package that
	// shares the same TestMain. Tests that need true isolation can call
	// Reset(t) at the top of each test instead of spinning up a fresh
	// container (10× faster).
	startOnce sync.Once
	shared    *PG
	startErr  error
)

// MustStartPostgres returns a process-wide *PG with migrations already
// applied. Subsequent calls return the same handle; callers are expected to
// invoke Reset at the start of each test to wipe state.
//
// If Docker is not reachable the test is t.Skip-ped rather than failed:
// developers without Docker can still run unit suites, and CI must enable
// Docker explicitly to gain coverage.
func MustStartPostgres(t testing.TB) *PG {
	t.Helper()
	startOnce.Do(func() {
		shared, startErr = startPostgres()
	})
	if startErr != nil {
		// Skip locally when Docker is unavailable so `go test ./...` stays
		// usable without testcontainers. In CI (CI=true) a missing Docker
		// daemon must fail the job  otherwise we get a false green.
		if os.Getenv("CI") == "true" {
			t.Fatalf("integration: docker/testcontainers required in CI: %v", startErr)
		}
		t.Skipf("integration: skipping (docker/testcontainers unavailable): %v", startErr)
		return nil
	}
	return shared
}

// openExternal connects to an already-running Postgres named by TEST_DATABASE_URL (for hosts
// without Docker). Reset truncates every table, so the database name must contain "test".
func openExternal(dsn string) (*PG, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("TEST_DATABASE_URL: %w", err)
	}
	// Tests TRUNCATE every table: only ever run against a database that is clearly a test one.
	if !strings.Contains(strings.ToLower(cfg.Database), "test") {
		return nil, fmt.Errorf("TEST_DATABASE_URL database %q must contain \"test\" (tests truncate all tables)", cfg.Database)
	}
	gormDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Silent),
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open gorm: %w", err)
	}
	database.Migrate(gormDB)
	return &PG{DB: gormDB}, nil
}

func startPostgres() (*PG, error) {
	if dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")); dsn != "" {
		return openExternal(dsn)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// postgres:16-alpine boots in ~2s on a warm host and matches what we
	// run in docker-compose. Pinning a minor would silently rot whenever
	// the public tag changes; we deliberately stay on a moving image tag
	// because schema migrations, not pg internals, are what we're testing.
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("carramarket_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
		tcpostgres.WithSQLDriver("pgx"),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}

	// Belt-and-suspenders: wait until we can actually log a SELECT 1
	// before we hand the container back. Removes the "first test of the
	// run is flaky" failure mode.
	if err := waitForReady(ctx, container); err != nil {
		_ = container.Terminate(context.Background())
		return nil, err
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(context.Background())
		return nil, fmt.Errorf("connection string: %w", err)
	}

	gormDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		// Tests are noisy enough; the production code path keeps gorm logging
		// at slog.Warn. Silence it here so test output stays grep-able.
		Logger: logger.Default.LogMode(logger.Silent),
		// Mirror the production setting  the admin/client schema has
		// FK references that cross the goose dialect boundary
		// (winner_id, participant_id sometimes point at IDs only the
		// client-backend creates).
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		_ = container.Terminate(context.Background())
		return nil, fmt.Errorf("open gorm: %w", err)
	}

	// goose runs the same embedded migration set used in production. If
	// a new migration breaks integration tests we want that to surface
	// here, not in prod.
	database.Migrate(gormDB)

	return &PG{DB: gormDB, container: container}, nil
}

// waitForReady polls a trivial SELECT until it succeeds or ctx expires.
// testcontainers' BasicWaitStrategies stops at pg_isready, which can race
// the network listener on slow hosts.
func waitForReady(ctx context.Context, container *tcpostgres.PostgresContainer) error {
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err == nil {
			sqlDB, _ := db.DB()
			if pingErr := sqlDB.PingContext(ctx); pingErr == nil {
				_ = sqlDB.Close()
				return nil
			} else {
				lastErr = pingErr
			}
			_ = sqlDB.Close()
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("postgres never became reachable: %w", lastErr)
}

// Reset truncates every table in the shared schema so the test starts from
// an empty DB. Migrations remain  only data is wiped. This is ~10× cheaper
// than recreating the container per test.
//
// We rely on a single TRUNCATE … RESTART IDENTITY CASCADE because:
//   - it ignores FK ordering (we'd otherwise need a topo sort),
//   - it resets sequences so generated IDs stay deterministic across runs,
//   - it works without superuser, unlike `pg_terminate_backend` tricks.
func (p *PG) Reset(t testing.TB) {
	t.Helper()
	if p == nil || p.DB == nil {
		return
	}
	sqlDB, err := p.DB.DB()
	if err != nil {
		t.Fatalf("Reset: extract sql.DB: %v", err)
	}
	rows, err := sqlDB.QueryContext(context.Background(),
		`SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("Reset: list tables: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("Reset: scan: %v", err)
		}
		names = append(names, `"`+name+`"`)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Reset: rows err: %v", err)
	}
	if len(names) == 0 {
		return
	}
	stmt := "TRUNCATE TABLE " + strings.Join(names, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := sqlDB.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("Reset: truncate: %v", err)
	}
}

// Stop terminates the container. Callers normally don't invoke this
// directly  TestMain handles it via defer. Exposed for the rare case of
// nested suites needing explicit teardown.
func (p *PG) Stop() {
	if p == nil || p.container == nil {
		return
	}
	_ = p.container.Terminate(context.Background())
}
