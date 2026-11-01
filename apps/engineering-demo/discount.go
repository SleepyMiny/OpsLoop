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
func CalculateQuote(items []Item) (Quote, error) {
	const maxInt64 = int64(1<<63 - 1)
	var total, count int64
	for _, item := range items {
		if item.UnitPrice < 0 || item.UnitPrice > 1000000 || item.Quantity < 1 || item.Quantity > 1000 {
			return Quote{}, errors.New("invalid item")
		}
		cost := item.UnitPrice * item.Quantity
		if total > maxInt64-cost || count > maxInt64-item.Quantity {
			return Quote{}, errors.New("quote total overflow")
		}
		total += cost
		count += item.Quantity
	}
	if count == 0 {
		return Quote{}, nil
	}
	return Quote{Total: total, Average: total / count}, nil
}
