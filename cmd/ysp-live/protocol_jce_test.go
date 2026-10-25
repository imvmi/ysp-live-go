package main

import (
	"strings"
	"testing"
)

func TestJCERejectsUnhashableMapKey(t *testing.T) {
	w := &jceWriter{}
	w.head(8, 0)
	w.integer(1, 0)
	w.bytes([]byte("key"), 0)
	w.str("value", 1)
	r := &jceReader{d: w.b}
	if _, err := r.fieldValue(); err == nil || !strings.Contains(err.Error(), "map key") {
		t.Fatalf("expected map key error, got %v", err)
	}
}

func TestJCELimitsNestingAndAllocation(t *testing.T) {
	w := &jceWriter{}
	for i := 0; i < 100; i++ {
		w.head(9, 0)
		w.integer(1, 0)
	}
	w.str("leaf", 0)
	r := &jceReader{d: w.b}
	if _, err := r.fieldValue(); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("expected depth error, got %v", err)
	}
	w = &jceWriter{}
	w.head(9, 0)
	w.integer(1_000_000, 0)
	r = &jceReader{d: w.b}
	if _, err := r.fieldValue(); err == nil {
		t.Fatal("accepted length larger than payload")
	}
}

func FuzzJCEReader(f *testing.F) {
	f.Add([]byte{0x08, 0, 1, 0x0d, 0, 0, 1, 'k', 0x0c})
	f.Add([]byte{0x0a, 0x06, 3, 'u', 'r', 'l', 0x0b})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			t.Skip()
		}
		r := &jceReader{d: data}
		_, _ = r.structure()
	})
}
