package main

import "testing"

// Existing normal behavior must be preserved by a repair.
func TestQuoteNormalOrder(t *testing.T) {
	q, err := CalculateQuote([]Item{{UnitPrice: 150, Quantity: 2}, {UnitPrice: 300, Quantity: 1}})
	if err != nil || q.Total != 600 || q.Average != 200 {
		t.Fatalf("normal quote = %+v, %v", q, err)
	}
}

func TestQuoteRejectsInvalidItems(t *testing.T) {
	for _, item := range []Item{{-1, 1}, {1, 0}, {1000001, 1}, {1, 1001}} {
		if _, err := CalculateQuote([]Item{item}); err == nil {
			t.Fatalf("accepted invalid item: %+v", item)
		}
	}
}

func TestQuoteEmptyOrder(t *testing.T) {
	q, err := CalculateQuote(nil)
	if err != nil || q != (Quote{}) {
		t.Fatalf("empty quote = %+v, %v; want zero quote and no error", q, err)
	}
}

func TestQuoteEmptyNonNilOrder(t *testing.T) {
	q, err := CalculateQuote([]Item{})
	if err != nil || q != (Quote{}) {
		t.Fatalf("empty quote = %+v, %v; want zero quote and no error", q, err)
	}
}
