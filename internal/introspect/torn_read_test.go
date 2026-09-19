package introspect

import (
	"errors"
	"strings"
	"testing"
)

// TestAnIndexDroppedMidReadAbandonsTheRead is the hazard a repeatable-read
// transaction does not cover.
//
// pg_index is bound by the transaction's snapshot; pg_get_indexdef is not. It
// reads the catalogue as it stands now, so an index dropped while the read is
// running answers NULL for every one of its keys while pg_index still reports
// the index as present. No isolation level prevents this.
//
// The read is abandoned rather than patched up. An index recorded without its
// keys is a different schema from the one really there, and a schema's
// fingerprint is its identity — writing a half-read one would report drift that
// is schemaver's own reading rather than anything that changed.
func TestAnIndexDroppedMidReadAbandonsTheRead(t *testing.T) {
	total := "total"
	got, err := definite([]*string{&total})
	if err != nil {
		t.Fatalf("a complete key list was refused: %v", err)
	}
	if len(got) != 1 || got[0] != "total" {
		t.Errorf("got %v, want [total]", got)
	}

	if _, err := definite([]*string{&total, nil}); err == nil {
		t.Fatal("an index missing a key was accepted, so a schema that was " +
			"never really there would be recorded and fingerprinted")
	} else {
		if !errors.Is(err, ErrRead) {
			t.Errorf("not reported as a read that has to be retried: %v", err)
		}
		if !strings.Contains(err.Error(), "dropped while this read") {
			t.Errorf("the reason does not say what happened: %v", err)
		}
	}

	// An index with no keys at all is not the same thing and is left alone.
	if _, err := definite(nil); err != nil {
		t.Errorf("an empty key list was refused: %v", err)
	}
}
