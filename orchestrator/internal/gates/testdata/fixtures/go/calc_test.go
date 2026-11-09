package goproj

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatal("Add(2, 3) != 5")
	}
}

// TestKnownBroken fails at baseline on purpose (a pre-existing failure).
func TestKnownBroken(t *testing.T) {
	if Sub(5, 3) != 3 {
		t.Fatal("pre-existing failure: Sub(5, 3) != 3")
	}
}
