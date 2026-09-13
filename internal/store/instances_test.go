package store

import "testing"

// TestMaintenanceDatabaseIsMarked covers the database somebody finds on a
// server they just registered and is certain they did not create.
//
// Every PostgreSQL installation has one called postgres, made by initdb as a
// place to connect before any real database exists. Discovery reads pg_database
// and reports the truth, but a hosted provider's console may not: a Neon project
// lists one database in its dashboard and has two on the endpoint, because Neon
// creates this one and does not surface it through its API. Without a note on
// the row, the honest answer looks like a bug in the discovery.
func TestMaintenanceDatabaseIsMarked(t *testing.T) {
	for name, want := range map[string]bool{
		"postgres":     true,
		"neondb":       false,
		"defaultdb":    false,
		"shop_prod":    false,
		"postgres_old": false,
		"my_postgres":  false,
	} {
		if got := (ManagedDatabase{Name: name}).Maintenance(); got != want {
			t.Errorf("%q: maintenance=%v, want %v", name, got, want)
		}
	}
}
