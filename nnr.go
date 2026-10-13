package main

import "time"

// Reference values from the Nordic Nutrition Recommendations 2023, Sweden's
// official ones (Livsmedelsverket, "Referensvärden för energi och
// näringsämnen"). They are for adults and are meant as averages over at
// least a week, which is why the summary offers a weekly view.

type Profile struct {
	Sex       string   `json:"sex"` // "f" | "m" | ""
	BirthYear *int     `json:"birthYear"`
	HeightCm  *float64 `json:"heightCm"`
	WeightKg  *float64 `json:"weightKg"`
	PAL       *float64 `json:"pal"`      // 1.4 low, 1.6 medium, 1.8 active
	KcalGoal  *float64 `json:"kcalGoal"` // the user's own number; the recommendation only informs it
}

type Range struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

func rng(min, max float64) Range { return Range{&min, &max} }
func atMost(v float64) Range     { return Range{Max: &v} }
func atLeast(v float64) Range    { return Range{Min: &v} }

type Recommendation struct {
	Age       int     `json:"age"`
	AgeGroup  string  `json:"ageGroup"`
	RestKcal  float64 `json:"restKcal"`
	EnergyLow float64 `json:"energyLow"` // PAL 1.4
	EnergyMid float64 `json:"energyMid"` // PAL 1.6
	EnergyHi  float64 `json:"energyHigh"`
	EnergyPAL float64 `json:"energyAtPal"` // at the user's own activity level, if set
	// Shares of energy, in percent.
	FatE       Range `json:"fatE"`
	SatFatE    Range `json:"satFatE"`
	CarbsE     Range `json:"carbsE"`
	FreeSugarE Range `json:"freeSugarE"`
	ProteinE   Range `json:"proteinE"`
	// Grams per day.
	FiberG Range  `json:"fiberG"`
	SaltG  Range  `json:"saltG"`
	Source string `json:"source"`
}

const kcalPerMJ = 239.006

// henryBEE is resting energy in MJ/day by the Henry (2005) equations, with the
// coefficients NNR 2023 lists in Appendix 4, table 5 (W in kg, H in m).
func henryBEE(sex string, age int, w, h float64) (float64, string, bool) {
	type eq struct {
		maxAge  int
		group   string
		w, h, c float64
	}
	var eqs []eq
	switch sex {
	case "f":
		eqs = []eq{{24, "18–24", 0.0433, 2.57, -1.180}, {50, "25–50", 0.0342, 2.10, -0.0486},
			{70, "51–70", 0.0356, 1.76, 0.0448}, {200, "över 70", 0.0356, 1.76, 0.0448}}
	case "m":
		eqs = []eq{{24, "18–24", 0.0600, 1.31, 0.473}, {50, "25–50", 0.0476, 2.26, -0.574},
			{70, "51–70", 0.0478, 2.26, -1.070}, {200, "över 70", 0.0478, 2.26, -1.070}}
	default:
		return 0, "", false
	}
	if age < 18 {
		return 0, "", false
	}
	for _, e := range eqs {
		if age <= e.maxAge {
			return e.w*w + e.h*h + e.c, e.group, true
		}
	}
	return 0, "", false
}

// recommend returns nil until the profile holds sex, birth year, height and
// weight for an adult: a partial profile has no honest answer.
func recommend(p Profile, now time.Time) *Recommendation {
	if p.BirthYear == nil || p.HeightCm == nil || p.WeightKg == nil {
		return nil
	}
	age := now.Year() - *p.BirthYear
	bee, group, ok := henryBEE(p.Sex, age, *p.WeightKg, *p.HeightCm/100)
	if !ok {
		return nil
	}
	fiber := 25.0
	if p.Sex == "m" {
		fiber = 35
	}
	r := &Recommendation{
		Age: age, AgeGroup: group, RestKcal: bee * kcalPerMJ,
		EnergyLow: bee * 1.4 * kcalPerMJ, EnergyMid: bee * 1.6 * kcalPerMJ, EnergyHi: bee * 1.8 * kcalPerMJ,
		FatE: rng(25, 40), SatFatE: atMost(10), CarbsE: rng(45, 60), FreeSugarE: atMost(10), ProteinE: rng(10, 20),
		FiberG: atLeast(fiber), SaltG: atMost(5.75),
		Source: "NNR 2023 (Livsmedelsverket)",
	}
	if p.PAL != nil {
		r.EnergyPAL = bee * *p.PAL * kcalPerMJ
	}
	return r
}
