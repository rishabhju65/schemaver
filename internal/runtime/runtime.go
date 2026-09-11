// Package runtime assembles schemaver's modules into a running system.
//
// Every module — introspection, diffing, execution, persistence, the interface,
// the worker — is independent and knows nothing about how a deployment is
// arranged. This is the one place that decides which of them run together.
//
// That is what keeps "bundled or split" a deployment decision rather than a code
// path: one process running both components and two processes running one each
// are the same assembly with a different selection, pointed at the same metadata
// database.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/config"
	"github.com/rishabhju65/schemaver/internal/migrate"
	"github.com/rishabhju65/schemaver/internal/store"
	"github.com/rishabhju65/schemaver/internal/web"
	"github.com/rishabhju65/schemaver/internal/worker"
)

// System is an assembled deployment.
type System struct {
	Config *config.Config
	Pool   *pgxpool.Pool
	Store  *store.Store

	// Interface and Worker are nil when the configuration does not select them.
	Interface *http.Server
	Worker    *worker.Worker

	log *slog.Logger
}

// Start assembles a system: connects, applies schemaver's own migrations, and
// builds whichever components the configuration selects.
//
// Migrations run here rather than in a separate step so a fresh deployment is
// usable without one. Repeating them is safe — applied ones are skipped, and an
// edited one is refused.
func Start(ctx context.Context, cfg *config.Config, log *slog.Logger) (*System, error) {
	if log == nil {
		log = slog.Default()
	}
	if !cfg.Components.Interface && !cfg.Components.Work {
		return nil, config.ErrNothingToRun
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to the metadata database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping the metadata database: %w", err)
	}

	applied, err := migrate.Apply(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	for _, name := range applied {
		log.Info("applied schemaver's own migration", "version", name)
	}

	sys := &System{
		Config: cfg, Pool: pool,
		Store: store.New(pool, cfg.Secrets), log: log,
	}
	for _, warning := range cfg.Warnings() {
		log.Warn(warning)
	}

	if cfg.Components.Interface {
		if err := sys.buildInterface(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}
	if cfg.Components.Work {
		sys.Worker = worker.New(sys.Store, worker.Config{}, log)
	}
	return sys, nil
}

// buildInterface constructs the web server, including the bootstrap decision
// about whether this deployment still needs its first account.
func (s *System) buildInterface(ctx context.Context) error {
	setup, err := s.bootstrap(ctx)
	if err != nil {
		return err
	}
	srv, err := web.New(s.Store, setup, s.Config.OpenSignup, s.Config.Targets)
	if err != nil {
		return err
	}
	s.Interface = &http.Server{
		Addr:              s.Config.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return nil
}

// bootstrap decides how the first account comes into existence (D-015).
func (s *System) bootstrap(ctx context.Context) (*auth.Setup, error) {
	admins, err := s.Store.CountAdmins(ctx)
	if err != nil {
		return nil, err
	}
	if admins > 0 {
		return auth.Completed(), nil
	}
	if s.Config.OpenSignup {
		// Anyone may sign up, so gating the first account behind a token would
		// protect nothing.
		s.log.Info("no accounts yet; open sign-up is on, so visit /signup to create one")
		return auth.Completed(), nil
	}

	setup, err := auth.NewSetup()
	if err != nil {
		return nil, err
	}
	// Printed rather than stored: the token lives only in this process, so a
	// restart invalidates it and there is nothing on disk to leak.
	s.log.Warn("no accounts exist — visit /signup with this one-time token",
		"token", setup.Token())
	return setup, nil
}

// Run starts every selected component and blocks until the context is cancelled
// or one of them fails.
//
// A failure in either stops the other. Half a system running is worse than none:
// an interface with no worker silently stops observing, and a worker with no
// interface leaves nobody able to see what it is doing.
func (s *System) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	failures := make(chan error, 2)

	if s.Interface != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.log.Info("interface listening", "addr", s.Config.Addr)
			if err := s.Interface.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failures <- fmt.Errorf("interface: %w", err)
			}
		}()
	}
	if s.Worker != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.log.Info("worker started")
			if err := s.Worker.Run(ctx); err != nil && ctx.Err() == nil {
				failures <- fmt.Errorf("worker: %w", err)
			}
		}()
	}

	var failure error
	select {
	case <-ctx.Done():
	case failure = <-failures:
	}

	s.shutdown()
	wg.Wait()
	return failure
}

// shutdown stops the interface, giving in-flight requests a moment to finish.
//
// The worker is not waited on. It stops when its context is cancelled, and a
// migration already in flight should be allowed to finish rather than being cut
// short — killing one halfway is how a concurrent index build leaves an invalid
// index behind.
func (s *System) shutdown() {
	if s.Interface == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Interface.Shutdown(ctx); err != nil {
		s.log.Warn("the interface did not shut down cleanly", "error", err)
	}
}

// Close releases the system's resources.
func (s *System) Close() {
	if s.Pool != nil {
		s.Pool.Close()
	}
}
