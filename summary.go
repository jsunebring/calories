package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (s *Store) Profile(uid int64) (Profile, error) {
	var p Profile
	err := s.db.QueryRow(`SELECT COALESCE(sex,''), birth_year, height_cm, weight_kg, pal, kcal_goal FROM users WHERE id=?`, uid).
		Scan(&p.Sex, &p.BirthYear, &p.HeightCm, &p.WeightKg, &p.PAL, &p.KcalGoal)
	return p, err
}

func (s *Store) SetProfile(uid int64, p Profile) error {
	_, err := s.db.Exec(`UPDATE users SET sex=?, birth_year=?, height_cm=?, weight_kg=?, pal=?, kcal_goal=? WHERE id=?`,
		nullStr(p.Sex), p.BirthYear, p.HeightCm, p.WeightKg, p.PAL, p.KcalGoal, uid)
	return err
}

// Totals counts, for each value a label may leave out, how many of the items
// carried it: "12 g fibre" over 9 items of which 4 had no fibre figure is not
// the same day as 12 g over 9 that all did.
type Totals struct {
	Kcal      float64        `json:"kcal"`
	Protein   float64        `json:"protein"`
	Carbs     float64        `json:"carbs"`
	Fat       float64        `json:"fat"`
	SatFat    float64        `json:"satFat"`
	Sugar     float64        `json:"sugar"`
	FreeSugar float64        `json:"freeSugar"`
	Fiber     float64        `json:"fiber"`
	Salt      float64        `json:"salt"`
	Items     int            `json:"items"`
	Known     map[string]int `json:"known"`
}

func newTotals() Totals { return Totals{Known: map[string]int{}} }

func (t *Totals) add(n Nutrients) {
	t.Kcal += n.Kcal
	t.Protein += n.Protein
	t.Carbs += n.Carbs
	t.Fat += n.Fat
	t.Items++
	for k, p := range map[string]*float64{"satFat": n.SatFat, "sugar": n.Sugar, "freeSugar": n.FreeSugar, "fiber": n.Fiber, "salt": n.Salt} {
		if p == nil {
			continue
		}
		t.Known[k]++
		switch k {
		case "satFat":
			t.SatFat += *p
		case "sugar":
			t.Sugar += *p
		case "freeSugar":
			t.FreeSugar += *p
		case "fiber":
			t.Fiber += *p
		case "salt":
			t.Salt += *p
		}
	}
}

func (t Totals) scaled(k float64) Totals {
	return Totals{Kcal: t.Kcal * k, Protein: t.Protein * k, Carbs: t.Carbs * k, Fat: t.Fat * k, SatFat: t.SatFat * k,
		Sugar: t.Sugar * k, FreeSugar: t.FreeSugar * k, Fiber: t.Fiber * k, Salt: t.Salt * k, Items: t.Items, Known: t.Known}
}

// summary is today plus the average of the logged days in the seven ending on
// it. A day with nothing logged is treated as not logged, not as a day of
// eating nothing, so it does not pull the average down.
func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	day := r.URL.Query().Get("day")
	if !validDay(day) {
		fail(w, 400, "day must be YYYY-MM-DD")
		return
	}
	d, _ := time.Parse("2006-01-02", day)
	from := d.AddDate(0, 0, -6).Format("2006-01-02")
	entries, err := s.store.Entries(uid, from, day)
	if err != nil {
		serverError(w, err)
		return
	}
	today, week := newTotals(), newTotals()
	days, todays := map[string]bool{}, []Entry{}
	for _, e := range entries {
		week.add(e.Totals)
		days[e.Day] = true
		if e.Day == day {
			today.add(e.Totals)
			todays = append(todays, e)
		}
	}
	avg, perDay := week, 1.0
	if len(days) > 0 {
		perDay = 1 / float64(len(days))
		avg = week.scaled(perDay)
	}
	p, err := s.store.Profile(uid)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"goal": p.KcalGoal, "rec": recommend(p, time.Now()), "today": today,
		"week":    map[string]any{"from": from, "to": day, "days": len(days), "avg": avg},
		"sources": map[string]any{"today": breakdown(todays, 1), "week": breakdown(entries, perDay)},
	})
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Profile(currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"profile": p, "rec": recommend(p, time.Now())})
}

func (s *Server) putProfile(w http.ResponseWriter, r *http.Request) {
	var p Profile
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		fail(w, 400, "bad json")
		return
	}
	p.Sex = strings.TrimSpace(p.Sex)
	bad := func(v *float64, lo, hi float64) bool { return v != nil && (*v < lo || *v > hi) }
	switch {
	case p.Sex != "" && p.Sex != "f" && p.Sex != "m":
		fail(w, 400, "sex must be f or m")
	case p.BirthYear != nil && (*p.BirthYear < 1900 || *p.BirthYear > time.Now().Year()):
		fail(w, 400, "birth year looks wrong")
	case bad(p.HeightCm, 100, 250), bad(p.WeightKg, 30, 300):
		fail(w, 400, "height in cm and weight in kg, please")
	case bad(p.PAL, 1.2, 2.5), bad(p.KcalGoal, 800, 6000):
		fail(w, 400, "activity or calorie goal out of range")
	default:
		if err := s.store.SetProfile(currentUser(r).ID, p); err != nil {
			serverError(w, err)
			return
		}
		s.getProfile(w, r)
	}
}

// source is one food's share of a nutrient total; Amount is nil when the
// food has no figure for it, which is what "known for 4 of 9" is made of.
type source struct {
	FoodID int64    `json:"foodId"`
	Name   string   `json:"name"`
	Amount *float64 `json:"amount"`
}

func nutrientValues(n Nutrients) map[string]*float64 {
	return map[string]*float64{"kcal": &n.Kcal, "protein": &n.Protein, "carbs": &n.Carbs, "fat": &n.Fat,
		"satFat": n.SatFat, "sugar": n.Sugar, "freeSugar": n.FreeSugar, "fiber": n.Fiber, "salt": n.Salt}
}

// breakdown answers, per nutrient, which foods the total came from, largest
// first. k scales the amounts the way the total they explain was scaled.
func breakdown(entries []Entry, k float64) map[string][]source {
	type acc struct {
		name string
		sums map[string]*float64
	}
	byFood, order := map[int64]*acc{}, []int64{}
	for _, e := range entries {
		a := byFood[e.Food.ID]
		if a == nil {
			a = &acc{name: e.Food.Name, sums: map[string]*float64{}}
			byFood[e.Food.ID] = a
			order = append(order, e.Food.ID)
		}
		for key, v := range nutrientValues(e.Totals) {
			if v == nil {
				continue
			}
			if a.sums[key] == nil {
				a.sums[key] = new(float64)
			}
			*a.sums[key] += *v * k
		}
	}
	out := map[string][]source{}
	for key := range nutrientValues(Nutrients{}) {
		list := []source{}
		for _, id := range order {
			list = append(list, source{FoodID: id, Name: byFood[id].name, Amount: byFood[id].sums[key]})
		}
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i].Amount, list[j].Amount
			if a == nil || b == nil {
				return b == nil && a != nil
			}
			return *a > *b
		})
		out[key] = list
	}
	return out
}
