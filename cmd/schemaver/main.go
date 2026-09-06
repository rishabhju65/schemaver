// Command schemaver is the command-line entry point.
//
// Per D-002 the CLI is not the primary surface — it serves CI and exercises the
// engine directly. The web application is the product.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/migrate"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/secret"
	"github.com/rishabhju65/schemaver/internal/store"
	"github.com/rishabhju65/schemaver/internal/web"
	"github.com/rishabhju65/schemaver/internal/worker"
)

const usage = `schemaver — version control for database schemas

Reading a target database (needs only CONNECT):
  schemaver introspect  <url>   Print the canonical schema as JSON
  schemaver fingerprint <url>   Print the schema's version digest
  schemaver ddl         <url>   Print the schema as executable DDL
  schemaver databases   <url>   List every database on the instance
  schemaver scan        <url>   Read every database on the instance

Running the control plane (against schemaver's own metadata database):
  schemaver migrate  <metadata-url>                 Apply schemaver's own schema
  schemaver register <metadata-url> <target-url> [name]
                                                   Register a server and manage
                                                   every database on it
  schemaver pair     <metadata-url> <instance-id> <database> <peer>
                                                   Compare one database against
                                                   another, for drift
  schemaver run      <metadata-url>                Observation loop and web
                                                   interface together
  schemaver work     <metadata-url>                Observation loop only
  schemaver serve    <metadata-url>                Web interface only
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "schemaver: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		if len(args) == 0 {
			return fmt.Errorf("no command given")
		}
		return nil
	}
	cmd := args[0]
	if len(args) < 2 {
		return fmt.Errorf("%s: needs a url", cmd)
	}
	url := args[1]

	switch cmd {
	case "introspect", "fingerprint", "ddl":
		return readOne(cmd, url)
	case "databases", "scan":
		return readInstance(cmd, url)
	case "migrate":
		return runMigrate(url)
	case "work":
		return runWorker(url)
	case "serve":
		return runServer(url)
	case "run":
		return runAll(url)
	case "register":
		if len(args) < 3 {
			return fmt.Errorf("register: needs a target url")
		}
		name := ""
		if len(args) > 3 {
			name = args[3]
		}
		return runRegister(url, args[2], name)
	case "pair":
		if len(args) < 5 {
			return fmt.Errorf("pair: needs an instance id, a database and a peer")
		}
		return runPair(url, args[2], args[3], args[4])
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func readOne(cmd, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())

	s, err := introspect.Schema(ctx, conn)
	if err != nil {
		return err
	}
	switch cmd {
	case "introspect":
		b, err := schema.Canonical(s)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	case "fingerprint":
		v, err := schema.Fingerprint(s)
		if err != nil {
			return err
		}
		fmt.Println(v)
	case "ddl":
		fmt.Print(render.Schema(s))
	}
	return nil
}

func readInstance(cmd, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if cmd == "databases" {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer conn.Close(context.Background())

		list, err := introspect.Databases(ctx, conn)
		if err != nil {
			return err
		}
		for _, d := range list {
			mark := " "
			if !d.Connectable {
				mark = "!"
			}
			fmt.Printf("%s %-32s %-16s %10.1f MB\n",
				mark, d.Name, d.Owner, float64(d.SizeBytes)/(1024*1024))
		}
		return nil
	}

	results, err := introspect.Instance(ctx, url)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func metadataPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to metadata database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping metadata database: %w", err)
	}
	return pool, nil
}

func runMigrate(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := metadataPool(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := migrate.Apply(ctx, pool)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("already up to date")
		return nil
	}
	for _, name := range applied {
		fmt.Printf("applied %s\n", name)
	}
	return nil
}

func runServer(url string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := metadataPool(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The interface reads state; it never opens a credential, so no encryption
	// key is required to run it.
	srv, err := web.New(store.New(pool, nil))
	if err != nil {
		return err
	}
	addr := os.Getenv("SCHEMAVER_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	fmt.Fprintf(os.Stderr, "schemaver listening on %s\n", addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// sslMode reads the TLS mode out of a connection URL. Postgres defaults to
// "prefer", and silently downgrading a caller who asked for verification would be
// worse than any convenience it bought, so the value is taken verbatim.
func sslMode(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return "prefer"
	}
	if m := u.Query().Get("sslmode"); m != "" {
		return m
	}
	return "prefer"
}

func runRegister(metadataURL, targetURL, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := pgx.ParseConfig(targetURL)
	if err != nil {
		return fmt.Errorf("parse target url: %w", err)
	}
	if name == "" {
		name = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	}

	pool, err := metadataPool(ctx, metadataURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	box, err := secret.FromEnv()
	if err != nil {
		return err
	}
	st := store.New(pool, box)

	// Prove the credentials work before storing them: a registration that looks
	// successful and then fails on every observation is worse than a refusal.
	probe, err := pgx.Connect(ctx, targetURL)
	if err != nil {
		return fmt.Errorf("cannot reach the target: %w", err)
	}
	found, err := introspect.Databases(ctx, probe)
	probe.Close(context.Background())
	if err != nil {
		return fmt.Errorf("cannot list databases (does the role have CONNECT?): %w", err)
	}

	instanceID, err := st.RegisterInstance(ctx, name, cfg.Host, int(cfg.Port),
		sslMode(targetURL), cfg.User, cfg.Password)
	if err != nil {
		return err
	}
	added, _, err := st.SyncDatabases(ctx, instanceID, found)
	if err != nil {
		return err
	}
	managed, err := st.SetManaged(ctx, instanceID, nil)
	if err != nil {
		return err
	}

	fmt.Printf("registered %s as instance %d\n", name, instanceID)
	fmt.Printf("discovered %d databases, now managing %d\n", added, managed)
	for _, d := range found {
		mark := " "
		if !d.Connectable {
			mark = "!"
		}
		fmt.Printf("  %s %-28s %8.1f MB\n", mark, d.Name, float64(d.SizeBytes)/(1024*1024))
	}
	return nil
}

func runPair(metadataURL, instanceArg, database, peer string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	instanceID, err := strconv.ParseInt(instanceArg, 10, 64)
	if err != nil {
		return fmt.Errorf("instance id must be a number: %w", err)
	}
	pool, err := metadataPool(ctx, metadataURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool, nil)

	dbID, err := st.DatabaseIDByName(ctx, instanceID, database)
	if err != nil {
		return err
	}
	peerID, err := st.DatabaseIDByName(ctx, instanceID, peer)
	if err != nil {
		return err
	}
	if err := st.PairForDrift(ctx, dbID, peerID); err != nil {
		return err
	}
	fmt.Printf("%s will be compared against %s\n", database, peer)
	return nil
}

// runAll runs the observation loop and the interface in one process, which is
// the shape D-005 asks for: a single container an operator deploys inside their
// own network.
func runAll(url string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := metadataPool(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Migrations run on start-up so a fresh deployment is usable without a
	// separate step. It is safe to repeat: already-applied migrations are
	// skipped, and an edited one is refused.
	if _, err := migrate.Apply(ctx, pool); err != nil {
		return err
	}

	box, err := secret.FromEnv()
	if err != nil {
		return err
	}
	st := store.New(pool, box)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	srv, err := web.New(st)
	if err != nil {
		return err
	}
	addr := os.Getenv("SCHEMAVER_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errs := make(chan error, 2)
	go func() {
		log.Info("interface listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()
	go func() {
		log.Info("observation loop started")
		if err := worker.New(st, worker.Config{}, log).Run(ctx); err != nil && ctx.Err() == nil {
			errs <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errs:
		stop()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		return err
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	log.Info("stopped")
	return nil
}

func runWorker(url string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := metadataPool(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	box, err := secret.FromEnv()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	w := worker.New(store.New(pool, box), worker.Config{}, log)

	log.Info("observation loop started")
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("observation loop stopped")
	return nil
}
