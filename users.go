package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Users tell people apart; they are not a security boundary. Who can reach
// the app at all is decided in front of it (caddy, the tailnet). Picking a
// name and typing its email only stops one person logging into another's day
// by accident.

type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// NeedsName: created on a first visit through Cloudflare Access, still
	// called by the start of their email until they pick a name.
	NeedsName bool `json:"needsName"`
	// ViaAccess: identified by Cloudflare on this request, so there is no
	// session to switch away from.
	ViaAccess bool `json:"viaAccess"`
	Admin     bool `json:"admin"`
}

// migrateUsers moves a single-user database to per-user rows. Rows that
// existed before are left unowned and claimed by the first user added.
func (s *Store) migrateUsers() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= 1 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX users_name ON users(name COLLATE NOCASE)`,
		`CREATE TABLE sessions (token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, created_at TEXT NOT NULL)`,
		`ALTER TABLE entries ADD COLUMN user_id INTEGER REFERENCES users(id)`,
		`ALTER TABLE photos ADD COLUMN user_id INTEGER REFERENCES users(id)`,
		`ALTER TABLE estimates ADD COLUMN user_id INTEGER REFERENCES users(id)`,
		`ALTER TABLE foods ADD COLUMN created_by INTEGER REFERENCES users(id)`,
		`CREATE INDEX entries_user_day ON entries(user_id, day)`,
		`CREATE INDEX photos_user_day ON photos(user_id, day)`,
		// A glass or portion name is unique per person, so two people can each
		// have a "Large glass" without one saving over the other's.
		`CREATE TABLE measures_v1 (id INTEGER PRIMARY KEY, name TEXT NOT NULL, ml REAL NOT NULL,
			created_by INTEGER REFERENCES users(id), UNIQUE (created_by, name))`,
		`INSERT INTO measures_v1 (id, name, ml) SELECT id, name, ml FROM measures`,
		`DROP TABLE measures`,
		`ALTER TABLE measures_v1 RENAME TO measures`,
		`CREATE TABLE portions_v1 (id INTEGER PRIMARY KEY, food_id INTEGER NOT NULL REFERENCES foods(id) ON DELETE CASCADE,
			name TEXT NOT NULL, grams REAL NOT NULL, created_by INTEGER REFERENCES users(id), UNIQUE (food_id, created_by, name))`,
		`INSERT INTO portions_v1 (id, food_id, name, grams) SELECT id, food_id, name, grams FROM portions`,
		`DROP TABLE portions`,
		`ALTER TABLE portions_v1 RENAME TO portions`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrate users: %w (%s)", err, strings.Fields(q)[0:3])
		}
	}
	return tx.Commit()
}

type userSettings struct {
	SealedKey string
}

func (s *Store) UserSettings(uid int64) (userSettings, error) {
	var u userSettings
	err := s.db.QueryRow(`SELECT COALESCE(api_key, '') FROM users WHERE id=?`, uid).Scan(&u.SealedKey)
	return u, err
}

func (s *Store) SetUserKey(uid int64, sealed string) error {
	_, err := s.db.Exec(`UPDATE users SET api_key=? WHERE id=?`, nullStr(sealed), uid)
	return err
}

// Spent sums what the user's estimates cost since a UTC RFC 3339 instant.
func (s *Store) Spent(uid int64, since string) (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM estimates WHERE user_id=? AND created_at >= ?`, uid, since).Scan(&v)
	return v, err
}

// spendWindows are the starts of today and of this week (Monday), local time.
func spendWindows(t time.Time) (day, week string) {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	back := (int(d.Weekday()) + 6) % 7
	return d.UTC().Format(time.RFC3339), d.AddDate(0, 0, -back).UTC().Format(time.RFC3339)
}

type spend struct {
	Today float64 `json:"today"`
	Week  float64 `json:"week"`
}

func (s *Store) SpendNow(uid int64) (spend, error) {
	d, w := spendWindows(time.Now())
	today, err := s.Spent(uid, d)
	if err != nil {
		return spend{}, err
	}
	week, err := s.Spent(uid, w)
	return spend{today, week}, err
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// AddUser creates a user. The first one also takes over every row written
// before users existed, which is the single-user history.
func (s *Store) AddUser(name, email string) (*User, error) {
	name, email = strings.TrimSpace(name), normEmail(email)
	if name == "" || !strings.Contains(email, "@") {
		return nil, errors.New("need a name and an email")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var first bool
	if err := tx.QueryRow(`SELECT NOT EXISTS (SELECT 1 FROM users)`).Scan(&first); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO users (name, email, created_at) VALUES (?,?,?)`, name, email, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if first {
		for _, q := range []string{
			`UPDATE users SET admin=1 WHERE id=?`,
			`UPDATE entries SET user_id=? WHERE user_id IS NULL`,
			`UPDATE photos SET user_id=? WHERE user_id IS NULL`,
			`UPDATE estimates SET user_id=? WHERE user_id IS NULL`,
			`UPDATE measures SET created_by=? WHERE created_by IS NULL`,
			`UPDATE portions SET created_by=? WHERE created_by IS NULL`,
			`UPDATE foods SET created_by=? WHERE created_by IS NULL AND source IN ('manual','estimate')`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return nil, err
			}
		}
	}
	return &User{ID: id, Name: name}, tx.Commit()
}

// UserForEmail finds the user with this email, creating one on a first visit.
// Cloudflare's allow-list decides who gets this far, so it is the one list of
// people; the app does not keep a second one to fall out of step with it.
func (s *Store) UserForEmail(email string) (*User, error) {
	email = normEmail(email)
	var u User
	var named bool
	err := s.db.QueryRow(`SELECT id, name, named, admin FROM users WHERE email=?`, email).Scan(&u.ID, &u.Name, &named, &u.Admin)
	if err == nil {
		u.NeedsName = !named
		return &u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	base := strings.SplitN(email, "@", 2)[0]
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s %d", base, i)
		}
		created, err := s.AddUser(name, email)
		if err == nil {
			if _, err := s.db.Exec(`UPDATE users SET named=0 WHERE id=?`, created.ID); err != nil {
				return nil, err
			}
			created.NeedsName = true
			return created, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") || i > 50 {
			return nil, err
		}
	}
}

// Rename sets a display name; false means another user already has it.
func (s *Store) Rename(uid int64, name string) (bool, error) {
	_, err := s.db.Exec(`UPDATE users SET name=?, named=1 WHERE id=?`, strings.TrimSpace(name), uid)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, name FROM users ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// NewSession checks the email against the picked user and returns a token.
func (s *Store) NewSession(userID int64, email string) (string, *User, error) {
	var u User
	var want string
	err := s.db.QueryRow(`SELECT id, name, email FROM users WHERE id=?`, userID).Scan(&u.ID, &u.Name, &want)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && want != normEmail(email)) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	b := make([]byte, 24)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	_, err = s.db.Exec(`INSERT INTO sessions (token, user_id, created_at) VALUES (?,?,?)`, tok, u.ID, now())
	return tok, &u, err
}

func (s *Store) SessionUser(token string) (*User, error) {
	var u User
	err := s.db.QueryRow(`SELECT u.id, u.name, u.admin FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token=?`, token).Scan(&u.ID, &u.Name, &u.Admin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

func (s *Store) EndSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token=?`, token)
	return err
}

const sessionCookie = "calorie_user"

type userKey struct{}

func currentUser(r *http.Request) *User { return r.Context().Value(userKey{}).(*User) }

// authed identifies the user: by Cloudflare Access when the request came
// through the tunnel, otherwise by the picked-name session (the LAN port).
// It answers 401 until one of them says who this is.
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok := r.Header.Get(accessHeader); tok != "" && s.access != nil {
			email, err := s.access.Email(tok)
			if err != nil {
				fail(w, http.StatusUnauthorized, "the Cloudflare sign-in could not be verified: "+err.Error())
				return
			}
			u, err := s.store.UserForEmail(email)
			if err != nil {
				serverError(w, err)
				return
			}
			u.ViaAccess = true
			s.store.Seen(u.ID)
			h(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			fail(w, http.StatusUnauthorized, "pick a user")
			return
		}
		u, err := s.store.SessionUser(c.Value)
		if err != nil {
			serverError(w, err)
			return
		}
		if u == nil {
			fail(w, http.StatusUnauthorized, "pick a user")
			return
		}
		s.store.Seen(u.ID)
		h(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	}
}

// Seen records activity, at most once a minute per user so a page load's
// burst of requests is one write.
func (s *Store) Seen(uid int64) {
	s.db.Exec(`UPDATE users SET last_seen=? WHERE id=? AND (last_seen IS NULL OR last_seen < ?)`,
		now(), uid, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
}

type Person struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Email        string  `json:"email"`
	Admin        bool    `json:"admin"`
	LastSeen     *string `json:"lastSeen"`
	Days7        int     `json:"days7"`
	Entries7     int     `json:"entries7"`
	Estimates    int     `json:"estimatesMonth"`
	CostOwn      float64 `json:"costOwnMonth"`
	CostShared   float64 `json:"costSharedMonth"`
	CostUnknown  float64 `json:"costUnknownMonth"` // made before the key used was recorded
	HasOwnAPIKey bool    `json:"hasOwnKey"`
}

// People is the admin's overview: who uses the app and whose key pays for
// their estimates this calendar month.
func (s *Store) People(today string) ([]Person, error) {
	d, _ := time.Parse("2006-01-02", today)
	from7 := d.AddDate(0, 0, -6).Format("2006-01-02")
	month := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT u.id, u.name, u.email, u.admin, u.last_seen, COALESCE(u.api_key,'') <> '',
		(SELECT COUNT(DISTINCT day) FROM entries e WHERE e.user_id=u.id AND e.day BETWEEN ? AND ?),
		(SELECT COUNT(*) FROM entries e WHERE e.user_id=u.id AND e.day BETWEEN ? AND ?),
		(SELECT COUNT(*) FROM estimates x WHERE x.user_id=u.id AND x.created_at >= ?),
		(SELECT COALESCE(SUM(cost_usd),0) FROM estimates x WHERE x.user_id=u.id AND x.created_at >= ? AND x.key_source='own'),
		(SELECT COALESCE(SUM(cost_usd),0) FROM estimates x WHERE x.user_id=u.id AND x.created_at >= ? AND x.key_source='shared'),
		(SELECT COALESCE(SUM(cost_usd),0) FROM estimates x WHERE x.user_id=u.id AND x.created_at >= ? AND x.key_source IS NULL)
		FROM users u ORDER BY u.last_seen IS NULL, u.last_seen DESC`,
		from7, today, from7, today, month, month, month, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name, &p.Email, &p.Admin, &p.LastSeen, &p.HasOwnAPIKey, &p.Days7, &p.Entries7,
			&p.Estimates, &p.CostOwn, &p.CostShared, &p.CostUnknown); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) people(w http.ResponseWriter, r *http.Request) {
	if !currentUser(r).Admin {
		fail(w, http.StatusForbidden, "only the admin sees this")
		return
	}
	day := r.URL.Query().Get("day")
	if !validDay(day) {
		day = time.Now().Format("2006-01-02")
	}
	p, err := s.store.People(day)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"people": p})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Users()
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"users": users})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, currentUser(r)) }

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Name) == "" {
		fail(w, 400, "a name, please")
		return
	}
	ok, err := s.store.Rename(currentUser(r).ID, req.Name)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		fail(w, http.StatusConflict, "someone else already has that name")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// getSettings never returns the key itself, only whether one is usable.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	st, err := s.store.UserSettings(uid)
	if err != nil {
		serverError(w, err)
		return
	}
	sp, err := s.store.SpendNow(uid)
	if err != nil {
		serverError(w, err)
		return
	}
	keyState := "none"
	if st.SealedKey != "" {
		keyState = "set"
		if s.keys == nil {
			keyState = "unreadable"
		} else if _, err := s.keys.open(st.SealedKey); err != nil {
			keyState = "unreadable"
		}
	}
	writeJSON(w, 200, map[string]any{
		"key": keyState, "sharedKey": s.sharedKey != "", "canStoreKeys": s.keys != nil,
		"spent": sp, "model": s.model,
	})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	var req struct {
		APIKey *string `json:"apiKey"` // absent: keep; "": remove
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		fail(w, 400, "bad json")
		return
	}
	if req.APIKey != nil {
		k := strings.TrimSpace(*req.APIKey)
		sealed := ""
		if k != "" {
			if s.keys == nil {
				fail(w, http.StatusServiceUnavailable, "keys cannot be stored until KEY_SECRET is set on the container")
				return
			}
			sealed = s.keys.seal(k)
		}
		if err := s.store.SetUserKey(uid, sealed); err != nil {
			serverError(w, err)
			return
		}
	}
	s.getSettings(w, r)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID int64  `json:"userId"`
		Email  string `json:"email"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		fail(w, 400, "bad json")
		return
	}
	tok, u, err := s.store.NewSession(req.UserID, req.Email)
	if err != nil {
		serverError(w, err)
		return
	}
	if u == nil {
		fail(w, http.StatusUnauthorized, "that email does not match")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", MaxAge: 400 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	writeJSON(w, 200, u)
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.EndSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// userCommand is the admin side: users are added from a shell on Tower,
//
//	docker exec calorie-log calorie-log user add "Johan" johan@example.com
func userCommand(store *Store, args []string, out io.Writer) error {
	switch {
	case len(args) == 3 && args[0] == "add":
		u, err := store.AddUser(args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "added %s (id %d)\n", u.Name, u.ID)
	case len(args) == 1 && args[0] == "list":
		rows, err := store.db.Query(`SELECT id, name, email FROM users ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var name, email string
			rows.Scan(&id, &name, &email)
			fmt.Fprintf(out, "%d\t%s\t%s\n", id, name, email)
		}
	case len(args) == 2 && args[0] == "admin":
		res, err := store.db.Exec(`UPDATE users SET admin=1 WHERE name=? COLLATE NOCASE`, args[1])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no user named %q", args[1])
		}
		fmt.Fprintln(out, "admin")
	case len(args) == 3 && args[0] == "email":
		res, err := store.db.Exec(`UPDATE users SET email=? WHERE name=? COLLATE NOCASE`, normEmail(args[2]), args[1])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no user named %q", args[1])
		}
		fmt.Fprintln(out, "updated")
	default:
		return errors.New(`usage: user add <name> <email> | user list | user email <name> <new email> | user admin <name>`)
	}
	return nil
}
