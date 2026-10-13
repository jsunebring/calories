package main

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func session(t *testing.T, store *Store, name, email string) (int64, string) {
	t.Helper()
	u, err := store.AddUser(name, email)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := store.NewSession(u.ID, email)
	if err != nil || tok == "" {
		t.Fatalf("session: %v", err)
	}
	return u.ID, tok
}

func TestPickingAUserNeedsTheMatchingEmail(t *testing.T) {
	srv := &Server{store: testStore(t)}
	if code, _ := apiReq(t, srv, "GET", "/api/day?day=2026-10-07", "", "-"); code != 401 {
		t.Fatalf("no user picked must be 401, got %d", code)
	}
	if code, _ := apiReq(t, srv, "POST", "/api/session", `{"userId":1,"email":"someone@else.com"}`, "-"); code != 401 {
		t.Fatalf("a wrong email must not start a session, got %d", code)
	}
	w := serve(t, srv, httptest.NewRequest("POST", "/api/session", strings.NewReader(`{"userId":1,"email":" Test@Example.com "}`)), "-")
	if w.Code != 200 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("email match is case- and space-insensitive; got %d", w.Code)
	}
	if code, body := apiReq(t, srv, "GET", "/api/me", "", w.Result().Cookies()[0].Value); code != 200 || body["name"] != "Test" {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestEachUserHasTheirOwnDay(t *testing.T) {
	store := testStore(t)
	srv := &Server{store: store}
	egg := &Food{Source: "manual", Name: "Ägg", Nutrients: Nutrients{Kcal: 140}}
	store.SaveFood(egg, "")
	id, _ := store.AddEntry(testUID, "2026-10-07", "breakfast", egg.ID, 60, "")
	_, anna := session(t, store, "Anna", "anna@example.com")

	_, body := apiReq(t, srv, "GET", "/api/day?day=2026-10-07", "", anna)
	if e := body["entries"].([]any); len(e) != 0 {
		t.Fatalf("Anna must not see Test's breakfast: %v", e)
	}
	if code, _ := apiReq(t, srv, "DELETE", fmt.Sprintf("/api/entries/%d", id), "", anna); code != 404 {
		t.Fatalf("Anna must not delete Test's entry, got %d", code)
	}
	if e, _ := store.Entries(testUID, "2026-10-07", "2026-10-07"); len(e) != 1 {
		t.Fatal("the entry must still be there")
	}
}

func TestGlassesAreSharedButNotOverwritten(t *testing.T) {
	store := testStore(t)
	srv := &Server{store: store}
	annaID, anna := session(t, store, "Anna", "anna@example.com")
	apiReq(t, srv, "POST", "/api/measures", `{"name":"Large glass","ml":300}`)
	apiReq(t, srv, "POST", "/api/measures", `{"name":"Large glass","ml":250}`, anna)

	mine, _ := store.Measures(testUID)
	hers, _ := store.Measures(annaID)
	if len(mine) != 2 || mine[0].Ml != 300 || !mine[0].Mine || mine[1].AddedBy != "Anna" {
		t.Fatalf("own glass first, Anna's after and named: %+v", mine)
	}
	if hers[0].Ml != 250 || !hers[0].Mine {
		t.Fatalf("Anna sees hers first: %+v", hers)
	}
	if code, _ := apiReq(t, srv, "DELETE", fmt.Sprintf("/api/measures/%d", mine[0].ID), "", anna); code != 404 {
		t.Fatalf("Anna may use Test's glass but not remove it, got %d", code)
	}
}

// A database from before users existed keeps its history: the first user
// added takes over every row that had no owner.
func TestFirstUserClaimsTheSingleUserHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, _ := sql.Open("sqlite", path)
	if _, err := db.Exec(schema + `ALTER TABLE foods ADD COLUMN g_per_ml REAL;
		ALTER TABLE entries ADD COLUMN estimate_id INTEGER;
		INSERT INTO foods (id, source, name, kcal, protein, carbs, fat, updated_at) VALUES (1,'manual','Ägg',140,12,0,10,'x');
		INSERT INTO entries (day, meal, food_id, grams, created_at) VALUES ('2026-10-01','breakfast',1,60,'x');
		INSERT INTO measures (name, ml) VALUES ('Large glass', 300);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := store.AddUser("Johan", "johan@example.com")
	if e, _ := store.Entries(u.ID, "2026-10-01", "2026-10-01"); len(e) != 1 || e[0].Food.AddedBy != "Johan" {
		t.Fatalf("history must belong to the first user: %+v", e)
	}
	if m, _ := store.Measures(u.ID); len(m) != 1 || !m[0].Mine {
		t.Fatalf("the glass must be Johan's: %+v", m)
	}
	second, _ := store.AddUser("Anna", "anna@example.com")
	if e, _ := store.Entries(second.ID, "2026-10-01", "2026-10-01"); len(e) != 0 {
		t.Fatal("a later user must start empty")
	}
}

func TestOwnKeyIsSealedAndUsed(t *testing.T) {
	store := testStore(t)
	box, _ := newKeyBox("a-long-enough-test-secret")
	var sentKey string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(toolUse("1", "submit_estimate", `{"dish":"x","note":"","components":[]}`)))
	}))
	defer api.Close()
	opts, _ := fakeClaude(t)
	opts[0] = optionBaseURL(api.URL)
	srv := &Server{store: store, keys: box, sharedKey: "shared-key", claudeOpts: opts, model: "claude-opus-5-5", photoDir: t.TempDir()}

	if code, body := apiReq(t, srv, "PUT", "/api/settings", `{"apiKey":"sk-ant-mine"}`); code != 200 || body["key"] != "set" {
		t.Fatalf("got %d %v", code, body)
	}
	var stored string
	store.db.QueryRow(`SELECT api_key FROM users WHERE id=1`).Scan(&stored)
	if strings.Contains(stored, "sk-ant-mine") || !strings.HasPrefix(stored, sealedPrefix) {
		t.Fatalf("the key must not be stored readable: %q", stored)
	}
	body, ct := estimateForm(t, "en macka", nil)
	req := httptest.NewRequest("POST", "/api/estimates", body)
	req.Header.Set("Content-Type", ct)
	estimateAndWait(t, srv, req)
	if sentKey != "sk-ant-mine" {
		t.Fatalf("the user's own key must be used over the shared one, sent %q", sentKey)
	}

	other, _ := newKeyBox("a-different-test-secret")
	if _, err := other.open(stored); err == nil {
		t.Fatal("a changed secret must not open the key")
	}
}

func TestWeekStartsOnMonday(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	day, week := spendWindows(time.Date(2026, 10, 7, 12, 0, 0, 0, loc)) // a Wednesday
	if day != "2026-10-06T22:00:00Z" || week != "2026-10-04T22:00:00Z" {
		t.Fatalf("day %s week %s", day, week)
	}
}

func TestPeopleShowsWhoseKeyPaidAndOnlyToTheAdmin(t *testing.T) {
	store := testStore(t) // "Test" is the first user, so the admin
	srv := &Server{store: store}
	annaID, anna := session(t, store, "Anna", "anna@example.com")
	today := time.Now().Format("2006-01-02")
	for _, c := range []struct {
		src  string
		cost float64
	}{{"own", 0.10}, {"shared", 0.07}, {"shared", 0.05}} {
		id, _ := store.StartEstimate(annaID, today, "lunch", "", "claude-opus-5-5", nil, c.src)
		cost := c.cost
		store.FinishEstimate(id, []byte("{}"), estimateUsage{}, &cost, []byte("{}"))
	}
	if code, _ := apiReq(t, srv, "GET", "/api/admin/people", "", anna); code != 403 {
		t.Fatalf("a non-admin must not see the overview, got %d", code)
	}
	_, body := apiReq(t, srv, "GET", "/api/admin/people?day="+today, "")
	var annaRow map[string]any
	for _, p := range body["people"].([]any) {
		if p.(map[string]any)["name"] == "Anna" {
			annaRow = p.(map[string]any)
		}
	}
	if annaRow == nil || annaRow["estimatesMonth"] != 3.0 || math.Abs(annaRow["costSharedMonth"].(float64)-0.12) > 1e-9 || annaRow["costOwnMonth"] != 0.10 {
		t.Fatalf("cost must be split by whose key paid: %v", annaRow)
	}
	if annaRow["lastSeen"] == nil {
		t.Fatal("Anna's request must have recorded her as active")
	}
}
