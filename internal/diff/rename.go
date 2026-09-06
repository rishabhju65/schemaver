package diff

import "fmt"

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

// RenameCandidate is a drop-and-add pair that may be one column renamed.
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
	Namespace  string     `json:"namespace"`
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
