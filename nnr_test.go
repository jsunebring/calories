package main

import (
	"math"
	"testing"
	"time"
)

// The equations must reproduce NNR 2023's own published resting energy for
// every adult reference person (Appendix 4, table 5; MJ/day to one decimal)
// and the PAL 1.6 intake in Livsmedelsverket's table 1, within 2%. Measured
// 2026-10-07: women over 70 come out 1.3% under the table (8.09 vs 8.2 MJ),
// every other row within 1%.
func TestHenryReproducesNNRReferenceValues(t *testing.T) {
	for _, c := range []struct {
		sex      string
		age      int
		w, h     float64
		bee, mid float64
	}{
		{"f", 20, 64.2, 1.67, 5.9, 9.4}, {"f", 40, 64.1, 1.67, 5.7, 9.0}, {"f", 60, 62.5, 1.65, 5.2, 8.3}, {"f", 75, 60.6, 1.62, 5.1, 8.2},
		{"m", 20, 75.2, 1.81, 7.4, 11.8}, {"m", 40, 74.8, 1.80, 7.1, 11.3}, {"m", 60, 73.0, 1.78, 6.4, 10.3}, {"m", 75, 70.6, 1.75, 6.3, 10.1},
	} {
		bee, _, ok := henryBEE(c.sex, c.age, c.w, c.h)
		if !ok || math.Abs(bee/c.bee-1) > 0.02 || math.Abs(bee*1.6/c.mid-1) > 0.02 {
			t.Errorf("%s %d: BEE %.3f (want %.1f), PAL1.6 %.2f (want %.1f)", c.sex, c.age, bee, c.bee, bee*1.6, c.mid)
		}
	}
}

func TestRecommendationNeedsAWholeAdultProfile(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	y, h, w := 1985, 180.0, 80.0
	if recommend(Profile{Sex: "m", BirthYear: &y, HeightCm: &h}, now) != nil {
		t.Fatal("no weight, no recommendation")
	}
	teen := 2012
	if recommend(Profile{Sex: "m", BirthYear: &teen, HeightCm: &h, WeightKg: &w}, now) != nil {
		t.Fatal("NNR adult values must not be applied to a 14-year-old")
	}
	r := recommend(Profile{Sex: "m", BirthYear: &y, HeightCm: &h, WeightKg: &w}, now)
	if r == nil || r.Age != 41 || *r.FiberG.Min != 35 || *r.SaltG.Max != 5.75 {
		t.Fatalf("%+v", r)
	}
}

func TestSummaryAveragesLoggedDaysAndCountsCoverage(t *testing.T) {
	store := testStore(t)
	srv := &Server{store: store}
	fiber := 10.0
	bread := &Food{Source: "manual", Name: "Bröd", Nutrients: Nutrients{Kcal: 250, Fiber: &fiber}}
	cola := &Food{Source: "manual", Name: "Läsk", Nutrients: Nutrients{Kcal: 40}}
	store.SaveFood(bread, "")
	store.SaveFood(cola, "")
	store.AddEntry(testUID, "2026-10-01", "lunch", bread.ID, 100, "") // 250 kcal
	store.AddEntry(testUID, "2026-10-07", "lunch", bread.ID, 200, "") // 500 kcal, 20 g fibre
	store.AddEntry(testUID, "2026-10-07", "lunch", cola.ID, 500, "")  // 200 kcal, fibre unknown

	_, body := apiReq(t, srv, "GET", "/api/summary?day=2026-10-07", "")
	today := body["today"].(map[string]any)
	if today["kcal"] != 700.0 || today["items"] != 2.0 || today["known"].(map[string]any)["fiber"] != 1.0 {
		t.Fatalf("today: %v", today)
	}
	week := body["week"].(map[string]any)
	if week["days"] != 2.0 || week["avg"].(map[string]any)["kcal"] != 475.0 {
		t.Fatalf("five empty days must not count as eating nothing: %v", week)
	}
}
