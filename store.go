package main

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Nutrients are always per 100 g. An entry stores only food + grams and is
// computed at read time, so correcting a food's label fixes every day it was
// eaten instead of leaving the typo frozen into history.
type Nutrients struct {
	Kcal    float64  `json:"kcal"`
	Protein float64  `json:"protein"`
	Carbs   float64  `json:"carbs"`
	Fat     float64  `json:"fat"`
	Sugar   *float64 `json:"sugar"`
	Fiber   *float64 `json:"fiber"`
	Salt    *float64 `json:"salt"`
	SatFat  *float64 `json:"satFat"`
	// FreeSugar is what the NNR limit is about. Labels give only total
	// sugars, so this is known for Livsmedelsverket foods alone.
	FreeSugar *float64 `json:"freeSugar"`
}

type Food struct {
	ID        int64    `json:"id"`
	Source    string   `json:"source"` // off | slv | manual
	SourceRef string   `json:"sourceRef,omitempty"`
	Barcode   string   `json:"barcode,omitempty"`
	Name      string   `json:"name"`
	Brand     string   `json:"brand,omitempty"`
	ServingG  *float64 `json:"servingG"`
	// GPerMl marks a food as poured, so household measures apply to it.
	// nil means it is not measured by volume.
	GPerMl *float64 `json:"gPerMl"`
	// AddedBy names whoever entered a food by hand or accepted it from an
	// estimate. Foods are shared, so it says whose label this is.
	AddedBy string `json:"addedBy,omitempty"`
	// HasImage means a thumbnail is stored; ImageURL is where a source said
	// the product photo is, kept so the download can be retried.
	HasImage bool   `json:"image"`
	ImageURL string `json:"-"`
	Nutrients
}

// A Measure is a piece of the household's dishware. A Portion is one food's
// own unit. Both only fill in the grams when logging; an entry never refers
// to one, so new glasses or a re-weighed egg leave past days as they were.
type Measure struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Ml      float64 `json:"ml"`
	AddedBy string  `json:"addedBy"`
	Mine    bool    `json:"mine"`
}

type Portion struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Grams   float64 `json:"grams"`
	AddedBy string  `json:"addedBy"`
	Mine    bool    `json:"mine"`
}

type Entry struct {
	ID      int64     `json:"id"`
	Day     string    `json:"day"`
	Meal    string    `json:"meal"`
	Grams   float64   `json:"grams"`
	Food    Food      `json:"food"`
	Totals  Nutrients `json:"totals"`
	Created string    `json:"created"`
	// Estimated marks an entry that came from a photo or description
	// estimate rather than a weighed or labelled amount.
	Estimated bool `json:"estimated"`
	// Portion is how the amount was picked ("2 × One apple"), copied at the
	// time so a later edit to the portion does not rewrite a logged day.
	Portion string `json:"portion,omitempty"`
	// PhotoID is the photo on this meal the entry was estimated from.
	PhotoID *int64 `json:"photoId,omitempty"`
}

type Photo struct {
	ID      int64  `json:"id"`
	Day     string `json:"day"`
	Meal    string `json:"meal"`
	Note    string `json:"note"`
	Created string `json:"created"`
}

var meals = []string{"breakfast", "lunch", "snack", "dinner"}

func validMeal(m string) bool {
	for _, x := range meals {
		if x == m {
			return true
		}
	}
	return false
}

func validDay(d string) bool {
	_, err := time.Parse("2006-01-02", d)
	return err == nil
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS foods (
	id         INTEGER PRIMARY KEY,
	source     TEXT NOT NULL,
	source_ref TEXT NOT NULL DEFAULT '',
	version    TEXT NOT NULL DEFAULT '',
	barcode    TEXT,
	name       TEXT NOT NULL,
	brand      TEXT NOT NULL DEFAULT '',
	serving_g  REAL,
	kcal       REAL NOT NULL,
	protein    REAL NOT NULL,
	carbs      REAL NOT NULL,
	fat        REAL NOT NULL,
	sugar      REAL,
	fiber      REAL,
	salt       REAL,
	updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS foods_barcode ON foods(barcode) WHERE barcode IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS foods_source_ref ON foods(source, source_ref) WHERE source_ref <> '';

CREATE TABLE IF NOT EXISTS entries (
	id         INTEGER PRIMARY KEY,
	day        TEXT NOT NULL,
	meal       TEXT NOT NULL,
	food_id    INTEGER NOT NULL REFERENCES foods(id),
	grams      REAL NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS entries_day ON entries(day);

CREATE TABLE IF NOT EXISTS photos (
	id         INTEGER PRIMARY KEY,
	day        TEXT NOT NULL,
	meal       TEXT NOT NULL,
	file       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS photos_day ON photos(day);

CREATE TABLE IF NOT EXISTS measures (
	id   INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	ml   REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS portions (
	id      INTEGER PRIMARY KEY,
	food_id INTEGER NOT NULL REFERENCES foods(id) ON DELETE CASCADE,
	name    TEXT NOT NULL,
	grams   REAL NOT NULL,
	UNIQUE (food_id, name)
);

CREATE TABLE IF NOT EXISTS estimates (
	id            INTEGER PRIMARY KEY,
	day           TEXT NOT NULL,
	meal          TEXT NOT NULL,
	description   TEXT NOT NULL DEFAULT '',
	photo_id      INTEGER,
	model         TEXT NOT NULL,
	answer        TEXT NOT NULL,
	input_tokens  INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL,
	created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS lookups (
	url        TEXT PRIMARY KEY,
	host       TEXT NOT NULL,
	body       BLOB NOT NULL,
	fetched_at TEXT NOT NULL
);
`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.addColumn("foods", "g_per_ml", "REAL"); err != nil {
		return nil, err
	}
	if err := s.addColumn("entries", "estimate_id", "INTEGER REFERENCES estimates(id)"); err != nil {
		return nil, err
	}
	if err := s.migrateUsers(); err != nil {
		return nil, err
	}
	for _, c := range [][3]string{
		{"users", "api_key", "TEXT"}, // sealed by keyBox, never plain
		{"estimates", "cost_usd", "REAL"},
		{"photos", "note", "TEXT NOT NULL DEFAULT ''"},
		{"foods", "sat_fat", "REAL"},
		{"foods", "free_sugar", "REAL"},
		{"users", "sex", "TEXT"},
		{"users", "birth_year", "INTEGER"},
		{"users", "height_cm", "REAL"},
		{"users", "weight_kg", "REAL"},
		{"users", "pal", "REAL"},
		{"users", "kcal_goal", "REAL"},
		{"users", "named", "INTEGER NOT NULL DEFAULT 1"},
		{"estimates", "status", "TEXT NOT NULL DEFAULT 'done'"}, // running | done | failed
		{"estimates", "error", "TEXT NOT NULL DEFAULT ''"},
		{"estimates", "result", "TEXT NOT NULL DEFAULT ''"},
		{"estimates", "key_source", "TEXT"}, // own | shared; NULL before it was recorded
		{"users", "admin", "INTEGER NOT NULL DEFAULT 0"},
		{"users", "last_seen", "TEXT"},
		{"foods", "image_url", "TEXT"}, // NULL unknown, '' the source has none
		{"foods", "image_file", "TEXT"},
		{"foods", "image_fails", "INTEGER NOT NULL DEFAULT 0"},
		{"entries", "portion", "TEXT NOT NULL DEFAULT ''"},
		{"entries", "adjusted", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.addColumn(c[0], c[1], c[2]); err != nil {
			return nil, err
		}
	}
	// A run is a goroutine; a restart ends it without a word, so whatever was
	// running when the process stopped is reported as cut off, not left
	// looking like it is still thinking.
	if _, err := s.db.Exec(`UPDATE estimates SET status='failed', error='interrupted by a restart, please try again' WHERE status='running'`); err != nil {
		return nil, err
	}
	// An estimate never sets a portion, so an estimated entry carrying one was
	// tuned by hand before adjusted was recorded.
	if _, err := s.db.Exec(`UPDATE entries SET adjusted=1 WHERE estimate_id IS NOT NULL AND portion <> '' AND adjusted=0`); err != nil {
		return nil, err
	}
	// The first user is the admin, so a database from before admins existed
	// gets one too.
	if _, err := s.db.Exec(`UPDATE users SET admin=1 WHERE id=(SELECT MIN(id) FROM users)
		AND NOT EXISTS (SELECT 1 FROM users WHERE admin=1)`); err != nil {
		return nil, err
	}
	if err := s.backfillFromLookups(); err != nil {
		return nil, err
	}
	return s, nil
}

// addColumn brings a database created before the column existed up to date.
func (s *Store) addColumn(table, col, def string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

const foodCols = `id, source, source_ref, COALESCE(barcode,''), name, brand, serving_g, g_per_ml,
	COALESCE((SELECT u.name FROM users u WHERE u.id = created_by), ''), image_file IS NOT NULL,
	kcal, protein, carbs, fat, sugar, fiber, salt, sat_fat, free_sugar`

type scanner interface{ Scan(...any) error }

func scanFood(r scanner, f *Food) error {
	return r.Scan(foodDest(f)...)
}

func foodDest(f *Food) []any {
	return []any{&f.ID, &f.Source, &f.SourceRef, &f.Barcode, &f.Name, &f.Brand, &f.ServingG, &f.GPerMl, &f.AddedBy, &f.HasImage,
		&f.Kcal, &f.Protein, &f.Carbs, &f.Fat, &f.Sugar, &f.Fiber, &f.Salt, &f.SatFat, &f.FreeSugar}
}

func (s *Store) FoodByID(id int64) (*Food, error) {
	var f Food
	err := scanFood(s.db.QueryRow(`SELECT `+foodCols+` FROM foods WHERE id = ?`, id), &f)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

func (s *Store) FoodByBarcode(code string) (*Food, error) {
	var f Food
	err := scanFood(s.db.QueryRow(`SELECT `+foodCols+` FROM foods WHERE barcode = ?`, code), &f)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SaveFood inserts, or updates when f.ID is set.
func (s *Store) SaveFood(f *Food, version string) error {
	return s.saveFood(f, version, 0)
}

// saveFood records createdBy on a new row only; an edit keeps the original author.
func (s *Store) saveFood(f *Food, version string, createdBy int64) error {
	args := []any{f.Source, f.SourceRef, version, nullStr(f.Barcode), f.Name, f.Brand, f.ServingG,
		f.Kcal, f.Protein, f.Carbs, f.Fat, f.Sugar, f.Fiber, f.Salt, f.SatFat, f.FreeSugar, now()}
	if f.ID != 0 {
		_, err := s.db.Exec(`UPDATE foods SET source=?, source_ref=?, version=?, barcode=?, name=?, brand=?, serving_g=?,
			kcal=?, protein=?, carbs=?, fat=?, sugar=?, fiber=?, salt=?, sat_fat=?, free_sugar=?, updated_at=? WHERE id=?`, append(args, f.ID)...)
		return err
	}
	res, err := s.db.Exec(`INSERT INTO foods (source, source_ref, version, barcode, name, brand, serving_g,
		kcal, protein, carbs, fat, sugar, fiber, salt, sat_fat, free_sugar, updated_at, created_by, image_url) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		append(args, nullID(createdBy), imageURLArg(f))...)
	if err != nil {
		return err
	}
	f.ID, err = res.LastInsertId()
	return err
}

// RecordLookup keeps the latest raw answer for one URL.
func (s *Store) RecordLookup(host, url string, body []byte) error {
	_, err := s.db.Exec(`INSERT INTO lookups (url, host, body, fetched_at) VALUES (?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET body=excluded.body, fetched_at=excluded.fetched_at`,
		url, host, body, now())
	return err
}

// KeepFood stores a food from a source unless its barcode or source ref is
// already held, so a correction made to an earlier copy is never overwritten.
func (s *Store) KeepFood(f *Food) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO foods (source, source_ref, version, barcode, name, brand, serving_g,
		kcal, protein, carbs, fat, sugar, fiber, salt, sat_fat, free_sugar, updated_at, image_url) VALUES (?,?,'',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.Source, f.SourceRef, nullStr(f.Barcode), f.Name, f.Brand, f.ServingG,
		f.Kcal, f.Protein, f.Carbs, f.Fat, f.Sugar, f.Fiber, f.Salt, f.SatFat, f.FreeSugar, now(), imageURLArg(f))
	return err
}

// UpsertSLV writes one Livsmedelsverket food keyed by its nummer.
// Livsmedelsverket has no serving size, so one on the row was set by hand
// and survives the update.
func (s *Store) UpsertSLV(f *Food, version string) error {
	var id int64
	var serving *float64
	err := s.db.QueryRow(`SELECT id, serving_g FROM foods WHERE source='slv' AND source_ref=?`, f.SourceRef).Scan(&id, &serving)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	f.ID = id
	if f.ServingG == nil {
		f.ServingG = serving
	}
	return s.SaveFood(f, version)
}

// SetGPerMl is kept out of SaveFood so no source update can reach it.
func (s *Store) SetGPerMl(foodID int64, v *float64) error {
	_, err := s.db.Exec(`UPDATE foods SET g_per_ml=? WHERE id=?`, v, foodID)
	return err
}

// Measures lists everyone's, the asking user's first.
func (s *Store) Measures(uid int64) ([]Measure, error) {
	rows, err := s.db.Query(`SELECT m.id, m.name, m.ml, COALESCE(u.name, ''), COALESCE(m.created_by = ?, 0)
		FROM measures m LEFT JOIN users u ON u.id = m.created_by
		ORDER BY COALESCE(m.created_by = ?, 0) DESC, m.ml`, uid, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Measure{}
	for rows.Next() {
		var m Measure
		if err := rows.Scan(&m.ID, &m.Name, &m.Ml, &m.AddedBy, &m.Mine); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveMeasure creates one, or resizes the user's own of that name. Someone
// else's glass of the same name is left alone.
func (s *Store) SaveMeasure(uid int64, m *Measure) error {
	return s.db.QueryRow(`INSERT INTO measures (name, ml, created_by) VALUES (?,?,?)
		ON CONFLICT(created_by, name) DO UPDATE SET ml=excluded.ml RETURNING id`, m.Name, m.Ml, uid).Scan(&m.ID)
}

// DeleteMeasure removes only the user's own; false means it was not theirs.
func (s *Store) DeleteMeasure(uid, id int64) (bool, error) {
	return affected(s.db.Exec(`DELETE FROM measures WHERE id=? AND created_by=?`, id, uid))
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) Portions(uid, foodID int64) ([]Portion, error) {
	rows, err := s.db.Query(`SELECT p.id, p.name, p.grams, COALESCE(u.name, ''), COALESCE(p.created_by = ?, 0)
		FROM portions p LEFT JOIN users u ON u.id = p.created_by
		WHERE p.food_id=? ORDER BY COALESCE(p.created_by = ?, 0) DESC, p.grams`, uid, foodID, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Portion{}
	for rows.Next() {
		var p Portion
		if err := rows.Scan(&p.ID, &p.Name, &p.Grams, &p.AddedBy, &p.Mine); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePortion creates one, or re-weighs the user's own of that name.
func (s *Store) SavePortion(uid, foodID int64, p *Portion) error {
	return s.db.QueryRow(`INSERT INTO portions (food_id, name, grams, created_by) VALUES (?,?,?,?)
		ON CONFLICT(food_id, created_by, name) DO UPDATE SET grams=excluded.grams RETURNING id`, foodID, p.Name, p.Grams, uid).Scan(&p.ID)
}

func (s *Store) DeletePortion(uid, id int64) (bool, error) {
	return affected(s.db.Exec(`DELETE FROM portions WHERE id=? AND created_by=?`, id, uid))
}

type Estimate struct {
	ID          int64
	UserID      int64
	Day, Meal   string
	Description string
}

// cost is stored at the time of the call, so a later price change does not
// rewrite what an estimate cost; nil when the model has no known price.
func (s *Store) AddEstimate(uid int64, day, meal, desc, model string, photoID *int64, answer []byte, u estimateUsage, cost *float64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO estimates (user_id, day, meal, description, photo_id, model, answer, input_tokens, output_tokens, cost_usd, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, uid, day, meal, desc, photoID, model, string(answer), u.InputTokens, u.OutputTokens, cost, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) StartEstimate(uid int64, day, meal, desc, model string, photoID *int64, keySource string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO estimates (user_id, day, meal, description, photo_id, model, answer, input_tokens, output_tokens, status, key_source, created_at)
		VALUES (?,?,?,?,?,?,'',0,0,'running',?,?)`, uid, day, meal, desc, photoID, model, keySource, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishEstimate(id int64, answer []byte, u estimateUsage, cost *float64, result []byte) error {
	_, err := s.db.Exec(`UPDATE estimates SET status='done', answer=?, input_tokens=?, output_tokens=?, cost_usd=?, result=? WHERE id=?`,
		string(answer), u.InputTokens, u.OutputTokens, cost, string(result), id)
	return err
}

func (s *Store) FailEstimate(id int64, msg string, u estimateUsage, cost *float64) error {
	_, err := s.db.Exec(`UPDATE estimates SET status='failed', error=?, input_tokens=?, output_tokens=?, cost_usd=? WHERE id=?`,
		msg, u.InputTokens, u.OutputTokens, cost, id)
	return err
}

type estimateState struct{ Status, Error, Result string }

func (s *Store) EstimateState(uid, id int64) (*estimateState, error) {
	var st estimateState
	err := s.db.QueryRow(`SELECT status, error, result FROM estimates WHERE id=? AND user_id=?`, id, uid).Scan(&st.Status, &st.Error, &st.Result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &st, err
}

func (s *Store) EstimateByID(id int64) (*Estimate, error) {
	e := Estimate{ID: id}
	err := s.db.QueryRow(`SELECT COALESCE(user_id, 0), day, meal, description FROM estimates WHERE id=?`, id).Scan(&e.UserID, &e.Day, &e.Meal, &e.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

// AcceptEstimate writes the reviewed components as ordinary entries in one
// transaction, creating a food for each component the database had no row for.
func (s *Store) AcceptEstimate(est *Estimate, items []proposedItem) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, it := range items {
		f := it.Food
		if f.ID == 0 {
			res, err := tx.Exec(`INSERT INTO foods (source, name, kcal, protein, carbs, fat, updated_at, created_by) VALUES ('estimate',?,?,?,?,?,?,?)`,
				f.Name, f.Kcal, f.Protein, f.Carbs, f.Fat, now(), est.UserID)
			if err != nil {
				return err
			}
			if f.ID, err = res.LastInsertId(); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO entries (user_id, day, meal, food_id, grams, created_at, estimate_id) VALUES (?,?,?,?,?,?,?)`,
			est.UserID, est.Day, est.Meal, f.ID, it.Grams, now(), est.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LastAmount is what the food was last logged at, or 0 if never.
func (s *Store) LastAmount(uid, foodID int64) (grams float64, portion string, err error) {
	err = s.db.QueryRow(`SELECT grams, portion FROM entries WHERE user_id=? AND food_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, uid, foodID).Scan(&grams, &portion)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return grams, portion, err
}

// SLVVersions maps nummer → version for what is already synced.
func (s *Store) SLVVersions() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT source_ref, version FROM foods WHERE source='slv'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var ref, v string
		if err := rows.Scan(&ref, &v); err != nil {
			return nil, err
		}
		out[ref] = v
	}
	return out, rows.Err()
}

// SearchFoods matches every whitespace-separated term case-insensitively.
// SQLite's lower() only folds ASCII, which would make "äpple" miss "Äpple",
// so the match happens here; a few thousand rows is nothing to scan.
// uid ranks by what that user eats; 0 ranks without anyone's history.
func (s *Store) SearchFoods(uid int64, q string, limit int) ([]Food, error) {
	terms := strings.Fields(strings.ToLower(q))
	if len(terms) == 0 {
		return []Food{}, nil
	}
	rows, err := s.db.Query(`SELECT `+foodCols+`,
		(SELECT COUNT(*) FROM entries e WHERE e.food_id = foods.id AND e.user_id = ?) FROM foods`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type hit struct {
		f     Food
		score int
	}
	var hits []hit
	for rows.Next() {
		var f Food
		var uses int
		if err := rows.Scan(append(foodDest(&f), &uses)...); err != nil {
			return nil, err
		}
		hay := strings.ToLower(f.Name + " " + f.Brand)
		ok := true
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		score := uses * 10
		if strings.HasPrefix(hay, terms[0]) {
			score += 5
		}
		// Your own and scanned foods before the reference table. Dabas search
		// hits are kept wholesale, so they earn their place by being eaten.
		if f.Source == "manual" || f.Source == "off" {
			score += 2
		}
		hits = append(hits, hit{f, score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return len(hits[i].f.Name) < len(hits[j].f.Name)
	})
	out := []Food{}
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].f)
	}
	return out, rows.Err()
}

// RecentFoods is what the add sheet shows before anything is typed.
func (s *Store) RecentFoods(uid int64, limit int) ([]Food, error) {
	rows, err := s.db.Query(`SELECT `+foodCols+` FROM foods f
		JOIN (SELECT food_id, MAX(created_at) last FROM entries WHERE user_id = ? GROUP BY food_id) e ON e.food_id = f.id
		ORDER BY e.last DESC LIMIT ?`, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Food{}
	for rows.Next() {
		var f Food
		if err := scanFood(rows, &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) AddEntry(uid int64, day, meal string, foodID int64, grams float64, portion string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO entries (user_id, day, meal, food_id, grams, portion, created_at) VALUES (?,?,?,?,?,?,?)`,
		uid, day, meal, foodID, grams, portion, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateEntry(uid, id int64, meal string, grams float64, portion string) (bool, error) {
	// An estimated amount someone has changed is theirs, not a guess any more;
	// moving it to another meal changes nothing about how much it was.
	return affected(s.db.Exec(`UPDATE entries SET adjusted = adjusted OR grams <> ?2 OR portion <> ?3,
		meal=?1, grams=?2, portion=?3 WHERE id=?4 AND user_id=?5`, meal, grams, portion, id, uid))
}

func (s *Store) DeleteEntry(uid, id int64) (bool, error) {
	return affected(s.db.Exec(`DELETE FROM entries WHERE id=? AND user_id=?`, id, uid))
}

func scale(n Nutrients, grams float64) Nutrients {
	k := grams / 100
	opt := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := *p * k
		return &v
	}
	return Nutrients{Kcal: n.Kcal * k, Protein: n.Protein * k, Carbs: n.Carbs * k, Fat: n.Fat * k,
		Sugar: opt(n.Sugar), Fiber: opt(n.Fiber), Salt: opt(n.Salt), SatFat: opt(n.SatFat), FreeSugar: opt(n.FreeSugar)}
}

func (s *Store) Entries(uid int64, from, to string) ([]Entry, error) {
	rows, err := s.db.Query(`SELECT e.id, e.day, e.meal, e.grams, e.portion, e.created_at, e.estimate_id IS NOT NULL AND NOT e.adjusted, p.id, `+
		strings.ReplaceAll(foodCols, "id, ", "f.id, ")+`
		FROM entries e JOIN foods f ON f.id = e.food_id
		LEFT JOIN estimates x ON x.id = e.estimate_id
		LEFT JOIN photos p ON p.id = x.photo_id AND p.user_id = e.user_id AND p.day = e.day AND p.meal = e.meal
			AND p.created_at <= x.created_at -- a deleted photo's id goes to the next upload
		WHERE e.user_id = ? AND e.day BETWEEN ? AND ? ORDER BY e.day, e.created_at`, uid, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		f := &e.Food
		if err := rows.Scan(append([]any{&e.ID, &e.Day, &e.Meal, &e.Grams, &e.Portion, &e.Created, &e.Estimated, &e.PhotoID}, foodDest(f)...)...); err != nil {
			return nil, err
		}
		e.Totals = scale(f.Nutrients, e.Grams)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) AddPhoto(uid int64, day, meal, file, note string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO photos (user_id, day, meal, file, note, created_at) VALUES (?,?,?,?,?,?)`, uid, day, meal, file, note, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Photos(uid int64, day string) ([]Photo, error) {
	rows, err := s.db.Query(`SELECT id, day, meal, note, created_at FROM photos WHERE user_id=? AND day=? ORDER BY created_at`, uid, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Photo{}
	for rows.Next() {
		var p Photo
		if err := rows.Scan(&p.ID, &p.Day, &p.Meal, &p.Note, &p.Created); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SetPhotoNote(uid, id int64, note string) (bool, error) {
	return affected(s.db.Exec(`UPDATE photos SET note=? WHERE id=? AND user_id=?`, note, id, uid))
}

func (s *Store) PhotoNote(uid, id int64) (string, error) {
	var n string
	err := s.db.QueryRow(`SELECT note FROM photos WHERE id=? AND user_id=?`, id, uid).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return n, err
}

func (s *Store) PhotoFile(uid, id int64) (string, error) {
	var f string
	err := s.db.QueryRow(`SELECT file FROM photos WHERE id=? AND user_id=?`, id, uid).Scan(&f)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return f, err
}

func (s *Store) DeletePhoto(uid, id int64) (string, error) {
	f, err := s.PhotoFile(uid, id)
	if err != nil || f == "" {
		return f, err
	}
	_, err = s.db.Exec(`DELETE FROM photos WHERE id=?`, id)
	return f, err
}
