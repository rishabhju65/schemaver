package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Version is the content-addressed identity of a schema: the digest of its
// canonical form. Two schemas share a Version if and only if they are logically
// identical.
type Version string

// Short returns the first 12 characters, for display.
func (v Version) Short() string {
	if len(v) <= 12 {
		return string(v)
	}
	return string(v[:12])
}

// Fingerprint normalizes s and returns its content-addressed version.
//
// It normalizes as part of computing the digest rather than trusting the caller
// to have done so: a fingerprint taken over a non-canonical schema would be
// silently wrong in exactly the way that is hardest to debug.
//
// Rendered Definition fields are stripped before hashing. They are the engine's
// own formatting of an object, which can change between engine versions without
// the schema changing; including them would make an engine upgrade look like a
// schema change. They remain in Canonical output, because the DDL renderer needs
// them to reproduce constructs it cannot rebuild structurally.
func Fingerprint(s *Schema) (Version, error) {
	c := clone(s)
	stripDefinitions(c)
	Normalize(c)

	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("serialize schema for fingerprint: %w", err)
	}
	sum := sha256.Sum256(b)
	return Version(hex.EncodeToString(sum[:])), nil
}

// Canonical returns the normalized JSON serialization of s. It is what to store,
// diff, or show a human who asks what the tool actually thinks the schema is.
//
// It is deliberately not identical to the bytes hashed by Fingerprint: this
// retains the rendered Definition fields, which the fingerprint strips.
func Canonical(s *Schema) ([]byte, error) {
	c := clone(s)
	Normalize(c)
	return json.MarshalIndent(c, "", "  ")
}

// clone deep-copies via the same serialization the fingerprint uses, so that
// Fingerprint and Canonical never mutate the caller's schema.
func clone(s *Schema) *Schema {
	b, err := json.Marshal(s)
	if err != nil {
		// Schema contains only strings, numbers, bools and slices of the same;
		// there is no input for which this can fail.
		panic(fmt.Sprintf("schema: unmarshalable model: %v", err))
	}
	var out Schema
	if err := json.Unmarshal(b, &out); err != nil {
		panic(fmt.Sprintf("schema: model does not round-trip: %v", err))
	}
	return &out
}

// stripDefinitions clears every engine-rendered Definition so it cannot reach the
// digest.
func stripDefinitions(s *Schema) {
	for i := range s.Namespaces {
		for j := range s.Namespaces[i].Tables {
			t := &s.Namespaces[i].Tables[j]
			for k := range t.Constraints {
				t.Constraints[k].Definition = ""
			}
			for k := range t.Indexes {
				t.Indexes[k].Definition = ""
			}
		}
	}
}
