package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeTeam struct {
	srv  *httptest.Server
	keys map[string]*rsa.PrivateKey
	v    *accessVerifier
}

func newFakeTeam(t *testing.T) *fakeTeam {
	ft := &fakeTeam{keys: map[string]*rsa.PrivateKey{}}
	ft.addKey(t, "k1")
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		for kid, k := range ft.keys {
			set.Keys = append(set.Keys, map[string]string{"kid": kid, "kty": "RSA",
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())})
		}
		json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(ft.srv.Close)
	ft.v = &accessVerifier{issuer: ft.srv.URL, aud: "aud-calories", client: ft.srv.Client(), now: time.Now}
	return ft
}

func (ft *fakeTeam) addKey(t *testing.T, kid string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ft.keys[kid] = k
}

func (ft *fakeTeam) token(kid string, claims map[string]any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	head := enc(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body := enc(claims)
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, ft.keys[kid], crypto.SHA256, sum[:])
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (ft *fakeTeam) claims(email string) map[string]any {
	return map[string]any{"email": email, "iss": ft.srv.URL, "aud": []string{"aud-calories"}, "exp": time.Now().Add(time.Hour).Unix()}
}

func TestAccessTokenIsVerified(t *testing.T) {
	ft := newFakeTeam(t)
	if email, err := ft.v.Email(ft.token("k1", ft.claims("Anna@Example.com"))); err != nil || email != "anna@example.com" {
		t.Fatalf("a valid token: %q %v", email, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"other application": func(c map[string]any) { c["aud"] = "aud-something-else" },
		"other team":        func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" },
		"expired":           func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
	} {
		c := ft.claims("anna@example.com")
		mutate(c)
		if _, err := ft.v.Email(ft.token("k1", c)); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	good := ft.token("k1", ft.claims("anna@example.com"))
	parts := strings.Split(good, ".")
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"johan@example.com"}`)) + "." + parts[2]
	if _, err := ft.v.Email(forged); err == nil {
		t.Error("a changed email must break the signature")
	}
}

func TestAccessPicksUpRotatedKeys(t *testing.T) {
	ft := newFakeTeam(t)
	ft.v.Email(ft.token("k1", ft.claims("a@example.com")))
	ft.addKey(t, "k2")
	ft.v.fetched = time.Now().Add(-2 * time.Minute) // past the refetch hold
	if _, err := ft.v.Email(ft.token("k2", ft.claims("a@example.com"))); err != nil {
		t.Fatalf("a rotated key must be fetched: %v", err)
	}
}

func TestAccessIdentifiesAndCreatesUsers(t *testing.T) {
	ft := newFakeTeam(t)
	store := testStore(t) // has "Test" <test@example.com>
	srv := &Server{store: store, access: ft.v}
	as := func(tok string) (int, map[string]any) {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.Header.Set(accessHeader, tok)
		w := serve(t, srv, req, "-")
		var body map[string]any
		json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}
	if code, me := as(ft.token("k1", ft.claims("TEST@example.com"))); code != 200 || me["name"] != "Test" || me["needsName"] != false {
		t.Fatalf("an existing email must be that user: %d %v", code, me)
	}
	code, me := as(ft.token("k1", ft.claims("anna@example.com")))
	if code != 200 || me["name"] != "anna" || me["needsName"] != true || me["viaAccess"] != true {
		t.Fatalf("a new allowed email must become a user who still needs a name: %d %v", code, me)
	}
	if code, _ := as("not.a.token"); code != 401 {
		t.Fatalf("a bad token must be refused, not fall back to a session: %d", code)
	}
}

func TestCloudflareRequestsNeedAValidAccessToken(t *testing.T) {
	ft := newFakeTeam(t)
	store := testStore(t)
	mux := http.NewServeMux()
	(&Server{store: store, access: ft.v}).routes(mux)
	guarded := (&Server{store: store, access: ft.v}).onlyVerifiedFromCloudflare(mux)
	unconfigured := (&Server{store: store}).onlyVerifiedFromCloudflare(mux)
	get := func(h http.Handler, headers map[string]string) int {
		req := httptest.NewRequest("GET", "/api/users", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	valid := ft.token("k1", ft.claims("test@example.com"))
	for name, c := range map[string]struct {
		h       http.Handler
		headers map[string]string
		want    int
	}{
		"LAN, no token":                     {guarded, nil, 200},
		"Cloudflare, valid token":           {guarded, map[string]string{cloudflareHeader: "203.0.113.9", accessHeader: valid}, 200},
		"Cloudflare, no token":              {guarded, map[string]string{cloudflareHeader: "203.0.113.9"}, 403},
		"Cloudflare, bad token":             {guarded, map[string]string{cloudflareHeader: "203.0.113.9", accessHeader: "not.a.token"}, 403},
		"Cloudflare, Access not configured": {unconfigured, map[string]string{cloudflareHeader: "203.0.113.9", accessHeader: valid}, 403},
		"LAN, Access not configured":        {unconfigured, nil, 200},
	} {
		if got := get(c.h, c.headers); got != c.want {
			t.Errorf("%s: got %d, want %d", name, got, c.want)
		}
	}
}
