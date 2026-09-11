// Command schemaver is the command-line entry point.
//
// It parses arguments and nothing else. What a deployment consists of is
// decided by internal/config, and how those parts are assembled by
// internal/runtime — so run, serve and work are three selections of one
// assembly rather than three code paths that happen to look alike.
//
// Per D-002 the command line is not the primary surface; it serves CI and
// exercises the engine directly. The web application is the product.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/config"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/migrate"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/runtime"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

const usage = `schemaver — version control for database schemas

Reading a target database (needs only CONNECT):
  schemaver introspect  <url>   Print the canonical schema as JSON
  schemaver fingerprint <url>   Print the schema's version digest
  schemaver ddl         <url>   Print the schema as executable DDL
  schemaver databases   <url>   List every database on the instance
  schemaver scan        <url>   Read every database on the instance

Running the control plane (against schemaver's own metadata database):
  schemaver run     [url]   Interface and worker together
  schemaver serve   [url]   Interface only
  schemaver work    [url]   Worker only
  schemaver migrate [url]   Apply schemaver's own schema and exit

  The url may be omitted when SCHEMAVER_DATABASE_URL is set.

  schemaver account <url> <org-name> <email> <password>
                            Create an organisation, its first project and its
                            administrator
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "schemaver: %v\n", err)
		os.Exit(1)
	}
}

// components selects what each control-plane command assembles.
var components = map[string]config.Components{
	"run":   config.Everything(),
	"serve": {Interface: true},
	"work":  {Work: true},
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
	rest := args[1:]

	if selected, ok := components[cmd]; ok {
		return serve(url(rest), selected)
	}

	switch cmd {
	case "introspect", "fingerprint", "ddl":
		if len(rest) == 0 {
			return fmt.Errorf("%s: needs a database url", cmd)
		}
		return readOne(cmd, rest[0])

	case "databases", "scan":
		if len(rest) == 0 {
			return fmt.Errorf("%s: needs a database url", cmd)
		}
		return readInstance(cmd, rest[0])

	case "migrate":
		return runMigrate(url(rest))

	case "account":
		if len(rest) < 4 {
			return fmt.Errorf("account: needs a url, a name, an email and a password")
		}
		return runAccount(rest[0], rest[1], rest[2], rest[3])
	}

	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", cmd)
}

// url takes the metadata database from an argument when given; config falls back
// to the environment when it is not.
func url(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// serve assembles and runs whichever components were selected.
//
// One function for run, serve and work: the difference between a bundled
// deployment and a split one is the selection, not the code.
func serve(metadataURL string, selected config.Components) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := logger()
	cfg, err := config.Load(metadataURL, selected)
	if err != nil {
		return err
	}

	system, err := runtime.Start(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer system.Close()

	log.Info("schemaver started", cfg.Describe()...)
	if err := system.Run(ctx); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

func readOne(cmd, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, target)
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

func readInstance(cmd, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if cmd == "databases" {
		conn, err := pgx.Connect(ctx, target)
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

	results, err := introspect.Instance(ctx, target)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

// metadataPool opens the metadata database for the commands that do not assemble
// a whole system.
func metadataPool(ctx context.Context, metadataURL string) (*pgxpool.Pool, error) {
	if metadataURL == "" {
		metadataURL = os.Getenv(config.EnvDatabaseURL)
	}
	if metadataURL == "" {
		return nil, fmt.Errorf("no metadata database: pass one or set %s", config.EnvDatabaseURL)
	}
	pool, err := pgxpool.New(ctx, metadataURL)
	if err != nil {
		return nil, fmt.Errorf("connect to the metadata database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping the metadata database: %w", err)
	}
	return pool, nil
}

func runMigrate(metadataURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := metadataPool(ctx, metadataURL)
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

func runAccount(metadataURL, orgName, email, password string) error {
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

	org, project, user, err := store.New(pool, nil).
		CreateOrganization(ctx, orgName, "default", email, "", hash)
	if err != nil {
		return err
	}
	fmt.Printf("created organisation %q (id %d)\n", org.Name, org.ID)
	fmt.Printf("  project %q (id %d)\n", project.Name, project.ID)
	fmt.Printf("  administrator %s\n", user.Email)
	fmt.Println("register database servers from the web interface")
	return nil
}
