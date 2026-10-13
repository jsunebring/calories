package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// barcodeSource is one place a scanned code can be answered. They are asked
// in order and the first complete answer wins.
type barcodeSource struct {
	Name   string
	Lookup func(string) (*OFFResult, error)
}

type Server struct {
	access      *accessVerifier // nil until CF_ACCESS_TEAM and CF_ACCESS_AUD are set
	sharedKey   string          // the template's key, for users without their own
	keys        *keyBox
	claudeOpts  []option.RequestOption
	model       string
	store       *Store
	slv         *slvSyncer
	photoDir    string
	imageDir    string
	images      *imageWorker
	sources     []barcodeSource
	dabasSearch func(string) ([]Food, error)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/users", s.listUsers)
	mux.HandleFunc("POST /api/session", s.startSession)
	mux.HandleFunc("DELETE /api/session", s.endSession)
	for pattern, h := range map[string]http.HandlerFunc{
		"GET /api/me":                     s.me,
		"GET /api/admin/people":           s.people,
		"PUT /api/me":                     s.rename,
		"GET /api/settings":               s.getSettings,
		"GET /api/profile":                s.getProfile,
		"PUT /api/profile":                s.putProfile,
		"GET /api/summary":                s.summary,
		"PUT /api/settings":               s.putSettings,
		"GET /api/day":                    s.getDay,
		"GET /api/foods/search":           s.searchFoods,
		"GET /api/foods/recent":           s.recentFoods,
		"GET /api/foods/barcode/{code}":   s.barcode,
		"GET /api/foods/dabas":            s.searchDabas,
		"POST /api/foods":                 s.saveFood,
		"PUT /api/foods/{id}":             s.saveFood,
		"GET /api/amounts/{id}":           s.amounts,
		"POST /api/foods/{id}/portions":   s.savePortion,
		"DELETE /api/portions/{id}":       s.deletePortion,
		"POST /api/measures":              s.saveMeasure,
		"DELETE /api/measures/{id}":       s.deleteMeasure,
		"POST /api/estimates":             s.newEstimate,
		"POST /api/estimates/{id}/accept": s.acceptEstimate,
		"GET /api/estimates/{id}":         s.getEstimate,
		"POST /api/entries":               s.addEntry,
		"PATCH /api/entries/{id}":         s.updateEntry,
		"DELETE /api/entries/{id}":        s.deleteEntry,
		"POST /api/photos":                s.addPhoto,
		"GET /api/photos/{id}":            s.getPhoto,
		"DELETE /api/photos/{id}":         s.deletePhoto,
		"PATCH /api/photos/{id}":          s.notePhoto,
		"GET /api/food-images/{id}":       s.foodImage,
	} {
		mux.HandleFunc(pattern, s.authed(h))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	fail(w, http.StatusInternalServerError, err.Error())
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) getDay(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("day")
	if !validDay(day) {
		fail(w, 400, "day must be YYYY-MM-DD")
		return
	}
	uid := currentUser(r).ID
	entries, err := s.store.Entries(uid, day, day)
	if err != nil {
		serverError(w, err)
		return
	}
	photos, err := s.store.Photos(uid, day)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"day": day, "entries": entries, "photos": photos})
}

func (s *Server) searchFoods(w http.ResponseWriter, r *http.Request) {
	foods, err := s.store.SearchFoods(currentUser(r).ID, r.URL.Query().Get("q"), 40)
	if err != nil {
		serverError(w, err)
		return
	}
	// The sync status rides along so an empty result during the first sync
	// says "still loading" rather than "no such food".
	writeJSON(w, 200, map[string]any{"foods": foods, "sync": s.slv.Status()})
}

func (s *Server) recentFoods(w http.ResponseWriter, r *http.Request) {
	foods, err := s.store.RecentFoods(currentUser(r).ID, 15)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"foods": foods})
}

var barcodeRe = regexp.MustCompile(`^[0-9]{6,14}$`)

// barcode answers found | incomplete | not_found. A product we have seen
// before, or entered by hand, is answered locally without asking again.
//
// not_found is only claimed when every source answered. A source that failed
// is named in `unreachable`, because "not in Dabas" and "Dabas was down" lead
// to different next steps.
func (s *Server) barcode(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !barcodeRe.MatchString(code) {
		fail(w, 400, "barcode must be 6–14 digits")
		return
	}
	if f, err := s.store.FoodByBarcode(code); err != nil {
		serverError(w, err)
		return
	} else if f != nil {
		writeJSON(w, 200, map[string]any{"status": "found", "food": f})
		return
	}
	var partial *Food
	unreachable := []string{}
	for _, src := range s.sources {
		res, err := src.Lookup(code)
		if err != nil {
			log.Printf("barcode %s: %s: %v", code, src.Name, err)
			unreachable = append(unreachable, src.Name)
			continue
		}
		if !res.Found {
			continue
		}
		if !res.Complete {
			if partial == nil {
				f := res.Food
				partial = &f
			}
			continue
		}
		if err := s.store.SaveFood(&res.Food, ""); err != nil {
			serverError(w, err)
			return
		}
		s.images.Wake()
		writeJSON(w, 200, map[string]any{"status": "found", "food": res.Food})
		return
	}
	if len(unreachable) == len(s.sources) {
		fail(w, http.StatusBadGateway, "no product database answered: "+strings.Join(unreachable, ", "))
		return
	}
	if partial != nil {
		writeJSON(w, 200, map[string]any{"status": "incomplete", "food": partial, "unreachable": unreachable})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "not_found", "food": Food{Barcode: code}, "unreachable": unreachable})
}

// searchDabas is a deliberate, per-tap text search. Every hit is kept, so
// what was found once is searchable here without asking Dabas again.
func (s *Server) searchDabas(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		fail(w, 400, "search for at least two characters")
		return
	}
	foods, err := s.dabasSearch(q)
	if err != nil {
		fail(w, http.StatusBadGateway, "Dabas did not answer: "+err.Error())
		return
	}
	for i := range foods {
		if err := s.store.KeepFood(&foods[i]); err != nil {
			log.Printf("keep dabas %s: %v", foods[i].Barcode, err)
		}
	}
	s.images.Wake()
	if len(foods) > 30 {
		foods = foods[:30]
	}
	writeJSON(w, 200, map[string]any{"foods": foods})
}

// saveFood creates a manual food, or edits any food. An edited scanned food
// keeps its barcode, so the correction is what the next scan finds.
func (s *Server) saveFood(w http.ResponseWriter, r *http.Request) {
	var f Food
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		fail(w, 400, "bad json")
		return
	}
	f.Name = strings.TrimSpace(f.Name)
	f.Barcode = strings.TrimSpace(f.Barcode)
	if f.Name == "" {
		fail(w, 400, "name is required")
		return
	}
	if f.GPerMl != nil && (*f.GPerMl <= 0 || *f.GPerMl > 3) {
		fail(w, 400, "g per ml must be between 0 and 3")
		return
	}
	if f.Kcal < 0 || f.Protein < 0 || f.Carbs < 0 || f.Fat < 0 {
		fail(w, 400, "values cannot be negative")
		return
	}
	if f.Barcode != "" && !barcodeRe.MatchString(f.Barcode) {
		fail(w, 400, "barcode must be 6–14 digits")
		return
	}
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			fail(w, 400, "bad id")
			return
		}
		old, err := s.store.FoodByID(id)
		if err != nil {
			serverError(w, err)
			return
		}
		if old == nil {
			fail(w, 404, "no such food")
			return
		}
		f.ID, f.Source, f.SourceRef = old.ID, old.Source, old.SourceRef
	} else {
		f.ID, f.Source = 0, "manual"
		f.SourceRef = ""
		if f.Barcode != "" {
			if existing, err := s.store.FoodByBarcode(f.Barcode); err != nil {
				serverError(w, err)
				return
			} else if existing != nil {
				f.ID, f.Source, f.SourceRef = existing.ID, existing.Source, existing.SourceRef
			}
		}
	}
	if err := s.store.saveFood(&f, "", currentUser(r).ID); err != nil {
		serverError(w, err)
		return
	}
	if err := s.store.SetGPerMl(f.ID, f.GPerMl); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, f)
}

// amounts is everything the amount sheet offers for one food. Measures come
// only with a poured food: a glass of muesli is not a weight.
func (s *Server) amounts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	f, err := s.store.FoodByID(id)
	if err != nil {
		serverError(w, err)
		return
	}
	if f == nil {
		fail(w, 404, "no such food")
		return
	}
	uid := currentUser(r).ID
	portions, err := s.store.Portions(uid, id)
	if err != nil {
		serverError(w, err)
		return
	}
	measures := []Measure{}
	if f.GPerMl != nil {
		if measures, err = s.store.Measures(uid); err != nil {
			serverError(w, err)
			return
		}
	}
	last, lastPortion, err := s.store.LastAmount(uid, id)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"portions": portions, "measures": measures, "lastGrams": last, "lastPortion": lastPortion})
}

func (s *Server) savePortion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	var p Portion
	if !ok || json.NewDecoder(r.Body).Decode(&p) != nil {
		fail(w, 400, "bad request")
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || p.Grams <= 0 {
		fail(w, 400, "need a name and grams > 0")
		return
	}
	if f, err := s.store.FoodByID(id); err != nil {
		serverError(w, err)
		return
	} else if f == nil {
		fail(w, 404, "no such food")
		return
	}
	if err := s.store.SavePortion(currentUser(r).ID, id, &p); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) deletePortion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	if ok, err := s.store.DeletePortion(currentUser(r).ID, id); err != nil {
		serverError(w, err)
		return
	} else if !ok {
		fail(w, 404, "only whoever added it can remove it")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) saveMeasure(w http.ResponseWriter, r *http.Request) {
	var m Measure
	if json.NewDecoder(r.Body).Decode(&m) != nil {
		fail(w, 400, "bad json")
		return
	}
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" || m.Ml <= 0 {
		fail(w, 400, "need a name and ml > 0")
		return
	}
	if err := s.store.SaveMeasure(currentUser(r).ID, &m); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) deleteMeasure(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	if ok, err := s.store.DeleteMeasure(currentUser(r).ID, id); err != nil {
		serverError(w, err)
		return
	} else if !ok {
		fail(w, 404, "only whoever added it can remove it")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type entryReq struct {
	Day    string  `json:"day"`
	Meal   string  `json:"meal"`
	FoodID int64   `json:"foodId"`
	Grams  float64 `json:"grams"`
	// Portion is display text only; Grams is what is counted.
	Portion string `json:"portion"`
}

func (e *entryReq) portion() string {
	p := strings.TrimSpace(e.Portion)
	if len([]rune(p)) > 80 {
		p = string([]rune(p)[:80])
	}
	return p
}

func (s *Server) addEntry(w http.ResponseWriter, r *http.Request) {
	var e entryReq
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		fail(w, 400, "bad json")
		return
	}
	if !validDay(e.Day) || !validMeal(e.Meal) || e.Grams <= 0 {
		fail(w, 400, "need day, meal and grams > 0")
		return
	}
	if f, err := s.store.FoodByID(e.FoodID); err != nil {
		serverError(w, err)
		return
	} else if f == nil {
		fail(w, 404, "no such food")
		return
	}
	id, err := s.store.AddEntry(currentUser(r).ID, e.Day, e.Meal, e.FoodID, e.Grams, e.portion())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}

func (s *Server) updateEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	var e entryReq
	if !ok || json.NewDecoder(r.Body).Decode(&e) != nil || !validMeal(e.Meal) || e.Grams <= 0 {
		fail(w, 400, "need meal and grams > 0")
		return
	}
	if ok, err := s.store.UpdateEntry(currentUser(r).ID, id, e.Meal, e.Grams, e.portion()); err != nil {
		serverError(w, err)
		return
	} else if !ok {
		fail(w, 404, "no such entry")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	if ok, err := s.store.DeleteEntry(currentUser(r).ID, id); err != nil {
		serverError(w, err)
		return
	} else if !ok {
		fail(w, 404, "no such entry")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

var photoExt = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}

// readPhoto returns the form's photo, or nil when none was sent.
func readPhoto(r *http.Request) ([]byte, string, error) {
	file, _, err := r.FormFile("photo")
	if errors.Is(err, http.ErrMissingFile) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", errors.New("photo too large")
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, "", errors.New("photo too large")
	}
	mt := http.DetectContentType(data)
	if _, ok := photoExt[mt]; !ok {
		return nil, "", errors.New("photo must be jpeg, png or webp")
	}
	return data, mt, nil
}

func (s *Server) savePhoto(uid int64, day, meal, note string, data []byte, mediaType string) (int64, error) {
	b := make([]byte, 12)
	rand.Read(b)
	name := day + "-" + hex.EncodeToString(b) + photoExt[mediaType]
	if err := os.WriteFile(filepath.Join(s.photoDir, name), data, 0o644); err != nil {
		return 0, err
	}
	id, err := s.store.AddPhoto(uid, day, meal, name, note)
	if err != nil {
		os.Remove(filepath.Join(s.photoDir, name))
	}
	return id, err
}

func (s *Server) addPhoto(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	day, meal := r.FormValue("day"), r.FormValue("meal")
	if !validDay(day) || !validMeal(meal) {
		fail(w, 400, "need day and meal")
		return
	}
	data, mt, err := readPhoto(r)
	if err != nil || data == nil {
		fail(w, 400, firstNonEmpty(errText(err), "no photo"))
		return
	}
	id, err := s.savePhoto(currentUser(r).ID, day, meal, strings.TrimSpace(r.FormValue("note")), data, mt)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}

func (s *Server) notePhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	var req struct {
		Note string `json:"note"`
	}
	if !ok || json.NewDecoder(r.Body).Decode(&req) != nil {
		fail(w, 400, "bad request")
		return
	}
	if ok, err := s.store.SetPhotoNote(currentUser(r).ID, id, strings.TrimSpace(req.Note)); err != nil {
		serverError(w, err)
		return
	} else if !ok {
		fail(w, 404, "no such photo")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// newEstimate asks Claude about a photo and/or description and returns the
// components for review. Nothing is logged until the review is accepted;
// the photo is kept on the meal either way, since it was taken for the record.
func (s *Server) newEstimate(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	key, keySource, err := s.keyFor(uid)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	day, meal := r.FormValue("day"), r.FormValue("meal")
	desc := strings.TrimSpace(r.FormValue("description"))
	if !validDay(day) || !validMeal(meal) {
		fail(w, 400, "need day and meal")
		return
	}
	data, mt, err := readPhoto(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	var photoID *int64
	// A photo already on the meal (snapped earlier, estimated later) is used
	// from disk rather than uploaded and stored a second time.
	if data == nil && r.FormValue("photoId") != "" {
		id, err := strconv.ParseInt(r.FormValue("photoId"), 10, 64)
		name := ""
		if err == nil {
			name, err = s.store.PhotoFile(uid, id)
		}
		if err != nil || name == "" {
			fail(w, 404, "no such photo")
			return
		}
		if data, err = os.ReadFile(filepath.Join(s.photoDir, name)); err != nil {
			serverError(w, err)
			return
		}
		mt, photoID = http.DetectContentType(data), &id
		if desc == "" {
			if desc, err = s.store.PhotoNote(uid, id); err != nil {
				serverError(w, err)
				return
			}
		} else if _, err := s.store.SetPhotoNote(uid, id, desc); err != nil {
			serverError(w, err)
			return
		}
	} else if data != nil {
		id, err := s.savePhoto(uid, day, meal, desc, data, mt)
		if err != nil {
			serverError(w, err)
			return
		}
		photoID = &id
	}
	if data == nil && desc == "" {
		fail(w, 400, "add a photo or a description")
		return
	}
	id, err := s.store.StartEstimate(uid, day, meal, desc, s.model, photoID, keySource)
	if err != nil {
		serverError(w, err)
		return
	}
	// The answer can take a minute and Cloudflare drops a request at 100 s,
	// so the work runs on its own and the page asks for the result.
	client := anthropic.NewClient(append(append([]option.RequestOption{}, s.claudeOpts...), option.WithAPIKey(key))...)
	go s.runEstimate(id, client, uid, data, mt, desc)
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "status": "running"})
}

func (s *Server) runEstimate(id int64, client anthropic.Client, uid int64, data []byte, mt, desc string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, raw, usage, err := estimate(ctx, client, s.model, s.store, uid, data, mt, desc)
	var items []proposedItem
	if err == nil {
		items, err = proposal(s.store, res)
	}
	if err != nil {
		log.Printf("estimate %d: %v", id, err)
		// Tokens spent on a failed run still count toward the limit.
		if ferr := s.store.FailEstimate(id, err.Error(), usage, estimateCost(s.model, usage)); ferr != nil {
			log.Printf("estimate %d: %v", id, ferr)
		}
		return
	}
	cost := estimateCost(s.model, usage)
	result, _ := json.Marshal(map[string]any{"dish": res.Dish, "note": res.Note, "items": items, "usage": usage, "model": s.model, "cost": cost})
	if err := s.store.FinishEstimate(id, raw, usage, cost, result); err != nil {
		log.Printf("estimate %d: %v", id, err)
	}
}

func (s *Server) getEstimate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	st, err := s.store.EstimateState(currentUser(r).ID, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if st == nil {
		fail(w, 404, "no such estimate")
		return
	}
	out := map[string]any{}
	if st.Result != "" {
		json.Unmarshal([]byte(st.Result), &out)
	}
	out["id"], out["status"], out["error"] = id, st.Status, st.Error
	writeJSON(w, 200, out)
}

// keyFor is the user's own key, or the shared one when they have none, and
// which of the two it was: that is whose bill the estimate lands on. A saved
// key that cannot be opened is an error, not a quiet switch to the shared one.
func (s *Server) keyFor(uid int64) (string, string, error) {
	st, err := s.store.UserSettings(uid)
	if err != nil {
		return "", "", err
	}
	if st.SealedKey != "" {
		if s.keys == nil {
			return "", "", errors.New("your saved API key cannot be read: KEY_SECRET is not set on the container")
		}
		k, err := s.keys.open(st.SealedKey)
		return k, "own", err
	}
	if s.sharedKey == "" {
		return "", "", errors.New("no API key: add yours under your name at the top")
	}
	return s.sharedKey, "shared", nil
}

func (s *Server) acceptEstimate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	var req struct {
		Items []proposedItem `json:"items"`
	}
	if !ok || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Items) == 0 {
		fail(w, 400, "need at least one item")
		return
	}
	est, err := s.store.EstimateByID(id)
	if err != nil {
		serverError(w, err)
		return
	}
	if est == nil || est.UserID != currentUser(r).ID {
		fail(w, 404, "no such estimate")
		return
	}
	for i, it := range req.Items {
		if it.Grams <= 0 {
			fail(w, 400, "grams must be > 0")
			return
		}
		if it.Food.ID != 0 {
			f, err := s.store.FoodByID(it.Food.ID)
			if err != nil {
				serverError(w, err)
				return
			}
			if f == nil {
				fail(w, 404, "no such food")
				return
			}
			req.Items[i].Food = *f
		} else if strings.TrimSpace(it.Food.Name) == "" || it.Food.Kcal < 0 {
			fail(w, 400, "a new food needs a name")
			return
		}
	}
	if err := s.store.AcceptEstimate(est, req.Items); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) getPhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	name, err := s.store.PhotoFile(currentUser(r).ID, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if name == "" {
		fail(w, 404, "no such photo")
		return
	}
	// SQLite hands a deleted photo's id to the next upload, so the URL does not
	// name the content: revalidate on every view rather than cache it for good.
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeFile(w, r, filepath.Join(s.photoDir, name))
}

func (s *Server) deletePhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	name, err := s.store.DeletePhoto(currentUser(r).ID, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if name != "" {
		if err := os.Remove(filepath.Join(s.photoDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("remove photo %s: %v", name, err)
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
