package worker_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/migrate"
	"github.com/rishabhju65/schemaver/internal/shadow"
	"github.com/rishabhju65/schemaver/internal/store"
	"github.com/rishabhju65/schemaver/internal/worker"
)

// withDatabase points a connection string at another database on the same
// server.
func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// ownMetadata builds an empty metadata database for this test and migrates it.
//
// Deliberately not the deployment's own. Run starts the drain loops as well as
// the sweep, and pointed at a real metadata database they would claim real
// jobs — observing databases and executing migrations that belong to somebody
// else. An empty one has nothing to claim.
func ownMetadata(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("SCHEMAVER_METADATA_URL")
	if raw == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the worker tests")
	}
	adminDSN, err := withDatabase(raw, "postgres")
	if err != nil {
		t.Fatalf("parse metadata url: %v", err)
	}
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Skipf("cannot reach the metadata server: %v", err)
	}
	defer admin.Close(context.Background())

	name := fmt.Sprintf("schemaver_worker_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create test metadata database: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		conn, err := pgx.Connect(bg, adminDSN)
		if err != nil {
			return
		}
		defer conn.Close(bg)
		_, _ = conn.Exec(bg, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})

	dsn, err := withDatabase(raw, name)
	if err != nil {
		t.Fatalf("build test url: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test metadata database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate test metadata database: %v", err)
	}
	return pool
}

// plantAbandoned creates a shadow database that looks as though a process died
// holding it, age ago. The creation time lives in the name, which is the only
// record an abandoned database leaves.
func plantAbandoned(t *testing.T, ctx context.Context, adminDSN string, age time.Duration) string {
	t.Helper()
	name := fmt.Sprintf("schemaver_shadow_%d_worker",
		time.Now().Add(-age).UnixMilli())

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Skipf("cannot reach the shadow server: %v", err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("plant abandoned shadow: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		conn, err := pgx.Connect(bg, adminDSN)
		if err != nil {
			return
		}
		defer conn.Close(bg)
		_, _ = conn.Exec(bg, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})
	return name
}

func databaseExists(t *testing.T, ctx context.Context, adminDSN, name string) bool {
	t.Helper()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("check database: %v", err)
	}
	return n > 0
}

func shadowAdminDSN(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("SCHEMAVER_SHADOW_URL")
	if raw == "" {
		t.Skip("set SCHEMAVER_SHADOW_URL to run the sweep test")
	}
	return raw
}

// TestRunningTheWorkerSweepsAbandonedShadows is the whole point of this file.
//
// Sweep was written, documented and covered by three tests of its own, and
// nothing in the product called it. Every one of those tests passed while
// abandoned databases accumulated on the server for as long as the deployment
// had been running. So this test asserts the one thing they could not: that
// starting a worker is enough to make the sweeping happen.
//
// It runs Run rather than the sweep loop directly, because the defect was
// exactly a missing call — a test that reached past Run would reproduce it.
// safeBuffer collects log output written from the worker's own goroutines.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunningTheWorkerSweepsAbandonedShadows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminDSN := shadowAdminDSN(t)
	pool := ownMetadata(t, ctx)

	shadowPool, err := shadow.NewPool(adminDSN)
	if err != nil {
		t.Fatalf("shadow pool: %v", err)
	}

	var logs safeBuffer
	w := worker.New(store.New(pool, nil), worker.Config{
		ID:     "sweep-test",
		Shadow: shadowPool,
		// An age the planted database exceeds, and a short interval so the
		// worker gets several attempts within the test.
		SweepAge:      time.Hour,
		SweepInterval: 500 * time.Millisecond,
	}, slog.New(slog.NewTextHandler(&logs, nil)))

	stale := plantAbandoned(t, ctx, adminDSN, 3*time.Hour)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// The assertion is on the worker saying it dropped something, not on the
	// database being gone. Shadow databases live on a shared server and this
	// package runs alongside the one whose own tests sweep it, so absence
	// proves only that somebody swept — which would pass just as happily with
	// the call removed. If something else takes the planted database first,
	// plant another and keep waiting.
	swept := false
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		if strings.Contains(logs.String(), "dropped abandoned shadow databases") {
			swept = true
			break
		}
		if !databaseExists(t, ctx, adminDSN, stale) {
			stale = plantAbandoned(t, ctx, adminDSN, 3*time.Hour)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	<-done

	if !swept {
		t.Error("the worker ran and never swept an abandoned shadow database; " +
			"nothing is calling Sweep")
	}
}

// TestTheWorkerWillNotSweepWithoutAShadowServer keeps the loop from being the
// thing that crashes a deployment that has no shadow server configured.
//
// A worker with no pool cannot prove migrations, which is reported per
// migration rather than treated as fatal. Sweeping has to be equally optional,
// and a nil pool is the shape that reaches this code.
func TestTheWorkerWillNotSweepWithoutAShadowServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := ownMetadata(t, ctx)
	w := worker.New(store.New(pool, nil), worker.Config{ID: "no-shadow"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()
	time.Sleep(time.Second)
	stop()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the worker did not stop after its context was cancelled")
	}
}
