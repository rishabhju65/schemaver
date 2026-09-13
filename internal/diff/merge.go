package diff

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// Conflict is one object the two sides disagree about.
//
// Disagreement is specific: both changed the object since their last common
// ancestor, and did not make the same change. Anything else — one side moved,
// or both made the identical change — has an answer that does not need a
// person.
type Conflict struct {
	// Object is the qualified name, as it appears in a diff.
	Object string
	// Ours and Theirs describe what each side did to it, for a reader.
	Ours   string
	Theirs string
}

// Describe renders the disagreement as one line.
func (c Conflict) Describe() string {
	return fmt.Sprintf("%s: this side %s, the other %s", c.Object, c.Ours, c.Theirs)
}

// Merge is the result of reconciling two schemas that share an ancestor.
type Merge struct {
	// Schema is the merged result: the ancestor with both sides' independent
	// work applied. Nil when there are conflicts, because there is no single
	// result to name until somebody resolves them.
	Schema *schema.Schema

	Conflicts []Conflict
}

// Clean reports a merge with nothing to resolve by hand.
func (m Merge) Clean() bool { return len(m.Conflicts) == 0 }

// ThreeWay merges two schemas that both descend from base.
//
// Written against the schemas rather than against the two lists of changes, and
// that distinction is the whole point. A change list says "add column channel";
// reconciling two such lists tells you what to run but never what you end up
// with. The result is needed as a schema: it is the migration's declared
// target, and the shadow proof (D-021) checks the rehearsal against it. Merging
// the models produces that target directly, and the statements fall out of
// diffing our current schema against it.
//
// The rule at every object is the ordinary three-way one. If only one side
// touched an object, take that side. If both made the same change, take it
// once. If both changed it differently, that is a conflict, and no rule
// recovers which intention was meant.
func ThreeWay(base, ours, theirs *schema.Schema) Merge {
	base, ours, theirs = normalized(base), normalized(ours), normalized(theirs)

	var conflicts []Conflict
	namespaces, conflicts := mergeNamespaces(base, ours, theirs, conflicts)
	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			return conflicts[i].Object < conflicts[j].Object
		})
		return Merge{Conflicts: conflicts}
	}
	return Merge{Schema: normalized(&schema.Schema{Namespaces: namespaces})}
}

// present is one side's view of an object: whether it is there, and what it is.
type present[T any] struct {
	ok  bool
	val T
}

func index[T any](items []T, key func(T) string) map[string]T {
	out := make(map[string]T, len(items))
	for _, it := range items {
		out[key(it)] = it
	}
	return out
}

// keys returns every name across the three sides, in a stable order.
func keys[T any](sides ...map[string]T) []string {
	seen := map[string]bool{}
	var out []string
	for _, side := range sides {
		for k := range side {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func look[T any](m map[string]T, k string) present[T] {
	v, ok := m[k]
	return present[T]{ok: ok, val: v}
}

// describe says what one side did to an object, in the words a reader needs.
func describe[T any](base, side present[T]) string {
	switch {
	case !base.ok && side.ok:
		return "added it"
	case base.ok && !side.ok:
		return "dropped it"
	case !side.ok:
		return "never had it"
	default:
		return "changed it"
	}
}

// resolve applies the three-way rule to one leaf object.
//
// Returns the winning value, whether it belongs in the result at all, and
// whether the two sides disagree.
func resolve[T any](base, ours, theirs present[T]) (val T, keep, conflict bool) {
	ourMove := base.ok != ours.ok || (base.ok && ours.ok && !reflect.DeepEqual(base.val, ours.val))
	theirMove := base.ok != theirs.ok || (base.ok && theirs.ok && !reflect.DeepEqual(base.val, theirs.val))

	switch {
	case !ourMove && !theirMove:
		return base.val, base.ok, false
	case ourMove && !theirMove:
		return ours.val, ours.ok, false
	case theirMove && !ourMove:
		return theirs.val, theirs.ok, false
	case ours.ok == theirs.ok && reflect.DeepEqual(ours.val, theirs.val):
		// Both did the same thing. Agreement, arrived at separately.
		return ours.val, ours.ok, false
	default:
		return val, false, true
	}
}

func mergeNamespaces(base, ours, theirs *schema.Schema, conflicts []Conflict) ([]schema.Namespace, []Conflict) {
	name := func(n schema.Namespace) string { return n.Name }
	b, o, t := index(base.Namespaces, name), index(ours.Namespaces, name), index(theirs.Namespaces, name)

	var out []schema.Namespace
	for _, k := range keys(b, o, t) {
		bn, on, tn := look(b, k), look(o, k), look(t, k)

		// Whether the namespace exists at all is settled before its contents.
		// One side dropping a schema while the other adds tables to it is a
		// disagreement about the schema itself, and resolving it per-table
		// would silently resurrect the dropped one.
		if !on.ok || !tn.ok {
			val, keep, conflict := resolve(bn, on, tn)
			if conflict {
				conflicts = append(conflicts, Conflict{Object: k,
					Ours: describe(bn, on), Theirs: describe(bn, tn)})
				continue
			}
			if keep {
				out = append(out, val)
			}
			continue
		}

		merged := schema.Namespace{Name: k}
		var resolved bool
		merged.Comment, resolved = resolveString(bn.val.Comment, on.val.Comment, tn.val.Comment)
		if !resolved {
			conflicts = append(conflicts, Conflict{Object: k,
				Ours:   "set its comment to " + on.val.Comment,
				Theirs: "set it to " + tn.val.Comment})
		}

		merged.Tables, conflicts = mergeTables(k, bn, on, tn, conflicts)
		merged.Enums, conflicts = mergeLeaves(k, bn.val.Enums, on.val.Enums, tn.val.Enums,
			bn.ok, true, true, func(e schema.Enum) string { return e.Name }, conflicts)
		merged.Sequences, conflicts = mergeLeaves(k, bn.val.Sequences, on.val.Sequences, tn.val.Sequences,
			bn.ok, true, true, func(s schema.Sequence) string { return s.Name }, conflicts)
		out = append(out, merged)
	}
	return out, conflicts
}

func mergeTables(ns string, bn, on, tn present[schema.Namespace], conflicts []Conflict) ([]schema.Table, []Conflict) {
	name := func(t schema.Table) string { return t.Name }
	var bt, ot, tt map[string]schema.Table
	if bn.ok {
		bt = index(bn.val.Tables, name)
	}
	if on.ok {
		ot = index(on.val.Tables, name)
	}
	if tn.ok {
		tt = index(tn.val.Tables, name)
	}

	var out []schema.Table
	for _, k := range keys(bt, ot, tt) {
		bb, oo, ttab := look(bt, k), look(ot, k), look(tt, k)
		qualified := ns + "." + k

		// A table both sides still have is merged column by column, so two
		// people adding different columns to the same table is not a conflict.
		if bb.ok && oo.ok && ttab.ok {
			merged := schema.Table{Name: k}
			var ok bool
			merged.Comment, ok = resolveString(bb.val.Comment, oo.val.Comment, ttab.val.Comment)
			if !ok {
				conflicts = append(conflicts, Conflict{Object: qualified,
					Ours: "commented it", Theirs: "commented it differently"})
			}
			merged.PartitionKey, ok = resolveString(bb.val.PartitionKey, oo.val.PartitionKey, ttab.val.PartitionKey)
			if !ok {
				conflicts = append(conflicts, Conflict{Object: qualified,
					Ours: "partitioned it", Theirs: "partitioned it differently"})
			}
			merged.Partitioned = merged.PartitionKey != "" || bb.val.Partitioned

			merged.Columns, conflicts = mergeLeaves(qualified,
				bb.val.Columns, oo.val.Columns, ttab.val.Columns, true, true, true,
				func(c schema.Column) string { return c.Name }, conflicts)
			merged.Constraints, conflicts = mergeLeaves(qualified,
				bb.val.Constraints, oo.val.Constraints, ttab.val.Constraints, true, true, true,
				func(c schema.Constraint) string { return c.Name }, conflicts)
			merged.Indexes, conflicts = mergeLeaves(qualified,
				bb.val.Indexes, oo.val.Indexes, ttab.val.Indexes, true, true, true,
				func(i schema.Index) string { return i.Name }, conflicts)
			out = append(out, merged)
			continue
		}

		val, keep, conflict := resolve(bb, oo, ttab)
		if conflict {
			conflicts = append(conflicts, Conflict{Object: qualified,
				Ours: describe(bb, oo), Theirs: describe(bb, ttab)})
			continue
		}
		if keep {
			out = append(out, val)
		}
	}
	return out, conflicts
}

// mergeLeaves merges a keyed collection whose members are compared whole.
//
// The container's own presence is already settled by the caller; hasBase and
// friends say whether each side had it, so that a collection missing because
// its table is new is not read as every member having been dropped.
func mergeLeaves[T any](container string, base, ours, theirs []T,
	hasBase, hasOurs, hasTheirs bool, key func(T) string, conflicts []Conflict) ([]T, []Conflict) {
	var b, o, t map[string]T
	if hasBase {
		b = index(base, key)
	}
	if hasOurs {
		o = index(ours, key)
	}
	if hasTheirs {
		t = index(theirs, key)
	}

	var out []T
	for _, k := range keys(b, o, t) {
		val, keep, conflict := resolve(look(b, k), look(o, k), look(t, k))
		if conflict {
			conflicts = append(conflicts, Conflict{
				Object: container + "." + k,
				Ours:   describeLeaf(look(b, k), look(o, k)),
				Theirs: describeLeaf(look(b, k), look(t, k)),
			})
			continue
		}
		if keep {
			out = append(out, val)
		}
	}
	return out, conflicts
}

// describeLeaf says what one side did, and — whenever the object survives on
// that side — what it became.
//
// The version matters more than the verb. "this side added it, the other added
// it" is a true sentence that tells a reader nothing: the whole question is
// which of the two versions to keep, so both have to be on the line.
func describeLeaf[T any](base, side present[T]) string {
	d := describe(base, side)
	if !side.ok {
		return d
	}
	if d == "changed it" {
		return d + " to " + render(side.val)
	}
	return d + " as " + render(side.val)
}

// render summarises a value for a conflict line: the fields that distinguish it
// from its other version, without printing the whole struct.
func render[T any](v T) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return fmt.Sprint(v)
	}
	var parts []string
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		if !f.IsExported() || f.Name == "Name" || f.Name == "Definition" {
			continue
		}
		fv := rv.Field(i)
		if fv.IsZero() {
			continue
		}
		parts = append(parts, strings.ToLower(f.Name)+" "+fmt.Sprint(fv.Interface()))
	}
	if len(parts) == 0 {
		return "nothing in particular"
	}
	return strings.Join(parts, ", ")
}

// resolveString applies the three-way rule to a scalar field, reporting whether
// it resolved.
func resolveString(base, ours, theirs string) (string, bool) {
	switch {
	case ours == theirs:
		return ours, true
	case ours == base:
		return theirs, true
	case theirs == base:
		return ours, true
	default:
		return "", false
	}
}
