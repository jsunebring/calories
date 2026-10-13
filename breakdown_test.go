package main

import (
	"encoding/json"
	"testing"
)

func TestBreakdownExplainsEveryTotal(t *testing.T) {
	var keys map[string]any
	b, _ := json.Marshal(newTotals())
	json.Unmarshal(b, &keys)
	got := breakdown(nil, 1)
	for k := range keys {
		if k == "items" || k == "known" {
			continue
		}
		if _, ok := got[k]; !ok {
			t.Errorf("the summary shows %q but nothing says which foods it came from", k)
		}
	}
}

func TestBreakdownSumsPerFoodAndKeepsUnknownsLast(t *testing.T) {
	sugar := func(v float64) *float64 { return &v }
	milk := Entry{Food: Food{ID: 1, Name: "Mjölk"}, Totals: Nutrients{Kcal: 60, Sugar: sugar(5)}}
	bread := Entry{Food: Food{ID: 2, Name: "Bröd"}, Totals: Nutrients{Kcal: 200}}
	cola := Entry{Food: Food{ID: 3, Name: "Cola"}, Totals: Nutrients{Kcal: 140, Sugar: sugar(35)}}
	got := breakdown([]Entry{milk, bread, cola, milk}, 0.5)["sugar"]
	if len(got) != 3 || got[0].Name != "Cola" || *got[0].Amount != 17.5 || got[1].Name != "Mjölk" || *got[1].Amount != 5 {
		t.Fatalf("largest first, the same food summed, scaled like the total: %+v", got)
	}
	if got[2].Name != "Bröd" || got[2].Amount != nil {
		t.Fatalf("a food without a sugar figure is listed as unknown, last: %+v", got[2])
	}
}
