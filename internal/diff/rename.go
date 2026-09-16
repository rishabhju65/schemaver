package diff

import (
	"fmt"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// Confidence is how strongly a rename is suggested.
type Confidence string

const (
	// Likely: exactly one column was dropped and one added in the table, and
	// their types match.
	Likely Confidence = "likely"
	// Possible: several were dropped and added, and these two happen to share a
	// type. Any pairing among them is a guess.
	Possible Confidence = "possible"
)

// RenameCandidate is a drop-and-add pair that may be one thing renamed.
//
// It is never applied. From the schema alone a rename and a drop-plus-add are
// indistinguishable, and the two are not remotely equivalent: renaming keeps the
// data, dropping destroys it. A tool that guessed would silently discard a
// column of production data whenever it guessed wrong, so the only defensible
// behaviour is to surface the ambiguity and make a human resolve it.
//
// The changes it refers to remain in the change list as an independent drop and
// add. Confirming a rename replaces them; declining leaves them as they are.
type RenameCandidate struct {
	Namespace string `json:"namespace"`
	// Table is empty where the candidate is a table rename, matching Rename.
	Table      string     `json:"table"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	Type       string     `json:"type"`
	Confidence Confidence `json:"confidence"`

	// DropID and AddID are the changes this pair would replace.
	DropID string `json:"drop_id"`
	AddID  string `json:"add_id"`

	Question string `json:"question"`
}

// Rename is a candidate somebody has confirmed: this thing became that one.
//
// Deliberately just the four names. It is a statement about intent, not about
// the shape of what was renamed, so it stays true across a regeneration that
// changed a type or a default — and ComputeWith re-checks against the schemas
// anyway before acting on it.
//
// An empty Table means the table itself is what was renamed, and From and To
// are table names. No column rename can have an empty table, so the two kinds
// never collide — which is what lets one stored answer and one lookup key serve
// both without a second column to say which is which.
type Rename struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// OfTable reports whether the subject is a table rather than a column.
func (r Rename) OfTable() bool { return r.Table == "" }

// Subject names what the rename concerns, for a message about it.
func (r Rename) Subject() string {
	if r.OfTable() {
		return r.Namespace + "." + r.From
	}
	return r.Namespace + "." + r.Table + "." + r.From
}

// findRenames proposes renames from the drop and add columns in a change set.
func findRenames(changes []Change) []RenameCandidate {
	type tableKey struct{ ns, table string }
	drops := map[tableKey][]Change{}
	adds := map[tableKey][]Change{}

	for _, c := range changes {
		k := tableKey{c.Namespace, c.Table}
		switch c.Kind {
		case DropColumn:
			drops[k] = append(drops[k], c)
		case AddColumn:
			adds[k] = append(adds[k], c)
		}
	}

	var out []RenameCandidate
	for k, dropped := range drops {
		added := adds[k]
		if len(added) == 0 {
			continue
		}
		// One out, one in: the clearest case there is, though still only a
		// suggestion.
		certainty := Possible
		if len(dropped) == 1 && len(added) == 1 {
			certainty = Likely
		}

		used := map[int]bool{}
		for _, d := range dropped {
			for i, a := range added {
				if used[i] || !sameColumnType(d, a) {
					continue
				}
				used[i] = true
				out = append(out, RenameCandidate{
					Namespace: k.ns, Table: k.table,
					From: d.Object, To: a.Object,
					Type: columnTypeOf(a), Confidence: certainty,
					DropID: d.ID, AddID: a.ID,
					Question: fmt.Sprintf(
						"Is %s.%s.%s being renamed to %s, or dropped and replaced? "+
							"Renaming keeps the data; dropping discards it.",
						k.ns, k.table, d.Object, a.Object),
				})
				break
			}
		}
	}
	return out
}

// sameColumnType reports whether a dropped and an added column share a type.
//
// Type equality is weak evidence on its own — two unrelated text columns match —
// which is exactly why the result is a question rather than a decision.
func sameColumnType(drop, add Change) bool {
	dt, at := columnTypeOf(drop), columnTypeOf(add)
	return dt != "" && dt == at
}

// columnTypeOf recovers the type recorded in a column change's summary.
func columnTypeOf(c Change) string { return c.typeHint }

// OfTable reports whether the subject is a table rather than a column.
func (c RenameCandidate) OfTable() bool { return c.Table == "" }

// Subject names what the candidate concerns, for a message about it.
func (c RenameCandidate) Subject() string {
	if c.OfTable() {
		return c.Namespace + "." + c.From
	}
	return c.Namespace + "." + c.Table + "." + c.From
}

// tableSignature is the set of a table's columns as "name:type", which is the
// evidence a dropped and a created table are the same table under a new name.
func tableSignature(t schema.Table) map[string]bool {
	out := map[string]bool{}
	for _, c := range t.Columns {
		out[c.Name+":"+c.Type] = true
	}
	return out
}

// overlap counts how many entries two signatures share.
func overlap(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

// findTableRenames proposes table renames from the dropped and created tables
// in a change set.
//
// The evidence is the columns, because the name is the only thing that changed
// and so the only thing that cannot be compared. Two tables that share most of
// their columns are worth asking about; two that share none are a drop and a
// create that happen to have occurred together, and asking about those would
// put a question in front of somebody for every unrelated pair of changes in a
// schema.
//
// Everything the column case argues applies here with more at stake. A column
// wrongly dropped loses one column; a table wrongly dropped loses all of them.
func findTableRenames(changes []Change, a, b *schema.Schema) []RenameCandidate {
	before, after := indexNamespaces(a), indexNamespaces(b)

	dropped := map[string][]Change{}
	created := map[string][]Change{}
	for _, c := range changes {
		switch c.Kind {
		case DropTable:
			dropped[c.Namespace] = append(dropped[c.Namespace], c)
		case CreateTable:
			created[c.Namespace] = append(created[c.Namespace], c)
		}
	}

	var out []RenameCandidate
	for ns, gone := range dropped {
		arrived := created[ns]
		if len(arrived) == 0 {
			continue
		}
		oldNS, newNS := before[ns], after[ns]
		if oldNS == nil || newNS == nil {
			continue
		}
		oldTables, newTables := indexTables(oldNS), indexTables(newNS)

		// One out and one in is the clearest case there is, exactly as it is
		// for columns, and still only a suggestion.
		certainty := Possible
		if len(gone) == 1 && len(arrived) == 1 {
			certainty = Likely
		}

		used := map[string]bool{}
		for _, drop := range gone {
			old, ok := oldTables[drop.Table]
			if !ok {
				continue
			}
			oldSig := tableSignature(old)
			var best Change
			bestScore := 0
			for _, add := range arrived {
				if used[add.Table] {
					continue
				}
				next, ok := newTables[add.Table]
				if !ok {
					continue
				}
				newSig := tableSignature(next)
				shared := overlap(oldSig, newSig)
				smaller := len(oldSig)
				if len(newSig) < smaller {
					smaller = len(newSig)
				}
				// Half the smaller table, and never nothing. Below that the
				// two are not plausibly the same table, and the question would
				// be noise standing between somebody and a migration.
				if shared == 0 || shared*2 < smaller {
					continue
				}
				if shared > bestScore {
					best, bestScore = add, shared
				}
			}
			if bestScore == 0 {
				continue
			}
			used[best.Table] = true
			out = append(out, RenameCandidate{
				Namespace: ns, Table: "",
				From: drop.Table, To: best.Table, Confidence: certainty,
				DropID: drop.ID, AddID: best.ID,
				Question: fmt.Sprintf(
					"Is table %s.%s being renamed to %s, or dropped and replaced? "+
						"Renaming keeps every row; dropping discards them all.",
					ns, drop.Table, best.Table),
			})
		}
	}
	return out
}

func indexTables(n *schema.Namespace) map[string]schema.Table {
	out := map[string]schema.Table{}
	for _, t := range n.Tables {
		out[t.Name] = t
	}
	return out
}
