package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejson "github.com/go-jose/go-jose/v4/json"
)

const (
	// maxJWKSBytes bounds the key set response; a larger one fails the fetch.
	maxJWKSBytes = 1 << 20
	// maxKeys bounds the keys in a set; more fails the fetch.
	maxKeys = 64
	// minRSABits: smaller RSA keys are ignored. go-jose's JWK parser caps
	// the modulus at jose.MaxRSAModulusBits (4096), left at its default.
	minRSABits = 2048
	// fetchTimeout bounds one key set fetch.
	fetchTimeout = 10 * time.Second
	// refetchCooldown is the least time between two fetches. A token naming
	// an unknown kid within it is refused as ReasonKeyCooldown without a
	// fetch, so a flood of made-up kids costs one fetch per cooldown.
	refetchCooldown = 30 * time.Second
	// refreshInterval: a set older than this is fetched again in the
	// background; until the new set arrives the old one is used.
	refreshInterval = 15 * time.Minute
	// maxKeyAge: a set not refreshed for this long is no longer used (every
	// lookup is KeyFetchFailed), so a withdrawn key dies even if fetches fail.
	maxKeyAge = 24 * time.Hour
	// maxWaiters bounds the lookups waiting on one fetch (as callerauth's
	// maxWaiting); past it a lookup gets the temporary reason at once.
	maxWaiters = 64
)

// key is one usable signing key from the set.
type key struct {
	alg string // the JWK's alg, "" when absent
	pub any    // *rsa.PublicKey, *ecdsa.PublicKey or ed25519.PublicKey
}

var curveAlg = map[elliptic.Curve]string{
	elliptic.P256(): "ES256",
	elliptic.P384(): "ES384",
	elliptic.P521(): "ES512",
}

// fits reports whether k can verify alg: the key type and curve must match,
// and so must the JWK's own alg when it has one.
func (k key) fits(alg string) bool {
	if k.alg != "" && k.alg != alg {
		return false
	}
	switch pub := k.pub.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")
	case *ecdsa.PublicKey:
		return curveAlg[pub.Curve] == alg
	case ed25519.PublicKey:
		return alg == string(jose.EdDSA)
	}
	return false
}

// keySet caches the authorization server's signing keys.
type keySet struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]key
	loaded    time.Time // last successful fetch; zero before the first
	attempted time.Time // start of the last fetch; zero before the first
	flight    *fetch    // the fetch in progress, if any
	// withdrawn: kids a fetched set dropped (at most maxKeys). They are
	// known to be gone, not unpublished yet: UnknownKey at once, no fetch.
	withdrawn map[string]bool
}

type fetch struct {
	done    chan struct{}
	err     error
	waiters int // lookups waiting on it; s.mu
}

// lookup returns the key named kid. An unknown kid, or any kid once the set
// is older than maxKeyAge, fetches the set again unless the last fetch
// started less than refetchCooldown ago; concurrent lookups share one fetch.
func (s *keySet) lookup(ctx context.Context, kid string) (key, error) {
	s.mu.Lock()
	now := s.now()
	cooling := !s.attempted.IsZero() && now.Sub(s.attempted) < refetchCooldown
	current := !s.loaded.IsZero() && now.Sub(s.loaded) < maxKeyAge
	if k, ok := s.keys[kid]; ok && current {
		if s.flight == nil && !cooling && now.Sub(s.loaded) >= refreshInterval {
			s.startLocked()
		}
		s.mu.Unlock()
		return k, nil
	}
	if s.withdrawn[kid] && current {
		s.mu.Unlock()
		return key{}, refuse(ReasonUnknownKey)
	}
	joined := s.flight != nil
	if (!joined && cooling) || (joined && s.flight.waiters >= maxWaiters) {
		err := s.unavailableLocked(now, current)
		s.mu.Unlock()
		return key{}, err
	}
	if !joined {
		s.startLocked()
	}
	f := s.flight
	f.waiters++
	s.mu.Unlock()

	done := false
	select {
	case <-f.done:
		done = true
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f.waiters--
	if !done || f.err != nil {
		return key{}, &Error{Reason: ReasonKeyFetchFailed, RetryAfter: refetchCooldown}
	}
	if k, ok := s.keys[kid]; ok {
		return k, nil
	}
	if joined && !s.withdrawn[kid] { // the fetch began before this lookup, maybe before its key existed
		return key{}, s.unavailableLocked(s.now(), true)
	}
	return key{}, refuse(ReasonUnknownKey)
}

// unavailableLocked: KeyCooldown with a current set, else KeyFetchFailed.
func (s *keySet) unavailableLocked(now time.Time, current bool) error {
	r := ReasonKeyCooldown
	if !current {
		r = ReasonKeyFetchFailed
	}
	return &Error{Reason: r, RetryAfter: max(refetchCooldown-now.Sub(s.attempted), time.Second)}
}

// refresh fetches the set now, or waits for the fetch in progress. A set
// with no usable key is installed (it withdraws every key) and reported.
func (s *keySet) refresh(ctx context.Context) error {
	s.mu.Lock()
	if s.flight == nil {
		s.startLocked()
	}
	f := s.flight
	s.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.err == nil && len(s.keys) == 0 {
		return fetchError("the set has no usable signing key")
	}
	return f.err
}

// startLocked starts a fetch; s.mu is held. The fetch runs on its own
// timeout, not on a caller's context, so a caller giving up does not
// cancel it for the others waiting.
func (s *keySet) startLocked() {
	f := &fetch{done: make(chan struct{})}
	s.flight = f
	s.attempted = s.now()
	go func() {
		keys, err := s.get()
		s.mu.Lock()
		if err == nil {
			s.withdrawn = withdrawnKids(s.withdrawn, s.keys, keys)
			s.keys, s.loaded = keys, s.now()
		}
		f.err = err
		s.flight = nil
		s.mu.Unlock()
		close(f.done)
	}()
}

// withdrawnKids is what next lacks of the old set and of the kids already
// withdrawn, at most maxKeys.
func withdrawnKids(prev map[string]bool, old, next map[string]key) map[string]bool {
	w := map[string]bool{}
	add := func(kid string) {
		if _, ok := next[kid]; !ok && len(w) < maxKeys {
			w[kid] = true
		}
	}
	for kid := range old {
		add(kid)
	}
	for kid := range prev {
		add(kid)
	}
	return w
}

// fetchError is a key set fetch failure with a fixed cause.
type fetchError string

func (e fetchError) Error() string { return "oauth: key set fetch failed: " + string(e) }

func (s *keySet) get() (map[string]key, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fetchError("bad request")
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		var cv *tls.CertificateVerificationError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fetchError("timed out")
		case errors.As(err, &cv):
			return nil, fetchError("TLS certificate not trusted")
		}
		return nil, fetchError("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fetchError(fmt.Sprintf("HTTP status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fetchError("reading the response failed")
	}
	if len(body) > maxJWKSBytes {
		return nil, fetchError("response larger than 1 MiB")
	}
	return parseKeySet(body)
}

// rawKey is one JWK's members. Only public members reach go-jose; a key
// with private or symmetric members is dropped unparsed.
type rawKey struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid"`
	Use    string   `json:"use"`
	Alg    string   `json:"alg"`
	KeyOps []string `json:"key_ops"`
	Crv    string   `json:"crv"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	X      string   `json:"x"`
	Y      string   `json:"y"`

	D    *josejson.RawMessage `json:"d"`
	P    *josejson.RawMessage `json:"p"`
	Q    *josejson.RawMessage `json:"q"`
	DP   *josejson.RawMessage `json:"dp"`
	DQ   *josejson.RawMessage `json:"dq"`
	QI   *josejson.RawMessage `json:"qi"`
	Oth  *josejson.RawMessage `json:"oth"`
	K    *josejson.RawMessage `json:"k"`
	Priv *josejson.RawMessage `json:"priv"`
}

func (r *rawKey) private() bool {
	for _, m := range []*josejson.RawMessage{r.D, r.P, r.Q, r.DP, r.DQ, r.QI, r.Oth, r.K, r.Priv} {
		if m != nil {
			return true
		}
	}
	return false
}

// parseKeySet keeps the keys that can verify a signature: kty RSA (at least
// 2048 bits), EC or OKP Ed25519, with use absent or "sig", key_ops absent or
// holding "verify", a kid, and an alg (if any) on the allowlist. Every other
// key is ignored, as are both keys of a duplicated kid. A JWK set (an object
// with a keys array, RFC 7517 §5) is authoritative even with no usable key:
// it replaces the old one, so {"keys":[]} withdraws every key. Anything else
// ({}, null, {"keys":null}, an error object) is a failure, which keeps the
// old set until maxKeyAge.
func parseKeySet(body []byte) (map[string]key, error) {
	var set struct {
		Keys *[]josejson.RawMessage `json:"keys"`
	}
	if josejson.Unmarshal(body, &set) != nil || set.Keys == nil {
		return nil, fetchError("response is not a JWK set")
	}
	if len(*set.Keys) > maxKeys {
		return nil, fetchError("more than 64 keys")
	}
	keys := map[string]key{}
	dup := map[string]bool{}
	for _, raw := range *set.Keys {
		k, kid, ok := parseKey(raw)
		if !ok {
			continue
		}
		if _, seen := keys[kid]; seen || dup[kid] {
			dup[kid] = true
			delete(keys, kid)
			continue
		}
		keys[kid] = k
	}
	return keys, nil
}

func parseKey(raw []byte) (key, string, bool) {
	var r rawKey
	if josejson.Unmarshal(raw, &r) != nil || r.Kid == "" || r.private() {
		return key{}, "", false
	}
	if (r.Use != "" && r.Use != "sig") || (r.KeyOps != nil && !slices.Contains(r.KeyOps, "verify")) {
		return key{}, "", false
	}
	if r.Alg != "" && !allowedAlgorithm(r.Alg) {
		return key{}, "", false
	}
	pub := map[string]string{"kty": r.Kty}
	switch r.Kty {
	case "RSA":
		pub["n"], pub["e"] = r.N, r.E
	case "EC":
		pub["crv"], pub["x"], pub["y"] = r.Crv, r.X, r.Y
	case "OKP":
		if r.Crv != "Ed25519" {
			return key{}, "", false
		}
		pub["crv"], pub["x"] = r.Crv, r.X
	default:
		return key{}, "", false
	}
	b, err := josejson.Marshal(pub)
	if err != nil {
		return key{}, "", false
	}
	var jwk jose.JSONWebKey
	if jwk.UnmarshalJSON(b) != nil || !jwk.Valid() || !jwk.IsPublic() {
		return key{}, "", false
	}
	if rk, ok := jwk.Key.(*rsa.PublicKey); ok && rk.N.BitLen() < minRSABits {
		return key{}, "", false
	}
	return key{alg: r.Alg, pub: jwk.Key}, r.Kid, true
}
