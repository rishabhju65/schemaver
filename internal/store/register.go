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
func (s *Scope) RegisterInstance(
	ctx context.Context, name, host string, port int,
	tlsMode, username, password string,
) (int64, error) {
	if s.store.box == nil {
		return 0, fmt.Errorf("no encryption key configured; set %s", encryptionKeyHint)
	}
	ciphertext, err := s.store.box.Seal(password)
	if err != nil {
		return 0, fmt.Errorf("encrypt credential: %w", err)
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var credentialID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.credential
		    (name, username, kind, secret_ciphertext, key_id, account_id)
		VALUES ($1, $2, 'inline', $3, $4, $5)
		RETURNING id`,
		name+" credential", username, ciphertext, s.store.box.KeyID(),
		s.account).Scan(&credentialID); err != nil {
		return 0, fmt.Errorf("store credential: %w", err)
	}

	var instanceID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.instance (name, host, port, tls_mode, credential_id, account_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		name, host, port, tlsMode, credentialID, s.account).Scan(&instanceID); err != nil {
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
func (s *Scope) SetManaged(ctx context.Context, instanceID int64, names []string) (int, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if len(names) == 0 {
		tag, err = s.store.pool.Exec(ctx, `
			UPDATE schemaver.database SET managed = true
			 WHERE instance_id = $1 AND archived_at IS NULL
			   AND instance_id IN (SELECT id FROM schemaver.instance WHERE account_id = $2)`,
			instanceID, s.account)
	} else {
		tag, err = s.store.pool.Exec(ctx, `
			UPDATE schemaver.database SET managed = true
			 WHERE instance_id = $1 AND archived_at IS NULL AND name = ANY($2)
			   AND instance_id IN (SELECT id FROM schemaver.instance WHERE account_id = $3)`,
			instanceID, names, s.account)
	}
	if err != nil {
		return 0, fmt.Errorf("mark databases managed: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PairForDrift makes one database's expectation another database, which is what
// enables drift detection before any repository is connected.
func (s *Scope) PairForDrift(ctx context.Context, databaseID, peerID int64) error {
	if databaseID == peerID {
		return fmt.Errorf("a database cannot be compared against itself")
	}
	// Both sides must belong to this account, or one account could learn of
	// another's databases by probing ids.
	if _, err := s.store.pool.Exec(ctx, `
		UPDATE schemaver.database d
		   SET expected_peer_id = $2, repository_id = NULL
		  FROM schemaver.instance i, schemaver.instance p, schemaver.database pd
		 WHERE d.id = $1 AND d.instance_id = i.id AND i.account_id = $3
		   AND pd.id = $2 AND pd.instance_id = p.id AND p.account_id = $3`,
		databaseID, peerID, s.account); err != nil {
		return fmt.Errorf("pair databases: %w", err)
	}
	return nil
}

// DatabaseIDByName resolves an instance-scoped database name to its id.
func (s *Scope) DatabaseIDByName(ctx context.Context, instanceID int64, name string) (int64, error) {
	var id int64
	if err := s.store.pool.QueryRow(ctx, `
		SELECT d.id FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.instance_id = $1 AND d.name = $2 AND i.account_id = $3`,
		instanceID, name, s.account).Scan(&id); err != nil {
		return 0, fmt.Errorf("find database %q: %w", name, err)
	}
	return id, nil
}
