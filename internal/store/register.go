package store

import (
	"context"
	"fmt"
)

// RegisterInstance records a Postgres server and the credential used to reach
// it, returning the new instance id.
//
// The password is encrypted before it is stored and never written anywhere in
// clear. Registering the same host and port twice is refused by a unique index
// rather than silently creating a second, divergent history of one server.
func (s *Store) RegisterInstance(
	ctx context.Context, name, host string, port int,
	tlsMode, username, password string,
) (int64, error) {
	if s.box == nil {
		return 0, fmt.Errorf("no encryption key configured; set %s", encryptionKeyHint)
	}
	ciphertext, err := s.box.Seal(password)
	if err != nil {
		return 0, fmt.Errorf("encrypt credential: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var credentialID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.credential
		    (name, username, kind, secret_ciphertext, key_id)
		VALUES ($1, $2, 'inline', $3, $4)
		RETURNING id`,
		name+" credential", username, ciphertext, s.box.KeyID()).Scan(&credentialID); err != nil {
		return 0, fmt.Errorf("store credential: %w", err)
	}

	var instanceID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.instance (name, host, port, tls_mode, credential_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		name, host, port, tlsMode, credentialID).Scan(&instanceID); err != nil {
		return 0, fmt.Errorf("register instance: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return instanceID, nil
}

// encryptionKeyHint names the variable an operator has to set. Kept here so the
// message stays consistent wherever a missing key is reported.
const encryptionKeyHint = "SCHEMAVER_ENCRYPTION_KEY"

// SetManaged marks databases on an instance for observation.
//
// Discovery finds every database; managing one is a separate, deliberate act.
// Passing no names manages all of them, which is the right default for a server
// that exists to be watched but must still be chosen explicitly.
func (s *Store) SetManaged(ctx context.Context, instanceID int64, names []string) (int, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if len(names) == 0 {
		tag, err = s.pool.Exec(ctx, `
			UPDATE schemaver.database SET managed = true
			 WHERE instance_id = $1 AND archived_at IS NULL`, instanceID)
	} else {
		tag, err = s.pool.Exec(ctx, `
			UPDATE schemaver.database SET managed = true
			 WHERE instance_id = $1 AND archived_at IS NULL AND name = ANY($2)`,
			instanceID, names)
	}
	if err != nil {
		return 0, fmt.Errorf("mark databases managed: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PairForDrift makes one database's expectation another database, which is what
// enables drift detection before any repository is connected.
func (s *Store) PairForDrift(ctx context.Context, databaseID, peerID int64) error {
	if databaseID == peerID {
		return fmt.Errorf("a database cannot be compared against itself")
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.database
		   SET expected_peer_id = $2, repository_id = NULL
		 WHERE id = $1`, databaseID, peerID); err != nil {
		return fmt.Errorf("pair databases: %w", err)
	}
	return nil
}

// DatabaseIDByName resolves an instance-scoped database name to its id.
func (s *Store) DatabaseIDByName(ctx context.Context, instanceID int64, name string) (int64, error) {
	var id int64
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM schemaver.database WHERE instance_id = $1 AND name = $2`,
		instanceID, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("find database %q: %w", name, err)
	}
	return id, nil
}
