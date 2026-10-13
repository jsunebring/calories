package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

func toolUse(id, name, input string) string {
	return fmt.Sprintf(`{"id":"msg_%s","type":"message","role":"assistant","model":"m","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"%s","name":"%s","input":%s}],"usage":{"input_tokens":1000,"output_tokens":100}}`, id, id, name, input)
}

// fakeClaude answers each Messages call with the next canned response and
// keeps the request bodies, so a test can see what was sent back.
func fakeClaude(t *testing.T, replies ...string) ([]option.RequestOption, *[]string) {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) > len(replies) {
			t.Errorf("unexpected call %d", len(bodies))
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(replies[len(bodies)-1]))
	}))
	t.Cleanup(srv.Close)
	return []option.RequestOption{option.WithBaseURL(srv.URL), option.WithMaxRetries(0)}, &bodies
}

func estimateForm(t *testing.T, desc string, photo []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("day", "2026-10-07")
	mw.WriteField("meal", "lunch")
	mw.WriteField("description", desc)
	if photo != nil {
		fw, _ := mw.CreateFormFile("photo", "meal.jpg")
		fw.Write(photo)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

// A minimal JPEG header is enough for content sniffing.
var jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, make([]byte, 64)...)

func TestEstimateUsesTheDatabaseAndLogsOnlyOnAccept(t *testing.T) {
	store := testStore(t)
	rice := &Food{Source: "slv", SourceRef: "1", Name: "Ris parboiled kokt", Nutrients: Nutrients{Kcal: 120, Protein: 3, Carbs: 26, Fat: 0.3}}
	store.SaveFood(rice, "")
	submit := fmt.Sprintf(`{"dish":"Kycklinggryta med ris","note":"Sauce amount is a guess",
		"components":[
		 {"name":"Ris","food_id":%d,"grams":200,"per_100g":{"kcal":999,"protein":0,"carbs":0,"fat":0}},
		 {"name":"Kycklinggryta med grädde","food_id":0,"grams":250,"per_100g":{"kcal":150,"protein":12,"carbs":4,"fat":9}}]}`, rice.ID)
	opts, bodies := fakeClaude(t, toolUse("1", "search_foods", `{"query":"ris kokt"}`), toolUse("2", "submit_estimate", submit))
	srv := &Server{store: store, sharedKey: "test", claudeOpts: opts, model: "claude-test", photoDir: t.TempDir()}

	body, ct := estimateForm(t, "kycklinggryta, stor tallrik", jpegBytes)
	req := httptest.NewRequest("POST", "/api/estimates", body)
	req.Header.Set("Content-Type", ct)
	result := estimateAndWait(t, srv, req)
	if !strings.Contains((*bodies)[0], `"type":"image"`) || !strings.Contains((*bodies)[0], "stor tallrik") {
		t.Fatal("the photo and description must both reach the model")
	}
	if !strings.Contains((*bodies)[1], "Ris parboiled kokt") {
		t.Fatal("the search result must be sent back to the model")
	}
	var got struct {
		ID    int64
		Items []proposedItem
	}
	json.Unmarshal(result, &got)
	if len(got.Items) != 2 || got.Items[0].Food.ID != rice.ID || got.Items[0].Food.Kcal != 120 {
		t.Fatalf("a matched component must carry the database's values, not the model's: %+v", got.Items)
	}
	if got.Items[1].Food.ID != 0 || got.Items[1].Food.Kcal != 150 {
		t.Fatalf("an unmatched component must be an unsaved draft: %+v", got.Items[1])
	}
	if e, _ := store.Entries(testUID, "2026-10-07", "2026-10-07"); len(e) != 0 {
		t.Fatal("nothing may be logged before the review is accepted")
	}
	if p, _ := store.Photos(testUID, "2026-10-07"); len(p) != 1 || p[0].Note != "kycklinggryta, stor tallrik" {
		t.Fatalf("the description must stay with the photo as its note: %+v", p)
	}

	got.Items[0].Grams = 150 // reviewed down
	b, _ := json.Marshal(map[string]any{"items": got.Items})
	if code, _ := apiReq(t, srv, "POST", fmt.Sprintf("/api/estimates/%d/accept", got.ID), string(b)); code != 200 {
		t.Fatalf("accept: %d", code)
	}
	e, _ := store.Entries(testUID, "2026-10-07", "2026-10-07")
	if len(e) != 2 || !e[0].Estimated || e[0].Meal != "lunch" || e[0].Grams != 150 || e[1].Food.Source != "estimate" {
		t.Fatalf("accepted entries: %+v", e)
	}
	var model string
	var in int
	store.db.QueryRow(`SELECT model, input_tokens FROM estimates WHERE id=?`, got.ID).Scan(&model, &in)
	if model != "claude-test" || in != 2000 {
		t.Fatalf("the estimate must record its model and summed usage: %q %d", model, in)
	}
}

func TestEstimateWithoutAKeySaysSo(t *testing.T) {
	srv := &Server{store: testStore(t)}
	code, body := apiReq(t, srv, "POST", "/api/estimates", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(body["error"].(string), "no API key") {
		t.Fatalf("got %d %v", code, body)
	}
}

func optionBaseURL(u string) option.RequestOption { return option.WithBaseURL(u) }

// estimateAndWait starts an estimate and polls it the way the page does,
// returning the finished state's body.
func estimateAndWait(t *testing.T, srv *Server, req *http.Request, as ...string) []byte {
	t.Helper()
	w := serve(t, srv, req, as...)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var started struct{ ID int64 }
	json.Unmarshal(w.Body.Bytes(), &started)
	for i := 0; i < 200; i++ {
		g := serve(t, srv, httptest.NewRequest("GET", fmt.Sprintf("/api/estimates/%d", started.ID), nil), as...)
		var st struct{ Status, Error string }
		json.Unmarshal(g.Body.Bytes(), &st)
		switch st.Status {
		case "done":
			return g.Body.Bytes()
		case "failed":
			t.Fatalf("estimate failed: %s", st.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("estimate never finished")
	return nil
}

func TestEstimateFromAPhotoAlreadyOnTheMeal(t *testing.T) {
	store := testStore(t)
	opts, bodies := fakeClaude(t, toolUse("1", "submit_estimate", `{"dish":"x","note":"","components":[]}`))
	srv := &Server{store: store, sharedKey: "k", claudeOpts: opts, model: "claude-opus-5-5", photoDir: t.TempDir()}
	pid, err := srv.savePhoto(testUID, "2026-10-07", "lunch", "kycklinggryta, stor tallrik", jpegBytes, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	form := func(id int64) (*http.Request, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("day", "2026-10-07")
		mw.WriteField("meal", "lunch")
		mw.WriteField("photoId", fmt.Sprint(id))
		mw.Close()
		req := httptest.NewRequest("POST", "/api/estimates", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return req, ""
	}
	req, _ := form(pid)
	estimateAndWait(t, srv, req)
	if !strings.Contains((*bodies)[0], `"type":"image"`) || !strings.Contains((*bodies)[0], "stor tallrik") {
		t.Fatal("the saved photo and its note must both reach the model")
	}
	if p, _ := store.Photos(testUID, "2026-10-07"); len(p) != 1 {
		t.Fatalf("estimating from a saved photo must not store it twice: %d photos", len(p))
	}
	_, anna := session(t, store, "Anna", "anna@example.com")
	req, _ = form(pid)
	if w := serve(t, srv, req, anna); w.Code != 404 {
		t.Fatalf("someone else's photo must not be usable, got %d", w.Code)
	}
}

func TestARestartReportsRunningEstimatesAsCutOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	store, _ := OpenStore(path)
	u, _ := store.AddUser("Test", "test@example.com")
	id, _ := store.StartEstimate(u.ID, "2026-10-07", "lunch", "", "claude-opus-5-5", nil, "shared")
	store.db.Close()
	store, _ = OpenStore(path)
	st, _ := store.EstimateState(u.ID, id)
	if st.Status != "failed" || !strings.Contains(st.Error, "restart") {
		t.Fatalf("an estimate a restart ended must not look like it is still running: %+v", st)
	}
}

func TestAReusedPhotoIdIsNeverServedFromCache(t *testing.T) {
	store := testStore(t)
	srv := &Server{store: store, photoDir: t.TempDir()}
	first, _ := srv.savePhoto(testUID, "2026-10-07", "breakfast", "", jpegBytes, "image/jpeg")
	if w := serve(t, srv, httptest.NewRequest("DELETE", fmt.Sprintf("/api/photos/%d", first), nil)); w.Code >= 300 {
		t.Fatalf("delete: %d", w.Code)
	}
	second, _ := srv.savePhoto(testUID, "2026-10-07", "breakfast", "", jpegBytes, "image/jpeg")
	if second != first {
		t.Skip("ids are no longer reused; the cache rule can be revisited")
	}
	w := serve(t, srv, httptest.NewRequest("GET", fmt.Sprintf("/api/photos/%d", second), nil))
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("a photo URL that a later upload can take over must be revalidated, got %q", cc)
	}
}

func TestEstimatedEntriesKnowTheirPhotoButNotAReusedId(t *testing.T) {
	store := testStore(t)
	pid, _ := store.AddPhoto(testUID, "2026-10-07", "breakfast", "a.jpg", "")
	eid, _ := store.StartEstimate(testUID, "2026-10-07", "breakfast", "", "claude-opus-5-5", &pid, "shared")
	store.db.Exec(`UPDATE photos SET created_at='2026-10-07T07:59:00Z' WHERE id=?`, pid)
	store.db.Exec(`UPDATE estimates SET created_at='2026-10-07T08:00:00Z' WHERE id=?`, eid)
	est := &Estimate{ID: eid, UserID: testUID, Day: "2026-10-07", Meal: "breakfast"}
	if err := store.AcceptEstimate(est, []proposedItem{{Name: "tortilla", Grams: 60, Food: Food{Name: "tortilla", Nutrients: Nutrients{Kcal: 300}}}}); err != nil {
		t.Fatal(err)
	}
	es, _ := store.Entries(testUID, "2026-10-07", "2026-10-07")
	if len(es) != 1 || es[0].PhotoID == nil || *es[0].PhotoID != pid {
		t.Fatalf("an entry estimated from a photo must name it: %+v", es)
	}
	store.DeletePhoto(testUID, pid)
	if again, _ := store.AddPhoto(testUID, "2026-10-07", "breakfast", "b.jpg", ""); again != pid {
		t.Skip("ids are no longer reused")
	}
	es, _ = store.Entries(testUID, "2026-10-07", "2026-10-07")
	if es[0].PhotoID != nil {
		t.Fatal("a photo uploaded after the estimate took a deleted photo's id; the entry must not claim it")
	}
}

func TestAnEstimatedEntryChangedByHandIsNoLongerAnEstimate(t *testing.T) {
	store := testStore(t)
	pid, _ := store.AddPhoto(testUID, "2026-10-07", "dinner", "a.jpg", "")
	eid, _ := store.StartEstimate(testUID, "2026-10-07", "dinner", "", "claude-opus-5-5", &pid, "shared")
	store.AcceptEstimate(&Estimate{ID: eid, UserID: testUID, Day: "2026-10-07", Meal: "dinner"},
		[]proposedItem{{Name: "äpple", Grams: 260, Food: Food{Name: "äpple", Nutrients: Nutrients{Kcal: 52}}}})
	es, _ := store.Entries(testUID, "2026-10-07", "2026-10-07")
	id := es[0].ID
	store.UpdateEntry(testUID, id, "snack", 260, "")
	if es, _ = store.Entries(testUID, "2026-10-07", "2026-10-07"); !es[0].Estimated {
		t.Fatal("moving an estimate to another meal does not make its amount anyone's but Claude's")
	}
	store.UpdateEntry(testUID, id, "dinner", 260, "2 × Ett äpple")
	if es, _ = store.Entries(testUID, "2026-10-07", "2026-10-07"); es[0].Estimated {
		t.Fatal("an amount picked by hand is not an estimate, even when it lands on the same grams")
	}
	if es[0].PhotoID == nil || *es[0].PhotoID != pid {
		t.Fatal("it still came from the photo")
	}
}
