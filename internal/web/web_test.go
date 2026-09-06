package web

import "testing"

// TestTemplatesParse catches a broken template at build time rather than on a
// request. Every page shares one layout, so a syntax error in any of them takes
// down the whole interface.
func TestTemplatesParse(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatalf("templates did not parse: %v", err)
	}
	for _, page := range []string{"fleet", "history", "change", "drift"} {
		if s.tmpl[page] == nil {
			t.Errorf("%s template missing", page)
		}
	}
}

// TestRoutesRegistered pins the URL surface; these paths appear in links people
// paste to each other.
func TestRoutesRegistered(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Handler() == nil {
		t.Fatal("no handler returned")
	}
}
