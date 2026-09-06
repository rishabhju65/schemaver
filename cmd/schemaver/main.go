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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/migrate"
	"github.com/rishabhju65/schemaver/internal/netguard"
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
  schemaver run      <metadata-url>                Observation loop and web
                                                   interface together
  schemaver work     <metadata-url>                Observation loop only
  schemaver serve    <metadata-url>                Web interface only
  schemaver account  <metadata-url> <name> <email> <password>
                                                   Create an account and its
                                                   administrator
`

// controlPlane names the commands that operate on schemaver's own database, and
// may therefore take its url from the environment.
var controlPlane = map[string]bool{
	"run": true, "serve": true, "work": true, "migrate": true, "account": true,
}

// listenAddr resolves where to listen.
//
// PORT is honoured because most hosting platforms assign one and expect the
// process to use it; a service that ignores it fails its health check and is
// killed without ever explaining why.
func listenAddr() string {
	if addr := os.Getenv("SCHEMAVER_ADDR"); addr != "" {
		return addr
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

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

	// The metadata url may come from the environment instead of an argument, so
	// a platform that supplies configuration as environment variables needs no
	// command arguments at all.
	url := ""
	if len(args) > 1 {
		url = args[1]
	} else if controlPlane[cmd] {
		url = os.Getenv("SCHEMAVER_DATABASE_URL")
	}
	if url == "" {
		if controlPlane[cmd] {
			return fmt.Errorf("%s: needs a url, either as an argument or in SCHEMAVER_DATABASE_URL", cmd)
		}
		return fmt.Errorf("%s: needs a url", cmd)
	}

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

// openSignupEnabled reports whether anyone may create an account.
//
// It also decides whether this deployment may connect to private addresses: a
// server strangers can register targets on must not be usable as a probe of the
// network it sits in. Overriding that is possible but deliberate.
func openSignupEnabled() bool {
	v := strings.ToLower(os.Getenv("SCHEMAVER_OPEN_SIGNUP"))
	return v == "1" || v == "true" || v == "yes"
}

// targetPolicy decides which addresses this deployment may connect to.
func targetPolicy(log *slog.Logger) netguard.Policy {
	override := strings.ToLower(os.Getenv("SCHEMAVER_ALLOW_PRIVATE_TARGETS"))
	explicit := override == "1" || override == "true" || override == "yes"

	if !openSignupEnabled() {
		// Closed to strangers: reaching a private database is the entire point
		// of a self-hosted deployment (D-005).
		return netguard.Policy{AllowPrivate: true}
	}
	if explicit {
		log.Warn("open sign-up is on AND private targets are permitted — " +
			"anyone who registers can make this server probe its own network")
		return netguard.Policy{AllowPrivate: true}
	}
	log.Info("open sign-up is on; connections to private and link-local addresses are refused")
	return netguard.Policy{}
}

// initAuth decides whether this deployment still needs its first account.
func initAuth(ctx context.Context, st *store.Store, log *slog.Logger) (*auth.Setup, bool, error) {
	open := openSignupEnabled()

	admins, err := st.CountAdmins(ctx)
	if err != nil {
		return nil, false, err
	}
	if admins > 0 {
		return auth.Completed(), open, nil
	}
	if open {
		// Anyone may sign up, so gating the first account behind a token would
		// protect nothing.
		log.Info("no accounts yet; open sign-up is on, so visit /signup to create one")
		return auth.Completed(), true, nil
	}

	setup, err := auth.NewSetup()
	if err != nil {
		return nil, false, err
	}
	// Printed rather than stored: the token lives only in this process, so a
	// restart invalidates it and there is nothing on disk to leak.
	log.Warn("no accounts exist — visit /signup with this one-time token",
		"token", setup.Token())
	return setup, false, nil
}

func runServer(url string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := metadataPool(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// The interface registers servers, which seals a credential, so it needs the
	// encryption key as much as the worker does.
	box, err := secret.FromEnv()
	if err != nil {
		return err
	}
	st := store.New(pool, box)
	setup, open, err := initAuth(ctx, st, log)
	if err != nil {
		return err
	}
	srv, err := web.New(st, setup, open, targetPolicy(log))
	if err != nil {
		return err
	}
	addr := listenAddr()
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

func runAccount(metadataURL, accountName, email, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	pool, err := metadataPool(ctx, metadataURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	account, user, err := store.New(pool, nil).CreateAccount(ctx, accountName, email, "", hash)
	if err != nil {
		return err
	}
	fmt.Printf("created account %q (id %d) with administrator %s\n",
		account.Name, account.ID, user.Email)
	fmt.Println("register database servers from the web interface")
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

	setup, open, err := initAuth(ctx, st, log)
	if err != nil {
		return err
	}
	srv, err := web.New(st, setup, open, targetPolicy(log))
	if err != nil {
		return err
	}
	addr := listenAddr()
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
