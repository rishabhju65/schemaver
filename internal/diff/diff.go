package diff

import (
	"fmt"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// Result is everything a diff produces.
type Result struct {
	// Changes are in execution order.
	Changes []Change `json:"changes"`
	// Renames are drop-and-add pairs that look like a rename. They are
	// proposals, never applied: see rename.go for why a human has to decide.
	Renames []RenameCandidate `json:"renames,omitempty"`
	Summary Summary           `json:"summary"`
}

// Empty reports whether the two schemas are equivalent.
func (r Result) Empty() bool { return len(r.Changes) == 0 }

// Compute reports what must happen to turn from into to.
//
// Both sides are normalized first, so a difference here is a real difference and
// never an artifact of how either schema was read or assembled.
func Compute(from, to *schema.Schema) Result { return ComputeWith(from, to, nil) }

// ComputeWith diffs two schemas, treating the given renames as settled.
//
// A rename and a drop-plus-add are indistinguishable from the schemas, and not
// remotely equivalent — one keeps the data and the other destroys it — so the
// engine never decides. It proposes candidates and somebody answers. This is
// where an answer of "yes, renamed" takes effect: the drop and the add collapse
// into a single rename, plus whatever else about the column actually changed,
// since confirming a rename says nothing about the column's type or nullability
// having stayed the same.
//
// Both schemas are unchanged either way: a rename produces the same end state
// as the drop-and-add it replaces, which is why the migration's declared target
// does not move and the rehearsal is unaffected.
func ComputeWith(from, to *schema.Schema, confirmed []Rename) Result {
	a, b := normalized(from), normalized(to)

	var changes []Change
	changes = append(changes, namespaceChanges(a, b)...)

	for name, ns := range indexNamespaces(a) {
		other, present := indexNamespaces(b)[name]
		if !present {
			continue // handled as DropNamespace
		}
		changes = append(changes, enumChanges(name, ns, other)...)
		changes = append(changes, sequenceChanges(name, ns, other)...)
		changes = append(changes, tableChanges(name, ns, other, confirmed)...)
	}

	ordered := Order(changes)
	renames := findRenames(ordered)
	renames = append(renames, findTableRenames(ordered, a, b)...)
	return Result{
		Changes: ordered,
		Renames: renames,
		Summary: Count(ordered),
	}
}

// normalized returns a canonical copy, so Compute never mutates its arguments.
func normalized(s *schema.Schema) *schema.Schema {
	if s == nil {
		return &schema.Schema{}
	}
	c := *s
	out := &c
	schema.Normalize(out)
	return out
}

func indexNamespaces(s *schema.Schema) map[string]*schema.Namespace {
	out := map[string]*schema.Namespace{}
	for i := range s.Namespaces {
		out[s.Namespaces[i].Name] = &s.Namespaces[i]
	}
	return out
}

func namespaceChanges(a, b *schema.Schema) []Change {
	before, after := indexNamespaces(a), indexNamespaces(b)
	var out []Change

	for name := range before {
		if _, present := after[name]; !present {
			out = append(out, newChange(DropNamespace, name, "", "",
				fmt.Sprintf("drop schema %s and everything in it", name)))
		}
	}
	for name, ns := range after {
		old, present := before[name]
		if !present {
			out = append(out, newChange(CreateNamespace, name, "", "",
				fmt.Sprintf("create schema %s", name)))
			// Everything inside a new namespace is new.
			out = append(out, contentsOf(name, ns)...)
			continue
		}
		if old.Comment != ns.Comment {
			c := newChange(SetComment, name, "", "",
				fmt.Sprintf("change the comment on schema %s", name))
			c.From, c.To = old.Comment, ns.Comment
			out = append(out, c)
		}
	}
	return out
}

// contentsOf enumerates everything in a namespace as newly created.
func contentsOf(ns string, n *schema.Namespace) []Change {
	var out []Change
	for _, e := range n.Enums {
		out = append(out, newChange(CreateEnum, ns, "", e.Name,
			fmt.Sprintf("create type %s.%s", ns, e.Name)))
	}
	for _, s := range n.Sequences {
		if s.Owned() {
			continue
		}
		out = append(out, newChange(CreateSequence, ns, "", s.Name,
			fmt.Sprintf("create sequence %s.%s", ns, s.Name)))
	}
	for _, t := range n.Tables {
		out = append(out, tableCreation(ns, t)...)
	}
	return out
}

// tableCreation is a new table plus the indexes and foreign keys that are
// separate statements. Non-foreign-key constraints ride along with CREATE TABLE
// and are not reported separately, because they cost nothing extra on an empty
// table and listing them would make a new table read as a dozen changes.
func tableCreation(ns string, t schema.Table) []Change {
	out := []Change{newChange(CreateTable, ns, t.Name, "",
		fmt.Sprintf("create table %s.%s with %d columns", ns, t.Name, len(t.Columns)))}
	for _, c := range t.Constraints {
		if c.Type == schema.ForeignKey {
			out = append(out, newChange(AddConstraint, ns, t.Name, c.Name,
				fmt.Sprintf("add foreign key %s on %s.%s", c.Name, ns, t.Name)))
		}
	}
	for _, i := range t.Indexes {
		out = append(out, newChange(CreateIndex, ns, t.Name, i.Name,
			fmt.Sprintf("create index %s on %s.%s", i.Name, ns, t.Name)))
	}
	return out
}

func enumChanges(ns string, a, b *schema.Namespace) []Change {
	before := map[string]schema.Enum{}
	for _, e := range a.Enums {
		before[e.Name] = e
	}
	after := map[string]schema.Enum{}
	for _, e := range b.Enums {
		after[e.Name] = e
	}

	var out []Change
	for name := range before {
		if _, ok := after[name]; !ok {
			out = append(out, newChange(DropEnum, ns, "", name,
				fmt.Sprintf("drop type %s.%s", ns, name)))
		}
	}
	for name, e := range after {
		old, ok := before[name]
		if !ok {
			out = append(out, newChange(CreateEnum, ns, "", name,
				fmt.Sprintf("create type %s.%s", ns, name)))
			continue
		}
		out = append(out, enumLabelChanges(ns, name, old, e)...)
	}
	return out
}

// enumLabelChanges distinguishes appending labels — cheap and safe — from any
// other rearrangement, which PostgreSQL cannot express as a simple alteration.
func enumLabelChanges(ns, name string, old, next schema.Enum) []Change {
	if equalStrings(old.Labels, next.Labels) {
		return nil
	}
	if len(next.Labels) > len(old.Labels) &&
		equalStrings(old.Labels, next.Labels[:len(old.Labels)]) {
		var out []Change
		for _, label := range next.Labels[len(old.Labels):] {
			c := newChange(AddEnumLabel, ns, "", name,
				fmt.Sprintf("add value %q to type %s.%s", label, ns, name))
			c.To = label
			out = append(out, c)
		}
		return out
	}
	c := newChange(AlterEnum, ns, "", name,
		fmt.Sprintf("reorder or remove values of type %s.%s, which requires recreating it", ns, name))
	c.From, c.To = fmt.Sprint(old.Labels), fmt.Sprint(next.Labels)
	return []Change{c}
}

func sequenceChanges(ns string, a, b *schema.Namespace) []Change {
	before := map[string]schema.Sequence{}
	for _, s := range a.Sequences {
		if !s.Owned() {
			before[s.Name] = s
		}
	}
	after := map[string]schema.Sequence{}
	for _, s := range b.Sequences {
		if !s.Owned() {
			after[s.Name] = s
		}
	}

	var out []Change
	for name := range before {
		if _, ok := after[name]; !ok {
			out = append(out, newChange(DropSequence, ns, "", name,
				fmt.Sprintf("drop sequence %s.%s", ns, name)))
		}
	}
	for name, s := range after {
		old, ok := before[name]
		if !ok {
			out = append(out, newChange(CreateSequence, ns, "", name,
				fmt.Sprintf("create sequence %s.%s", ns, name)))
			continue
		}
		if old != s {
			out = append(out, newChange(AlterSequence, ns, "", name,
				fmt.Sprintf("change sequence %s.%s", ns, name)))
		}
	}
	return out
}

func tableChanges(ns string, a, b *schema.Namespace, confirmed []Rename) []Change {
	before := map[string]schema.Table{}
	for _, t := range a.Tables {
		before[t.Name] = t
	}
	after := map[string]schema.Table{}
	for _, t := range b.Tables {
		after[t.Name] = t
	}

	// Table renames confirmed for this namespace, read both ways: the drop and
	// the create are found in separate passes below and each has to know it is
	// half of one rename.
	//
	// Checked against the schemas rather than trusted, for the same reason the
	// column case is: an answer kept from before they moved must not rename a
	// table that is not there, or into a name that no longer appears.
	renamedFrom := map[string]string{}
	renamedTo := map[string]string{}
	for _, r := range confirmed {
		if !r.OfTable() || r.Namespace != ns {
			continue
		}
		if _, present := before[r.From]; !present {
			continue
		}
		if _, arrived := after[r.To]; !arrived {
			continue
		}
		renamedFrom[r.From] = r.To
		renamedTo[r.To] = r.From
	}

	var out []Change
	for name := range before {
		if to, renamed := renamedFrom[name]; renamed {
			c := newChange(RenameTable, ns, name, "",
				fmt.Sprintf("rename table %s.%s to %s, keeping its rows", ns, name, to))
			c.From, c.To = name, to
			out = append(out, c)
			continue
		}
		if _, ok := after[name]; !ok {
			out = append(out, newChange(DropTable, ns, name, "",
				fmt.Sprintf("drop table %s.%s and all its rows", ns, name)))
		}
	}
	for name, t := range after {
		old, ok := before[name]
		// A name whose table was renamed away is free again. What stands here
		// under it is a new table that happens to have inherited the name, not
		// the one that used to hold it — that one is now called something else
		// and is being compared under its new name below.
		if _, renamedAway := renamedFrom[name]; renamedAway {
			ok = false
		}
		if !ok {
			// A table that arrived under a new name is the one that left under
			// the old one, so what it needs is not creating but whatever else
			// changed about it — compared under the name it now has, because
			// the rename is ordered before every change here.
			from, renamed := renamedTo[name]
			if !renamed {
				out = append(out, tableCreation(ns, t)...)
				continue
			}
			old = before[from]
		}
		out = append(out, columnChanges(ns, name, old, t, confirmed)...)
		out = append(out, constraintChanges(ns, name, old, t)...)
		out = append(out, indexChanges(ns, name, old, t)...)
		if old.Comment != t.Comment {
			c := newChange(SetComment, ns, name, "",
				fmt.Sprintf("change the comment on %s.%s", ns, name))
			c.From, c.To = old.Comment, t.Comment
			out = append(out, c)
		}
	}
	return out
}

func columnChanges(ns, table string, a, b schema.Table, confirmed []Rename) []Change {
	before := map[string]schema.Column{}
	for _, c := range a.Columns {
		before[c.Name] = c
	}
	after := map[string]schema.Column{}
	for _, c := range b.Columns {
		after[c.Name] = c
	}

	// Renames confirmed for this table, read both ways: by the name going and
	// by the name arriving, because the drop and the add are found in separate
	// passes below and each has to know it is half of one.
	renamedFrom := map[string]string{}
	renamedTo := map[string]string{}
	for _, r := range confirmed {
		if r.Namespace != ns || r.Table != table {
			continue
		}
		// Only where the schemas still support it. An answer kept from before
		// they moved must not rename a column that is not there, or into a name
		// that no longer appears.
		//
		// Deliberately not also requiring the old name to be gone from the
		// target. "Rename note to remark, and add a new note" is a real thing
		// to do, and it is the case where honouring the answer matters most:
		// refusing here would turn it back into the drop-and-add that discards
		// the data, which is exactly what the answer was given to prevent.
		if _, present := before[r.From]; !present {
			continue
		}
		if _, arrived := after[r.To]; !arrived {
			continue
		}
		renamedFrom[r.From] = r.To
		renamedTo[r.To] = r.From
	}

	var out []Change
	for name := range before {
		if to, renamed := renamedFrom[name]; renamed {
			c := newChange(RenameColumn, ns, table, name,
				fmt.Sprintf("rename %s.%s.%s to %s, keeping its data", ns, table, name, to))
			c.From, c.To = name, to
			c.typeHint = before[name].Type
			out = append(out, c)
			continue
		}
		if _, ok := after[name]; !ok {
			c := newChange(DropColumn, ns, table, name,
				fmt.Sprintf("drop column %s from %s.%s, discarding its data", name, ns, table))
			c.typeHint = before[name].Type
			out = append(out, c)
		}
	}
	for name, col := range after {
		if from, renamed := renamedTo[name]; renamed {
			// The rename itself is emitted above. What is left is whatever else
			// about the column differs — a widened type, a dropped default —
			// which a rename does not carry and which would otherwise be lost
			// with the add that used to describe it.
			was := before[from]
			was.Name = name
			out = append(out, alteredColumn(ns, table, was, col)...)
			continue
		}
		old, ok := before[name]
		// A name a rename has vacated is free, so whatever arrives at it is a
		// new column rather than an alteration of the one that left. Read the
		// other way round it would describe changing a column that is no longer
		// there under that name.
		if _, vacated := renamedFrom[name]; vacated {
			ok = false
		}
		if !ok {
			c := newChange(AddColumn, ns, table, name,
				fmt.Sprintf("add column %s %s to %s.%s", name, col.Type, ns, table))
			c.typeHint = col.Type
			out = append(out, c)
			continue
		}
		out = append(out, alteredColumn(ns, table, old, col)...)
	}
	return out
}

// alteredColumn reports each property of a column that differs as its own
// change, because they have different costs: widening a type rewrites the
// table, while setting a default touches only the catalogue.
func alteredColumn(ns, table string, old, next schema.Column) []Change {
	var out []Change
	name := next.Name

	if old.Type != next.Type {
		c := newChange(AlterColumnType, ns, table, name,
			fmt.Sprintf("change %s.%s.%s from %s to %s", ns, table, name, old.Type, next.Type))
		c.From, c.To = old.Type, next.Type
		out = append(out, c)
	}
	if old.Nullable && !next.Nullable {
		out = append(out, newChange(SetNotNull, ns, table, name,
			fmt.Sprintf("require %s.%s.%s to be non-null, which scans every existing row", ns, table, name)))
	}
	if !old.Nullable && next.Nullable {
		out = append(out, newChange(DropNotNull, ns, table, name,
			fmt.Sprintf("allow %s.%s.%s to be null", ns, table, name)))
	}
	if old.Default != next.Default {
		kind, verb := SetDefault, "set"
		if next.Default == "" {
			kind, verb = DropDefault, "remove"
		}
		c := newChange(kind, ns, table, name,
			fmt.Sprintf("%s the default on %s.%s.%s", verb, ns, table, name))
		c.From, c.To = old.Default, next.Default
		out = append(out, c)
	}
	if old.Identity != next.Identity || old.Generated != next.Generated ||
		old.Collation != next.Collation {
		out = append(out, newChange(AlterColumnOther, ns, table, name,
			fmt.Sprintf("change how %s.%s.%s is generated or collated", ns, table, name)))
	}
	if old.Comment != next.Comment {
		c := newChange(SetComment, ns, table, name,
			fmt.Sprintf("change the comment on %s.%s.%s", ns, table, name))
		c.From, c.To = old.Comment, next.Comment
		out = append(out, c)
	}
	return out
}

func constraintChanges(ns, table string, a, b schema.Table) []Change {
	before := map[string]schema.Constraint{}
	for _, c := range a.Constraints {
		before[c.Name] = c
	}
	after := map[string]schema.Constraint{}
	for _, c := range b.Constraints {
		after[c.Name] = c
	}

	var out []Change
	for name, old := range before {
		next, ok := after[name]
		if !ok {
			out = append(out, newChange(DropConstraint, ns, table, name,
				fmt.Sprintf("drop constraint %s on %s.%s", name, ns, table)))
			continue
		}
		// A constraint cannot be altered in place; a changed one is a drop
		// followed by an add, and reporting it as two changes is the truth
		// rather than a convenience.
		if !sameConstraint(old, next) {
			out = append(out,
				newChange(DropConstraint, ns, table, name,
					fmt.Sprintf("drop constraint %s on %s.%s so it can be redefined", name, ns, table)),
				newChange(AddConstraint, ns, table, name,
					fmt.Sprintf("add the redefined constraint %s on %s.%s", name, ns, table)))
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			out = append(out, newChange(AddConstraint, ns, table, name,
				fmt.Sprintf("add constraint %s on %s.%s, which verifies every existing row", name, ns, table)))
		}
	}
	return out
}

func indexChanges(ns, table string, a, b schema.Table) []Change {
	before := map[string]schema.Index{}
	for _, i := range a.Indexes {
		before[i.Name] = i
	}
	after := map[string]schema.Index{}
	for _, i := range b.Indexes {
		after[i.Name] = i
	}

	var out []Change
	for name, old := range before {
		next, ok := after[name]
		if !ok {
			out = append(out, newChange(DropIndex, ns, table, name,
				fmt.Sprintf("drop index %s on %s.%s", name, ns, table)))
			continue
		}
		if !sameIndex(old, next) {
			out = append(out,
				newChange(DropIndex, ns, table, name,
					fmt.Sprintf("drop index %s on %s.%s so it can be rebuilt", name, ns, table)),
				newChange(CreateIndex, ns, table, name,
					fmt.Sprintf("rebuild index %s on %s.%s", name, ns, table)))
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			out = append(out, newChange(CreateIndex, ns, table, name,
				fmt.Sprintf("create index %s on %s.%s", name, ns, table)))
		}
	}
	return out
}

// sameConstraint compares the structured form only. The engine's rendered
// definition is excluded: its formatting can differ between engine versions
// without the constraint having changed, and a diff that reported that would
// make an upgrade look like a schema change.
func sameConstraint(a, b schema.Constraint) bool {
	return a.Type == b.Type &&
		equalStrings(a.Columns, b.Columns) &&
		a.RefNamespace == b.RefNamespace && a.RefTable == b.RefTable &&
		equalStrings(a.RefColumns, b.RefColumns) &&
		a.OnDelete == b.OnDelete && a.OnUpdate == b.OnUpdate &&
		a.Expression == b.Expression &&
		a.Deferrable == b.Deferrable && a.InitiallyDeferred == b.InitiallyDeferred
}

func sameIndex(a, b schema.Index) bool {
	// Validity counts. An index left behind by a failed concurrent build
	// describes the same columns as the one that was wanted and is not the same
	// object: one is used and the other never will be. Comparing them as equal
	// is what let a failed build read back as success.
	return a.Unique == b.Unique && a.Method == b.Method &&
		equalStrings(a.Columns, b.Columns) && equalStrings(a.Include, b.Include) &&
		a.Predicate == b.Predicate && a.Invalid == b.Invalid
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
