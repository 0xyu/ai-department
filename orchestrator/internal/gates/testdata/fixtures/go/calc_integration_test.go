//go:build integration

package goproj

import "testing"

func TestIntegrationAdd(t *testing.T) {
	if Add(Add(1, 2), 3) != 6 {
		t.Fatal("chained Add")
	}
}
