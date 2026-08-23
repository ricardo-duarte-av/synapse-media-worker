package main

import (
	"strings"
	"testing"
)

// Synapse uses random_string(24) over string.ascii_letters. Matching it keeps
// our rows indistinguishable from Synapse's own.
func TestFilesystemIDMatchesSynapseShape(t *testing.T) {
	for range 200 {
		id := newFilesystemID()
		if len(id) != 24 {
			t.Fatalf("length = %d, want 24: %q", len(id), id)
		}
		for _, r := range id {
			if !strings.ContainsRune(fsidAlphabet, r) {
				t.Fatalf("character %q is outside Python's string.ascii_letters: %q", r, id)
			}
		}
	}
}

// Digits are the easy mistake here: Python's ascii_letters has none.
func TestFilesystemIDHasNoDigits(t *testing.T) {
	var seen strings.Builder
	for range 500 {
		seen.WriteString(newFilesystemID())
	}
	if strings.ContainsAny(seen.String(), "0123456789") {
		t.Error("generated a digit; Synapse's alphabet is letters only")
	}
}

func TestFilesystemIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := newFilesystemID()
		if seen[id] {
			t.Fatalf("duplicate filesystem_id %q", id)
		}
		seen[id] = true
	}
}

// The ID is sharded as id[0:2]/id[2:4]/id[4:], so it must survive path
// validation and be long enough to shard.
func TestFilesystemIDIsAValidPathComponent(t *testing.T) {
	p := NewMediaPaths(testBase)
	for range 50 {
		id := newFilesystemID()
		if err := validatePathComponent(id); err != nil {
			t.Fatalf("%q rejected by path validation: %v", id, err)
		}
		if _, err := p.RemoteMedia("example.com", id); err != nil {
			t.Fatalf("%q could not be turned into a path: %v", id, err)
		}
	}
}
