// Package schema defines the canonical representation of a database schema.
//
// Everything downstream — diffing, planning, drift detection, version identity —
// reads this model and nothing else. Two rules govern it:
//
//  1. The model is engine-neutral in shape. Postgres is the only engine today
//     (D-003), but no type here names it.
//  2. The model is *canonical*: the same logical schema must produce an
//     identical model regardless of how it was written or where it was read
//     from. Normalization (normalize.go) enforces this, and version identity
//     (hash.go) depends on it absolutely.
package schema

// Schema is a complete database schema — the unit that gets versioned, diffed,
// and compared against reality.
type Schema struct {
	Namespaces []Namespace `json:"namespaces"`
}

// Namespace is a SQL schema: a named container for tables and types.
type Namespace struct {
	Name      string     `json:"name"`
	Comment   string     `json:"comment,omitempty"`
	Tables    []Table    `json:"tables,omitempty"`
	Sequences []Sequence `json:"sequences,omitempty"`
	Enums     []Enum     `json:"enums,omitempty"`
}

// Table is a base or partitioned table.
type Table struct {
	Name        string       `json:"name"`
	Comment     string       `json:"comment,omitempty"`
	Columns     []Column     `json:"columns"`
	Constraints []Constraint `json:"constraints,omitempty"`
	Indexes     []Index      `json:"indexes,omitempty"`

	// Partitioned reports whether this table is partitioned. PartitionKey holds
	// the engine's canonical rendering of the partition key when it is.
	Partitioned  bool   `json:"partitioned,omitempty"`
	PartitionKey string `json:"partition_key,omitempty"`
}

// Column is a single column of a table.
//
// Position is deliberately absent. Physical column order is an artifact of the
// order changes happened to be applied in, not a property of the schema, and
// including it would make two logically identical schemas hash differently.
// Ordering for display is by name (see normalize.go).
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Comment  string `json:"comment,omitempty"`

	// Default is the canonical rendering of the column default, empty when
	// there is none.
	Default string `json:"default,omitempty"`

	// Identity is "", "ALWAYS", or "BY DEFAULT".
	Identity string `json:"identity,omitempty"`

	// Generated holds the generation expression for a generated column, empty
	// otherwise.
	Generated string `json:"generated,omitempty"`

	// Collation is recorded only when it differs from the type's default, since
	// the default is not a property the author chose.
	Collation string `json:"collation,omitempty"`
}

// ConstraintType enumerates the kinds of table constraint the model carries.
type ConstraintType string

const (
	PrimaryKey ConstraintType = "primary_key"
	ForeignKey ConstraintType = "foreign_key"
	Unique     ConstraintType = "unique"
	Check      ConstraintType = "check"
	Exclusion  ConstraintType = "exclusion"
)

// Constraint is a table constraint.
//
// Both a structured form and a rendered Definition are kept. The structured
// fields are what the diff engine reasons over — they are what let it say "a
// column was added to this key" rather than "this text changed". Definition is
// the engine's own canonical rendering, kept for display and for reproducing
// constructs too intricate to rebuild structurally; it is stored but excluded
// from version identity, because its formatting can shift between engine
// versions without the schema having changed.
type Constraint struct {
	Name       string         `json:"name"`
	Type       ConstraintType `json:"type"`
	Columns    []string       `json:"columns,omitempty"`
	Definition string         `json:"definition,omitempty"`

	// Foreign key fields, populated only when Type is ForeignKey.
	RefNamespace string   `json:"ref_namespace,omitempty"`
	RefTable     string   `json:"ref_table,omitempty"`
	RefColumns   []string `json:"ref_columns,omitempty"`
	OnDelete     string   `json:"on_delete,omitempty"`
	OnUpdate     string   `json:"on_update,omitempty"`

	// Expression holds the check or exclusion predicate.
	Expression string `json:"expression,omitempty"`

	// Deferrable reports whether the constraint can be deferred, and
	// InitiallyDeferred whether it is by default. Both matter for planning:
	// circular foreign keys are only satisfiable with deferred constraints.
	Deferrable        bool `json:"deferrable,omitempty"`
	InitiallyDeferred bool `json:"initially_deferred,omitempty"`
}

// Index is a table index.
//
// Constraint-backed indexes (those the engine creates to enforce a primary key
// or unique constraint) are not represented here — they belong to their
// constraint. Recording them in both places would make a single logical object
// appear twice in every diff.
type Index struct {
	Name    string `json:"name"`
	Unique  bool   `json:"unique"`
	Method  string `json:"method"`
	Comment string `json:"comment,omitempty"`

	// Columns holds the indexed columns or expressions, in index order. Order is
	// significant here and must not be sorted.
	Columns []string `json:"columns"`

	// Include holds non-key payload columns.
	Include []string `json:"include,omitempty"`

	// Predicate is the WHERE clause of a partial index, empty otherwise.
	Predicate string `json:"predicate,omitempty"`

	// Invalid marks an index the engine will not use.
	//
	// A `CREATE INDEX CONCURRENTLY` that fails leaves the index behind marked
	// invalid: it is in the catalogue, it has a name, `pg_get_indexdef` renders
	// it identically to a working one, and no query will ever touch it. Without
	// this field such an index was indistinguishable from the real thing, so a
	// build that failed after three hours on a large table read back as the
	// schema it was trying to reach — the migration looked done, drift saw
	// nothing, and the planner quietly ignored the index forever.
	//
	// omitempty is load-bearing rather than tidiness: a valid index serializes
	// exactly as it did before this field existed, so no stored fingerprint
	// moves and nothing has to be re-baselined.
	Invalid bool `json:"invalid,omitempty"`

	Definition string `json:"definition,omitempty"`
}

// Sequence is a sequence generator.
type Sequence struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Start     int64  `json:"start"`
	Increment int64  `json:"increment"`
	Min       int64  `json:"min"`
	Max       int64  `json:"max"`
	Cache     int64  `json:"cache"`
	Cycle     bool   `json:"cycle"`
	Comment   string `json:"comment,omitempty"`

	// OwnedByTable and OwnedByColumn are set for sequences the engine created to
	// back an identity or serial column. Such sequences are owned by that column
	// and must never be diffed as independent objects.
	OwnedByTable  string `json:"owned_by_table,omitempty"`
	OwnedByColumn string `json:"owned_by_column,omitempty"`
}

// Owned reports whether the sequence belongs to a column rather than standing
// on its own.
func (s Sequence) Owned() bool { return s.OwnedByTable != "" }

// Enum is an enumerated type.
//
// Label order is significant — it defines the type's sort order — and is
// therefore never sorted during normalization.
type Enum struct {
	Name    string   `json:"name"`
	Labels  []string `json:"labels"`
	Comment string   `json:"comment,omitempty"`
}

// Without returns a copy of s with the named namespaces removed.
//
// Applied after a database is read rather than pushed into the queries that
// read it. The engine is the authority on what is there; which of it this
// project cares about is a separate question, and answering it in one place
// means every reader of a schema gets the same answer.
//
// A foreign key pointing into an excluded namespace keeps its reference. The
// constraint genuinely does point there, and rewriting it to say otherwise
// would be inventing a schema nobody has.
func (s *Schema) Without(namespaces []string) *Schema {
	if len(namespaces) == 0 || s == nil {
		return s
	}
	drop := make(map[string]bool, len(namespaces))
	for _, n := range namespaces {
		drop[n] = true
	}
	out := &Schema{}
	for _, ns := range s.Namespaces {
		if drop[ns.Name] {
			continue
		}
		out.Namespaces = append(out.Namespaces, ns)
	}
	return out
}
