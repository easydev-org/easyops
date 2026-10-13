package opssvc

import "testing"

// TestWritablePolicy: A1 — only a NON-EMPTY creator equal to the owner may be
// written; an unowned object (creator "") is NOT writable by anyone.
func TestWritablePolicy(t *testing.T) {
	cases := []struct {
		creator, owner string
		want           bool
	}{
		{"alice", "alice", true},
		{"alice", "bob", false},
		{"alice", "", false}, // no caller tenant
		{"", "alice", false}, // unowned: never writable (A1)
		{"", "", false},      // unowned + no owner
	}
	for _, c := range cases {
		if got := writable(c.creator, c.owner); got != c.want {
			t.Fatalf("writable(%q,%q)=%v want %v", c.creator, c.owner, got, c.want)
		}
	}
}

// TestOwnedReadFilterUnchanged: the READ filter still treats unowned as visible.
func TestOwnedReadFilterUnchanged(t *testing.T) {
	if !owned("", "alice") || !owned("alice", "alice") || owned("alice", "bob") {
		t.Fatal("owned() read-filter semantics changed")
	}
}
