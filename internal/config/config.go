// Package config reads a deployment's settings once, in one place.
//
// It exists because the alternative — reading the environment wherever a value
// happens to be needed — leaves no single answer to "what is this deployment
// configured to do". That question has to be answerable before a deployment is
// trusted with production credentials, and it has to be answerable from one
// file rather than by grepping.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rishabhju65/schemaver/internal/netguard"
	"github.com/rishabhju65/schemaver/internal/secret"
)

// Components selects which parts of the system run in this process.
//
// The bundled and split deployments differ only here: one process with both, or
// two processes with one each, pointed at the same metadata database. Nothing
// else changes, which is what keeps the split a deployment decision rather than
// a code path.
type Components struct {
	// Interface serves the web application.
	Interface bool
	// Work runs the observation loop and the migration executor.
	Work bool
}

// Everything runs the whole system in one process.
func Everything() Components { return Components{Interface: true, Work: true} }

// Config is a deployment's complete settings.
type Config struct {
	// DatabaseURL addresses schemaver's own metadata database.
	DatabaseURL string
	// Addr is where the interface listens, when it is running.
	Addr string

	Components Components

	// Secrets opens and seals stored credentials. Required by both components:
	// the interface seals a credential when a server is registered, and the
	// worker opens it to connect. There is no arrangement where only one holds
	// the key.
	Secrets *secret.Box

	// OpenSignup lets anyone create an organisation.
	OpenSignup bool
	// Targets decides which addresses a registration may point at.
	Targets netguard.Policy
	// PrivateTargetsOverridden records that private addresses were permitted
	// deliberately despite open sign-up, so the caller can say so loudly.
	PrivateTargetsOverridden bool

	// ShadowURL addresses a server where throwaway databases can be created to
	// prove a migration before it runs. The role needs CREATEDB.
	//
	// Defaults to the metadata database's own server, so the proof runs without
	// anyone configuring anything — a check nobody switched on is a check
	// nobody has. Point it elsewhere to keep the churn of created and dropped
	// databases off the server holding schemaver's own records.
	ShadowURL string
	// ShadowDefaulted records that nothing named a shadow server and the
	// metadata server was assumed, so the caller can say so rather than implying
	// it was chosen.
	ShadowDefaulted bool
}

// Environment variable names, gathered so the set is visible at a glance.
const (
	EnvDatabaseURL  = "SCHEMAVER_DATABASE_URL"
	EnvAddr         = "SCHEMAVER_ADDR"
	EnvPort         = "PORT"
	EnvOpenSignup   = "SCHEMAVER_OPEN_SIGNUP"
	EnvAllowPrivate = "SCHEMAVER_ALLOW_PRIVATE_TARGETS"
	EnvShadowURL    = "SCHEMAVER_SHADOW_URL"
)

// Load reads a deployment's settings.
//
// databaseURL overrides the environment when given, so a command argument still
// works; components selects what this process runs.
func Load(databaseURL string, components Components) (*Config, error) {
	cfg := &Config{
		DatabaseURL: databaseURL,
		Addr:        listenAddr(),
		Components:  components,
		OpenSignup:  boolEnv(EnvOpenSignup),
	}
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = os.Getenv(EnvDatabaseURL)
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf(
			"no metadata database: pass one as an argument or set %s", EnvDatabaseURL)
	}

	box, err := secret.FromEnv()
	if err != nil {
		return nil, err
	}
	cfg.Secrets = box

	if cfg.ShadowURL = os.Getenv(EnvShadowURL); cfg.ShadowURL == "" {
		cfg.ShadowURL, cfg.ShadowDefaulted = cfg.DatabaseURL, true
	}

	cfg.Targets, cfg.PrivateTargetsOverridden = targetPolicy(cfg.OpenSignup)
	return cfg, nil
}

// targetPolicy decides which addresses this deployment may connect to.
//
// Derived from who may create an account rather than configured separately: a
// deployment strangers can join must not be usable as a probe of the network it
// sits in, and a closed one has no reason to distrust its own operator — where
// reaching a private database is the entire point.
func targetPolicy(openSignup bool) (netguard.Policy, bool) {
	override := boolEnv(EnvAllowPrivate)
	if !openSignup {
		return netguard.Policy{AllowPrivate: true}, false
	}
	if override {
		return netguard.Policy{AllowPrivate: true}, true
	}
	return netguard.Policy{}, false
}

// listenAddr resolves where the interface listens.
//
// PORT is honoured because most hosting platforms assign one and expect the
// process to bind it; a service that ignores it fails its health check and is
// killed without explaining why.
func listenAddr() string {
	if addr := os.Getenv(EnvAddr); addr != "" {
		return addr
	}
	if port := os.Getenv(EnvPort); port != "" {
		return ":" + port
	}
	return ":8080"
}

func boolEnv(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// Describe renders the settings a person would want confirmed at startup,
// naming nothing secret.
func (c *Config) Describe() []any {
	out := []any{
		"interface", c.Components.Interface,
		"worker", c.Components.Work,
		"open_signup", c.OpenSignup,
		"private_targets", c.Targets.AllowPrivate,
	}
	if c.Components.Interface {
		out = append(out, "addr", c.Addr)
	}
	return out
}

// Warnings reports combinations that are legal but worth saying out loud.
func (c *Config) Warnings() []string {
	var out []string
	if c.PrivateTargetsOverridden {
		out = append(out,
			"open sign-up is on AND private targets are permitted — anyone who "+
				"registers can make this server probe its own network")
	}
	if !c.Components.Interface && !c.Components.Work {
		out = append(out, "neither the interface nor the worker is enabled; this process will do nothing")
	}
	return out
}

// ErrNothingToRun is returned when a configuration would start no components.
var ErrNothingToRun = errors.New("no components enabled")
