package main

import "errors"

type Item struct {
	UnitPrice int64 `json:"unitPrice"`
	Quantity  int64 `json:"quantity"`
}

type Quote struct {
	Total   int64 `json:"total"`
	Average int64 `json:"average"`
}

// CalculateQuote prices an order in integer cents. Empty orders are valid.
// This dedicated Demo B application deliberately starts with an unfixed defect;
// the repair must come from the isolated Codex run, not the demo coordinator.
func CalculateQuote(items []Item) (Quote, error) {
	var total, count int64
	for _, item := range items {
		if item.UnitPrice < 0 || item.UnitPrice > 1000000 || item.Quantity < 1 || item.Quantity > 1000 {
			return Quote{}, errors.New("invalid item")
		}
		total += item.UnitPrice * item.Quantity
		count += item.Quantity
	}
	return Quote{Total: total, Average: total / count}, nil
}
