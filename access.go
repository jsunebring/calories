package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Cloudflare Access puts a signed token on every request that came through
// the tunnel. Its email is who the person is; the signature, audience and
// issuer are checked, so the header cannot be forged by anyone who reaches
// the app another way (the LAN port, which keeps the name picker).
type accessVerifier struct {
	issuer string // https://<team>.cloudflareaccess.com
	aud    string // the Access application's audience tag
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func newAccessVerifier(team, aud string) *accessVerifier {
	team = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(team), "https://"), "/")
	if !strings.Contains(team, ".") {
		team += ".cloudflareaccess.com"
	}
	return &accessVerifier{issuer: "https://" + team, aud: strings.TrimSpace(aud), client: httpClient, now: time.Now}
}

const accessHeader = "Cf-Access-Jwt-Assertion"

func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Email verifies the token and returns the signed-in email.
func (v *accessVerifier) Email(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT")
	}
	var head struct{ Alg, Kid string }
	if raw, err := b64(parts[0]); err != nil || json.Unmarshal(raw, &head) != nil {
		return "", errors.New("bad header")
	}
	if head.Alg != "RS256" {
		return "", fmt.Errorf("unexpected alg %q", head.Alg)
	}
	key, err := v.key(head.Kid)
	if err != nil {
		return "", err
	}
	sig, err := b64(parts[2])
	if err != nil {
		return "", errors.New("bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("bad signature")
	}
	var c struct {
		Email string          `json:"email"`
		Iss   string          `json:"iss"`
		Aud   json.RawMessage `json:"aud"`
		Exp   int64           `json:"exp"`
		Nbf   int64           `json:"nbf"`
	}
	raw, err := b64(parts[1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return "", errors.New("bad claims")
	}
	now := v.now().Unix()
	switch {
	case c.Iss != v.issuer:
		return "", fmt.Errorf("issuer %q is not this team", c.Iss)
	case !audHas(c.Aud, v.aud):
		return "", errors.New("token is for another application")
	case c.Exp != 0 && now > c.Exp+30:
		return "", errors.New("token expired")
	case c.Nbf != 0 && now < c.Nbf-30:
		return "", errors.New("token not valid yet")
	case c.Email == "":
		return "", errors.New("token has no email")
	}
	return normEmail(c.Email), nil
}

// The audience claim may be a string or a list.
func audHas(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	json.Unmarshal(raw, &many)
	for _, a := range many {
		if a == want {
			return true
		}
	}
	return false
}

// key returns the signing key, refetching the team's keys when one is
// unknown: Cloudflare rotates them, and a new kid is how that shows. A
// refetch is held to once a minute so a stream of junk tokens cannot turn
// into a stream of requests.
func (v *accessVerifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if v.now().Sub(v.fetched) < time.Minute {
		return nil, errors.New("unknown signing key")
	}
	v.fetched = v.now()
	keys, err := v.fetchKeys()
	if err != nil {
		return nil, err
	}
	v.keys = keys
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

func (v *accessVerifier) fetchKeys() (map[string]*rsa.PublicKey, error) {
	resp, err := v.client.Get(v.issuer + "/cdn-cgi/access/certs")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("access certs: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct{ Kid, Kty, N, E string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("access certs: %w", err)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := b64(k.N)
		e, err2 := b64(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(out) == 0 {
		return nil, errors.New("access certs: no RSA keys")
	}
	return out, nil
}

// Cloudflare sets Cf-Connecting-IP on every request it forwards, so its
// presence means the request came from the internet, not the LAN.
const cloudflareHeader = "Cf-Connecting-IP"

// onlyVerifiedFromCloudflare closes the LAN fallback to the internet. The name
// picker is fine on a home network, but a request that came through
// Cloudflare without a valid Access token means Access is not covering it
// (a bypass rule, a removed application, a new hostname), and the picker
// would then be a login anyone could pass. The check fails closed: such a
// request is refused even when Access is not configured here at all. A LAN
// client sending the header itself only gets itself refused.
func (s *Server) onlyVerifiedFromCloudflare(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(cloudflareHeader) != "" {
			if s.access == nil {
				fail(w, http.StatusForbidden, "reached through Cloudflare, but CF_ACCESS_TEAM and CF_ACCESS_AUD are not set")
				return
			}
			if _, err := s.access.Email(r.Header.Get(accessHeader)); err != nil {
				fail(w, http.StatusForbidden, "reached through Cloudflare without a valid Access sign-in")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
