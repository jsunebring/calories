package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestParseOFFFound(t *testing.T) {
	r, err := parseOFF("5449000000996", fixture(t, "off-coca-cola.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Found || !r.Complete {
		t.Fatalf("want found+complete, got %+v", r)
	}
	f := r.Food
	if f.Name != "Coca-Cola Original" || !near(f.Kcal, 42.4) || !near(f.Carbs, 10.6) || f.Barcode != "5449000000996" {
		t.Fatalf("unexpected food %+v", f)
	}
	if f.ServingG == nil || !near(*f.ServingG, 330) {
		t.Fatalf("serving: %v", f.ServingG)
	}
}

func TestParseOFFTakesFirstBrand(t *testing.T) {
	r, err := parseOFF("7300400481588", fixture(t, "off-wasa-fibres.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Food.Brand != "wasa" || !near(r.Food.Kcal, 333) {
		t.Fatalf("got %+v", r.Food)
	}
}

func TestParseOFFNotFound(t *testing.T) {
	r, err := parseOFF("7310865000149", fixture(t, "off-not-found.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Found {
		t.Fatal("a status-0 body must read as not found")
	}
}

func TestParseOFFFallsBackToKJ(t *testing.T) {
	body := `{"status":1,"product":{"product_name":"X","nutriments":{"energy-kj_100g":418.4,"proteins_100g":1,"carbohydrates_100g":"2","fat_100g":3}}}`
	r, err := parseOFF("12345678", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Complete || !near(r.Food.Kcal, 100) || !near(r.Food.Carbs, 2) {
		t.Fatalf("got %+v", r)
	}
}

func TestParseOFFWithoutEnergyIsIncomplete(t *testing.T) {
	body := `{"status":1,"product":{"product_name":"X","nutriments":{}}}`
	r, _ := parseOFF("12345678", []byte(body))
	if !r.Found || r.Complete {
		t.Fatalf("want found but incomplete, got %+v", r)
	}
}

func TestParseSLVNutrientsPicksKcalNotKJ(t *testing.T) {
	n, err := parseSLVNutrients(fixture(t, "slv-231-naringsvarden.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Potatis höst kokt m. salt: 85 kcal / 355 kJ.
	if !near(n.Kcal, 85) || !near(n.Protein, 1.9) || !near(n.Carbs, 17.8) || !near(n.Fat, 0.1) {
		t.Fatalf("got %+v", n)
	}
	if n.Fiber == nil || !near(*n.Fiber, 2.1) || n.Salt == nil || !near(*n.Salt, 0.2) {
		t.Fatalf("optional values: %+v", n)
	}
}

func TestParseSLVList(t *testing.T) {
	items, total, err := parseSLVList(fixture(t, "slv-list-page.json"))
	if err != nil {
		t.Fatal(err)
	}
	if total != 2606 || len(items) != 4 || items[0].Version == "" || items[0].Namn == "" {
		t.Fatalf("total=%d items=%+v", total, items)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("Test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	return s
}

// testUID is the user every testStore starts with.
const testUID = 1

// serve runs a request as the test user, unless as names another session.
func serve(t *testing.T, srv *Server, req *http.Request, as ...string) *httptest.ResponseRecorder {
	t.Helper()
	tok := ""
	if len(as) > 0 {
		tok = as[0]
	} else {
		var err error
		if tok, _, err = srv.store.NewSession(testUID, "test@example.com"); err != nil || tok == "" {
			t.Fatalf("session: %v", err)
		}
	}
	if tok != "-" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	}
	mux := http.NewServeMux()
	srv.routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestSearchFoldsSwedishCase(t *testing.T) {
	s := testStore(t)
	for _, n := range []string{"Äpple rått", "Päron", "Äppelmos"} {
		if err := s.SaveFood(&Food{Source: "manual", Name: n, Nutrients: Nutrients{Kcal: 50}}, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SearchFoods(0, "äpple", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Äpple rått" {
		t.Fatalf("got %+v", got)
	}
}

func TestEntriesFollowFoodCorrections(t *testing.T) {
	s := testStore(t)
	f := &Food{Source: "manual", Name: "Gröt", Nutrients: Nutrients{Kcal: 70, Protein: 2, Carbs: 12, Fat: 1}}
	if err := s.SaveFood(f, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEntry(testUID, "2026-10-07", "breakfast", f.ID, 300, ""); err != nil {
		t.Fatal(err)
	}
	f.Kcal = 80
	if err := s.SaveFood(f, ""); err != nil {
		t.Fatal(err)
	}
	es, err := s.Entries(testUID, "2026-10-07", "2026-10-07")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || !near(es[0].Totals.Kcal, 240) || !near(es[0].Totals.Carbs, 36) {
		t.Fatalf("got %+v", es)
	}
}

func barcodeReq(t *testing.T, srv *Server, code string) (int, map[string]any) {
	t.Helper()
	w := serve(t, srv, httptest.NewRequest("GET", "/api/foods/barcode/"+code, nil))
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func down(string) (*OFFResult, error)    { return nil, errors.New("timeout") }
func missing(string) (*OFFResult, error) { return &OFFResult{}, nil }

func TestBarcodeOutageIsNotNotFound(t *testing.T) {
	srv := &Server{store: testStore(t), sources: []barcodeSource{{"Dabas", down}, {"OFF", down}}}
	code, _ := barcodeReq(t, srv, "7310865000149")
	if code != http.StatusBadGateway {
		t.Fatalf("every source down must be an error, not not_found; got %d", code)
	}
}

func TestBarcodeNotFoundNamesTheSourceThatFailed(t *testing.T) {
	srv := &Server{store: testStore(t), sources: []barcodeSource{{"Dabas", down}, {"OFF", missing}}}
	code, body := barcodeReq(t, srv, "7310865000149")
	if code != 200 || body["status"] != "not_found" {
		t.Fatalf("got %d %v", code, body)
	}
	if u, _ := body["unreachable"].([]any); len(u) != 1 || u[0] != "Dabas" {
		t.Fatalf("a miss with one source down must say which; got %v", body["unreachable"])
	}
}

func TestBarcodeFallsThroughToSecondSource(t *testing.T) {
	srv := &Server{store: testStore(t), sources: []barcodeSource{
		{"Dabas", func(c string) (*OFFResult, error) { return dabasLookupFrom(t, c, "dabas-gtin-miss.json") }},
		{"OFF", func(c string) (*OFFResult, error) { return parseOFF(c, fixture(t, "off-coca-cola.json")) }},
	}}
	_, body := barcodeReq(t, srv, "5449000000996")
	if body["status"] != "found" || body["food"].(map[string]any)["source"] != "off" {
		t.Fatalf("got %v", body)
	}
}

func TestBarcodeCachesAndPrefersLocal(t *testing.T) {
	calls := 0
	srv := &Server{store: testStore(t), sources: []barcodeSource{{"OFF", func(c string) (*OFFResult, error) {
		calls++
		return parseOFF(c, fixture(t, "off-coca-cola.json"))
	}}}}
	for i := 0; i < 2; i++ {
		code, body := barcodeReq(t, srv, "5449000000996")
		if code != 200 || body["status"] != "found" {
			t.Fatalf("got %d %v", code, body)
		}
	}
	if calls != 1 {
		t.Fatalf("second scan should be answered locally; source called %d times", calls)
	}
}

func dabasLookupFrom(t *testing.T, code, file string) (*OFFResult, error) {
	return dabasMatch(fixture(t, file), code)
}

func TestParseDabasBarcodeHit(t *testing.T) {
	r, err := dabasLookupFrom(t, "7350092520157", "dabas-gtin-mellanmjolk.json")
	if err != nil {
		t.Fatal(err)
	}
	f := r.Food
	if !r.Complete || f.Name != "Mellanmjölk 1,5%" || f.Source != "dabas" || f.Barcode != "7350092520157" {
		t.Fatalf("got %+v", f)
	}
	if !near(f.Kcal, 45) || !near(f.Protein, 3.5) || !near(f.Carbs, 4.9) || !near(f.Fat, 1.5) || f.Sugar == nil || !near(*f.Sugar, 4.9) {
		t.Fatalf("nutrients %+v", f.Nutrients)
	}
}

func TestParseDabasMiss(t *testing.T) {
	r, err := dabasLookupFrom(t, "7311070347275", "dabas-gtin-miss.json")
	if err != nil || r.Found {
		t.Fatalf("got %+v %v", r, err)
	}
}

func TestParseDabasTextSearch(t *testing.T) {
	foods, err := parseDabas(fixture(t, "dabas-search-pagen-gifflar.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(foods) != 4 || foods[0].Name != "Gifflar Kanel" || foods[0].Brand != "PÅGEN" || !near(foods[0].Kcal, 373) {
		t.Fatalf("got %+v", foods)
	}
}

func TestDabasSearchKeepsHitsWithoutOverwritingCorrections(t *testing.T) {
	store := testStore(t)
	search := func(string) ([]Food, error) { return parseDabas(fixture(t, "dabas-search-pagen-gifflar.json")) }
	srv := &Server{store: store, dabasSearch: search}
	get := func() {
		w := serve(t, srv, httptest.NewRequest("GET", "/api/foods/dabas?q=gifflar", nil))
		if w.Code != 200 {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	}
	get()
	hits, _ := search("")
	f, err := store.FoodByBarcode(hits[0].Barcode)
	if err != nil || f == nil {
		t.Fatalf("a Dabas hit must be kept; got %v %v", f, err)
	}
	f.Kcal = 999
	if err := store.SaveFood(f, ""); err != nil {
		t.Fatal(err)
	}
	get()
	if again, _ := store.FoodByBarcode(f.Barcode); again.Kcal != 999 {
		t.Fatalf("searching again overwrote a correction: kcal %v", again.Kcal)
	}
	if found, _ := store.SearchFoods(0, "gifflar", 40); len(found) != len(hits) {
		t.Fatalf("kept hits must be locally searchable; got %d of %d", len(found), len(hits))
	}
}

func TestRecordingKeepsOnlySuccessfulAnswers(t *testing.T) {
	store := testStore(t)
	status := 200
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte("answer " + http.StatusText(status)))
	}))
	defer upstream.Close()
	c := &http.Client{Transport: recordingTransport{store, http.DefaultTransport}}
	stored := func() string {
		var b string
		store.db.QueryRow(`SELECT body FROM lookups WHERE url=?`, upstream.URL+"/p").Scan(&b)
		return b
	}
	for _, st := range []int{200, 503} {
		status = st
		resp, err := c.Get(upstream.URL + "/p")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "answer "+http.StatusText(st) {
			t.Fatalf("the caller must still read the body; got %q", body)
		}
	}
	if got := stored(); got != "answer OK" {
		t.Fatalf("an outage must not replace the last good copy; stored %q", got)
	}
}

func TestDabasNum(t *testing.T) {
	for in, want := range map[string]float64{" 373 kcal": 373, " 2.5 g": 2.5, "< 0,1 g": 0.1, "0,46 g": 0.46} {
		if got := dabasNum(in); got == nil || !near(*got, want) {
			t.Fatalf("%q -> %v, want %v", in, got, want)
		}
	}
	if dabasNum(" - ") != nil {
		t.Fatal("a non-number must not read as zero")
	}
}

func TestMealsAreTheFourTheUIShows(t *testing.T) {
	html, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range meals {
		if !strings.Contains(string(html), `["`+m+`",`) {
			t.Fatalf("meal %q accepted by the API but missing from the page", m)
		}
	}
}

func apiReq(t *testing.T, srv *Server, method, path, body string, as ...string) (int, map[string]any) {
	t.Helper()
	w := serve(t, srv, httptest.NewRequest(method, path, strings.NewReader(body)), as...)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestResyncKeepsWhatWasSetByHand(t *testing.T) {
	store := testStore(t)
	f := &Food{Source: "slv", SourceRef: "1225", Name: "Mjölk mellan 1,5%", Nutrients: Nutrients{Kcal: 46}}
	if err := store.UpsertSLV(f, "v1"); err != nil {
		t.Fatal(err)
	}
	serving, density := 250.0, 1.03
	f.ServingG = &serving
	if err := store.SaveFood(f, "v1"); err != nil {
		t.Fatal(err)
	}
	store.SetGPerMl(f.ID, &density)
	store.SavePortion(testUID, f.ID, &Portion{Name: "glass", Grams: 300})

	if err := store.UpsertSLV(&Food{Source: "slv", SourceRef: "1225", Name: "Mjölk mellan 1,5%", Nutrients: Nutrients{Kcal: 47}}, "v2"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.FoodByID(f.ID)
	if got.Kcal != 47 {
		t.Fatalf("the update itself must land; kcal %v", got.Kcal)
	}
	if got.ServingG == nil || *got.ServingG != 250 || got.GPerMl == nil || *got.GPerMl != 1.03 {
		t.Fatalf("a re-sync wiped a hand-set value: serving %v, g/ml %v", got.ServingG, got.GPerMl)
	}
	if p, _ := store.Portions(testUID, f.ID); len(p) != 1 {
		t.Fatalf("a re-sync wiped portions: %v", p)
	}
}

func TestMeasuresComeOnlyWithPouredFoods(t *testing.T) {
	store := testStore(t)
	srv := &Server{store: store}
	store.SaveMeasure(testUID, &Measure{Name: "Large glass", Ml: 300})
	muesli := &Food{Source: "manual", Name: "Müsli"}
	milk := &Food{Source: "manual", Name: "Mjölk"}
	store.SaveFood(muesli, "")
	store.SaveFood(milk, "")
	one := 1.0
	store.SetGPerMl(milk.ID, &one)

	_, body := apiReq(t, srv, "GET", fmt.Sprintf("/api/amounts/%d", muesli.ID), "")
	if m := body["measures"].([]any); len(m) != 0 {
		t.Fatalf("a solid must not be offered a glass: %v", m)
	}
	_, body = apiReq(t, srv, "GET", fmt.Sprintf("/api/amounts/%d", milk.ID), "")
	if m := body["measures"].([]any); len(m) != 1 {
		t.Fatalf("a poured food must be offered the household measures: %v", body)
	}
}

func TestReweighingAPortionLeavesLoggedDaysAlone(t *testing.T) {
	store := testStore(t)
	srv := &Server{store: store}
	egg := &Food{Source: "manual", Name: "Ägg kokt", Nutrients: Nutrients{Kcal: 140}}
	store.SaveFood(egg, "")
	path := fmt.Sprintf("/api/foods/%d/portions", egg.ID)
	if code, _ := apiReq(t, srv, "POST", path, `{"name":"egg","grams":60}`); code != 200 {
		t.Fatalf("got %d", code)
	}
	store.AddEntry(testUID, "2026-10-07", "breakfast", egg.ID, 60, "")
	apiReq(t, srv, "POST", path, `{"name":"egg","grams":55}`)

	if p, _ := store.Portions(testUID, egg.ID); len(p) != 1 || p[0].Grams != 55 {
		t.Fatalf("saving the same name must re-weigh, not add: %v", p)
	}
	e, _ := store.Entries(testUID, "2026-10-07", "2026-10-07")
	if e[0].Grams != 60 {
		t.Fatalf("a logged day changed with the portion: %v g", e[0].Grams)
	}
	_, body := apiReq(t, srv, "GET", fmt.Sprintf("/api/amounts/%d", egg.ID), "")
	if body["lastGrams"] != 60.0 {
		t.Fatalf("lastGrams: %v", body["lastGrams"])
	}
}

func TestEveryParserReadsSaturatedFat(t *testing.T) {
	n, err := parseSLVNutrients(fixture(t, "slv-231-naringsvarden.json"))
	if err != nil || n.SatFat == nil || n.FreeSugar == nil {
		t.Fatalf("Livsmedelsverket: %+v %v", n, err)
	}
	off, _ := parseOFF("7300400481588", fixture(t, "off-wasa-fibres.json"))
	if off.Food.SatFat == nil {
		t.Fatal("Open Food Facts: no saturated fat")
	}
	dabas, _ := dabasLookupFrom(t, "7350092520157", "dabas-gtin-mellanmjolk.json")
	if dabas.Food.SatFat == nil {
		t.Fatal("Dabas: no saturated fat")
	}
}

// A product scanned before the parser read saturated fat gets it from the
// raw answer kept at the time, without asking the source again.
func TestBackfillReadsKeptLookups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := parseOFF("7300400481588", fixture(t, "off-wasa-fibres.json"))
	f := r.Food
	f.SatFat = nil
	store.SaveFood(&f, "")
	store.RecordLookup("world.openfoodfacts.org", "https://world.openfoodfacts.org/api/v2/product/7300400481588.json", fixture(t, "off-wasa-fibres.json"))
	store.db.Close()
	if store, err = OpenStore(path); err != nil {
		t.Fatal(err)
	}
	got, _ := store.FoodByID(f.ID)
	if got.SatFat == nil {
		t.Fatal("saturated fat must be filled from the kept lookup")
	}
}

func TestBackfillMatchesDabasSearchesByGTIN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	store, _ := OpenStore(path)
	hits, _ := parseDabas(fixture(t, "dabas-search-pagen-gifflar.json"))
	f := hits[0]
	f.SatFat = nil
	store.KeepFood(&f)
	store.RecordLookup("www.dabas.com", "https://www.dabas.com/api/v2/search?SearchText=gifflar", fixture(t, "dabas-search-pagen-gifflar.json"))
	store.db.Close()
	store, _ = OpenStore(path)
	got, _ := store.FoodByBarcode(f.Barcode)
	if got == nil || got.SatFat == nil {
		t.Fatalf("a food kept from a text search must be filled from that search's answer: %+v", got)
	}
}

func TestAnEntryKeepsThePortionItWasPickedBy(t *testing.T) {
	store := testStore(t)
	srv := &Server{store: store}
	apple := &Food{Source: "manual", Name: "Äpple", Nutrients: Nutrients{Kcal: 52}}
	store.SaveFood(apple, "")
	code, _ := apiReq(t, srv, "POST", "/api/entries", fmt.Sprintf(`{"day":"2026-10-07","meal":"snack","foodId":%d,"grams":260,"portion":"2 × One apple"}`, apple.ID))
	if code != 200 {
		t.Fatalf("got %d", code)
	}
	e, _ := store.Entries(testUID, "2026-10-07", "2026-10-07")
	if e[0].Grams != 260 || e[0].Portion != "2 × One apple" {
		t.Fatalf("two apples must not read as one 260 g apple: %+v", e[0])
	}
	_, body := apiReq(t, srv, "GET", fmt.Sprintf("/api/amounts/%d", apple.ID), "")
	if body["lastPortion"] != "2 × One apple" {
		t.Fatalf("Last must offer the portion as well as the grams: %v", body)
	}
}
