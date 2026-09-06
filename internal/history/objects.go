// Package history turns the record of observed schemas into something a person
// can read.
//
// It computes an *object-level* delta: which tables, enums and sequences were
// added, removed or changed between two schema versions. That is deliberately
// not the semantic diff engine — it will not say "a column became nullable", only
// that a table is not what it was. It needs no diff engine to exist, and it
// answers the question a history view actually asks: what did this change touch.
package history

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// Kind is what happened to an object.
type Kind string

const (
	Added    Kind = "added"
	Removed  Kind = "removed"
	Modified Kind = "modified"
)

// ObjectChange is one object that differs between two schema versions.
type ObjectChange struct {
	Kind Kind   `json:"kind"`
	Type string `json:"type"` // table, enum, sequence, namespace
	Name string `json:"name"` // qualified, e.g. public.orders
}

// Summary counts a set of changes.
type Summary struct {
	Added    int `json:"added"`
	Removed  int `json:"removed"`
	Modified int `json:"modified"`
}

// Total reports how many objects differ.
func (s Summary) Total() int { return s.Added + s.Removed + s.Modified }

// Describe renders the summary the way a changelog line would read.
func (s Summary) Describe() string {
	if s.Total() == 0 {
		return "no object-level changes"
	}
	parts := make([]string, 0, 3)
	for _, p := range []struct {
		n     int
		label string
	}{{s.Added, "added"}, {s.Modified, "modified"}, {s.Removed, "removed"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.label))
		}
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

// key identifies an object across two versions of a schema.
type key struct{ typ, name string }

// ObjectsChanged reports which objects differ between two schemas.
//
// Both sides are normalized first, so a difference here is a real difference and
// never an artifact of how either schema was read. Objects are compared by their
// canonical serialization: equal bytes mean equal object.
func ObjectsChanged(before, after *schema.Schema) []ObjectChange {
	b, a := index(before), index(after)

	var out []ObjectChange
	for k, beforeForm := range b {
		afterForm, present := a[k]
		switch {
		case !present:
			out = append(out, ObjectChange{Kind: Removed, Type: k.typ, Name: k.name})
		case beforeForm != afterForm:
			out = append(out, ObjectChange{Kind: Modified, Type: k.typ, Name: k.name})
		}
	}
	for k := range a {
		if _, present := b[k]; !present {
			out = append(out, ObjectChange{Kind: Added, Type: k.typ, Name: k.name})
		}
	}

	// Stable order: by name, then by kind, so the same pair of schemas always
	// produces the same list.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Count summarises a change list.
func Count(changes []ObjectChange) Summary {
	var s Summary
	for _, c := range changes {
		switch c.Kind {
		case Added:
			s.Added++
		case Removed:
			s.Removed++
		case Modified:
			s.Modified++
		}
	}
	return s
}

// index reduces a schema to a map from object identity to its canonical form.
func index(s *schema.Schema) map[key]string {
	out := map[key]string{}
	if s == nil {
		return out
	}
	// Work on a normalized copy so the comparison cannot be perturbed by
	// collection ordering.
	c := *s
	normalized := &c
	schema.Normalize(normalized)

	for _, ns := range normalized.Namespaces {
		out[key{"namespace", ns.Name}] = ns.Comment
		for _, t := range ns.Tables {
			out[key{"table", ns.Name + "." + t.Name}] = form(t)
		}
		for _, e := range ns.Enums {
			out[key{"enum", ns.Name + "." + e.Name}] = form(e)
		}
		for _, sq := range ns.Sequences {
			if sq.Owned() {
				// An owned sequence belongs to its column; reporting it
				// separately would show two changes for one edit.
				continue
			}
			out[key{"sequence", ns.Name + "." + sq.Name}] = form(sq)
		}
	}
	return out
}

func form(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Every model type is plain data; there is no input for which this can
		// fail. Returning the error text still compares unequal, which is the
		// safe direction.
		return "unserializable:" + err.Error()
	}
	return string(b)
}
