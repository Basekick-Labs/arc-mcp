package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testIssuer   = "https://idp.example/realms/arc"
	testAudience = "https://mcp.example/mcp"
)

var (
	keysOnce                   sync.Once
	rsaKey, rsaOther, rsaSmall *rsa.PrivateKey
	ecKey, ec384Key            *ecdsa.PrivateKey
	edKey                      ed25519.PrivateKey
	start                      = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	b64                        = base64.RawURLEncoding.EncodeToString
)

// genKeys makes the test keys once per test binary (RSA generation is slow
// under -race).
func genKeys() {
	keysOnce.Do(func() {
		must := func(err error) {
			if err != nil {
				panic(err)
			}
		}
		var err error
		rsaKey, err = rsa.GenerateKey(rand.Reader, 2048)
		must(err)
		rsaOther, err = rsa.GenerateKey(rand.Reader, 2048)
		must(err)
		rsaSmall, err = rsa.GenerateKey(rand.Reader, 1024)
		must(err)
		ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		must(err)
		ec384Key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		must(err)
		_, edKey, err = ed25519.GenerateKey(rand.Reader)
		must(err)
	})
}

// clock is a fake clock safe for concurrent use.
type clock struct{ ns atomic.Int64 }

func (c *clock) now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) add(d time.Duration) { c.ns.Add(int64(d)) }
func jwk(pub any, kid, alg string) any {
	return jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: alg, Use: "sig"}
}

// idp serves a JWKS on loopback and counts fetches.
type idp struct {
	srv      *httptest.Server
	body     atomic.Value // []byte
	location atomic.Value // string: a Location header, for redirects
	status   atomic.Int64
	hits     atomic.Int64
}

func newIDP(t testing.TB, keys ...any) *idp {
	p := &idp{}
	p.status.Store(http.StatusOK)
	p.serve(t, keys...)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		if loc, _ := p.location.Load().(string); loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(int(p.status.Load()))
		_, _ = w.Write(p.body.Load().([]byte))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) serve(t testing.TB, keys ...any) {
	b, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	p.body.Store(b)
}

func defaultKeys() []any {
	return []any{
		jwk(&rsaKey.PublicKey, "rsa", ""), jwk(&rsaKey.PublicKey, "rs512", "RS512"),
		jwk(&ecKey.PublicKey, "ec", ""), jwk(&ec384Key.PublicKey, "ec384", "ES384"),
		jwk(edKey.Public(), "ed", ""),
	}
}

type rig struct {
	v      *Verifier
	clk    *clock
	idp    *idp
	parses atomic.Int64
}

func newRig(t testing.TB, cfg Config, keys ...any) *rig {
	genKeys()
	if keys == nil {
		keys = defaultKeys()
	}
	r := &rig{clk: &clock{}, idp: newIDP(t, keys...)}
	r.clk.ns.Store(start.UnixNano())
	if cfg.Issuer == "" {
		cfg.Issuer, cfg.Audience = testIssuer, testAudience
	}
	cfg.JWKSURL = r.idp.srv.URL + "/jwks"
	v, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	v.now, v.keys.now = r.clk.now, r.clk.now
	v.parse = func(s string, a []jose.SignatureAlgorithm) (*jose.JSONWebSignature, error) {
		r.parses.Add(1)
		return jose.ParseSignedCompact(s, a)
	}
	r.v = v
	return r
}

func (r *rig) verify(tok string) (*Token, error) { return r.v.Verify(context.Background(), tok) }

// waitFlight waits for a background fetch to finish.
func (r *rig) waitFlight() {
	r.v.keys.mu.Lock()
	f := r.v.keys.flight
	r.v.keys.mu.Unlock()
	if f != nil {
		<-f.done
	}
}

func payload(now time.Time, edit ...func(map[string]any)) map[string]any {
	c := map[string]any{
		"iss": testIssuer, "sub": "user-1", "aud": testAudience, "azp": "mcp-client",
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(), "nbf": now.Unix(), "scope": "openid arc:read",
	}
	for _, e := range edit {
		e(c)
	}
	return c
}

func set(k string, v any) func(map[string]any) { return func(c map[string]any) { c[k] = v } }
func del(k string) func(map[string]any)        { return func(c map[string]any) { delete(c, k) } }

// sign makes a compact JWS; extra header members are added as given.
func sign(t testing.TB, priv any, alg jose.SignatureAlgorithm, kid, typ string, c map[string]any, extra ...string) string {
	opts := (&jose.SignerOptions{}).WithHeader("kid", kid)
	if typ != "" {
		opts = opts.WithType(jose.ContentType(typ))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		opts = opts.WithHeader(jose.HeaderKey(extra[i]), extra[i+1])
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: priv}, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	jws, err := s.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// unsigned builds a token from a literal header; the signature is junk.
func unsigned(hdr string, c map[string]any) string {
	b, _ := json.Marshal(c)
	return b64([]byte(hdr)) + "." + b64(b) + "." + b64([]byte("not-a-signature"))
}

func checkRefusal(t *testing.T, err error, want Reason) {
	t.Helper()
	if got := ReasonOf(err); got != want {
		t.Fatalf("reason = %v (%v), want %v", got, err, want)
	}
	if err.Error() != "oauth: token refused: "+want.String() || errors.Unwrap(err) != nil {
		t.Fatalf("error text %q is not the fixed text for %v, or it wraps", err, want)
	}
}

func TestVerify(t *testing.T) {
	r := newRig(t, Config{})
	now := start
	jwkHdr, _ := json.Marshal(jwk(&rsaOther.PublicKey, "MARKER", ""))
	hmacKey, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	cases := []struct {
		name    string
		tok     func() string
		want    Reason // 0: accepted
		noParse bool   // go-jose must not have been called
	}{
		{"RS256 at+jwt", func() string { return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now)) }, 0, false},
		{"PS256 application/at+jwt", func() string { return sign(t, rsaKey, jose.PS256, "rsa", "application/at+jwt", payload(now)) }, 0, false},
		{"ES256 AT+JWT, aud array", func() string {
			return sign(t, ecKey, jose.ES256, "ec", "AT+JWT", payload(now, set("aud", []string{"other", testAudience})))
		}, 0, false},
		{"ES384", func() string { return sign(t, ec384Key, jose.ES384, "ec384", "at+jwt", payload(now)) }, 0, false},
		{"EdDSA", func() string { return sign(t, edKey, jose.EdDSA, "ed", "at+jwt", payload(now)) }, 0, false},
		{"RS512 on a key pinned to RS512, x5t kept", func() string {
			return sign(t, rsaKey, jose.RS512, "rs512", "at+jwt", payload(now), "x5t", "dGh1bWI")
		}, 0, false},
		{"exp inside the leeway", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("exp", now.Add(-Leeway+time.Second).Unix())))
		}, 0, false},
		{"expired at the leeway edge", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("exp", now.Add(-Leeway).Unix())))
		}, ReasonExpired, false},
		{"nbf at the leeway edge", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("nbf", now.Add(Leeway).Unix())))
		}, 0, false},
		{"nbf in the future", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("nbf", now.Add(Leeway+time.Second).Unix())))
		}, ReasonNotYetValid, false},
		{"iat in the future", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("iat", now.Add(Leeway+time.Second).Unix())))
		}, ReasonNotYetValid, false},
		{"issuer with a trailing slash", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("iss", testIssuer+"/")))
		}, ReasonWrongIssuer, true},
		{"wrong audience", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("aud", "mcp-client")))
		}, ReasonWrongAudience, false},
		{"no aud", func() string { return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, del("aud"))) }, ReasonWrongAudience, false},
		{"no exp", func() string { return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, del("exp"))) }, ReasonMalformed, false},
		{"exp not a number", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("exp", "soon")))
		}, ReasonMalformed, false},
		{"no sub", func() string { return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, del("sub"))) }, ReasonMalformed, false},
		{"scope not a string", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("scope", []string{"arc"})))
		}, ReasonMalformed, false},
		{"bad signature", func() string { return sign(t, rsaOther, jose.RS256, "rsa", "at+jwt", payload(now)) }, ReasonBadSignature, false},
		{"alg none", func() string { return unsigned(`{"alg":"none","kid":"rsa","typ":"at+jwt"}`, payload(now)) }, ReasonMalformed, true},
		{"HS256 keyed with the public key", func() string {
			return sign(t, hmacKey, jose.HS256, "rsa", "at+jwt", payload(now))
		}, ReasonMalformed, true},
		{"ES256 naming an RSA key", func() string { return sign(t, ecKey, jose.ES256, "rsa", "at+jwt", payload(now)) }, ReasonUnknownKey, true},
		{"ES384 naming a P-256 key", func() string { return sign(t, ec384Key, jose.ES384, "ec", "at+jwt", payload(now)) }, ReasonUnknownKey, true},
		{"RS256 naming a key pinned to RS512", func() string {
			return sign(t, rsaKey, jose.RS256, "rs512", "at+jwt", payload(now))
		}, ReasonUnknownKey, true},
		{"no kid", func() string { return unsigned(`{"alg":"RS256","typ":"at+jwt"}`, payload(now)) }, ReasonUnknownKey, true},
		{"embedded jwk", func() string {
			return unsigned(`{"alg":"RS256","kid":"rsa","jwk":`+string(jwkHdr)+`}`, payload(now))
		}, ReasonMalformed, true},
		{"jku", func() string { return unsigned(`{"alg":"RS256","kid":"rsa","jku":"`+r.idp.srv.URL+`"}`, payload(now)) }, ReasonMalformed, true},
		{"x5u", func() string {
			return unsigned(`{"alg":"RS256","kid":"rsa","x5u":"https://x.example/c"}`, payload(now))
		}, ReasonMalformed, true},
		{"x5c", func() string { return unsigned(`{"alg":"RS256","kid":"rsa","x5c":["MIIB"]}`, payload(now)) }, ReasonMalformed, true},
		{"jwk null", func() string { return unsigned(`{"alg":"RS256","kid":"rsa","jwk":null}`, payload(now)) }, ReasonMalformed, true},
		{"crit", func() string { return unsigned(`{"alg":"RS256","kid":"rsa","crit":["exp"]}`, payload(now)) }, ReasonMalformed, true},
		{"b64", func() string { return unsigned(`{"alg":"RS256","kid":"rsa","b64":false}`, payload(now)) }, ReasonMalformed, true},
		{"duplicate alg", func() string { return unsigned(`{"alg":"none","kid":"rsa","alg":"RS256"}`, payload(now)) }, ReasonMalformed, true},
		{"oversized", func() string {
			return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now, set("pad", strings.Repeat("a", MaxTokenBytes))))
		}, ReasonMalformed, true},
		{"not base64url", func() string { return sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(now)) + "=" }, ReasonMalformed, true},
		{"typ JWT, no rule configured", func() string { return sign(t, rsaKey, jose.RS256, "rsa", "JWT", payload(now)) }, ReasonNotAccessToken, false},
		{"no typ, no rule configured", func() string { return sign(t, rsaKey, jose.RS256, "rsa", "", payload(now)) }, ReasonNotAccessToken, false},
		{"empty", func() string { return "" }, ReasonMissing, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := tc.tok()
			before := r.parses.Load()
			got, err := r.verify(tok)
			if tc.noParse && r.parses.Load() != before {
				t.Fatal("go-jose parsed a token the header checks must refuse first")
			}
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got.Subject != "user-1" || got.ClientID != "mcp-client" || len(got.Scopes) != 2 {
					t.Fatalf("token = %+v", got)
				}
				return
			}
			checkRefusal(t, err, tc.want)
			if strings.Contains(err.Error(), "MARKER") {
				t.Fatal("refusal echoes the token")
			}
		})
	}
	if n := r.idp.hits.Load(); n != 1 {
		t.Fatalf("key set fetched %d times, want 1 (no refusal above may refetch)", n)
	}
}

func TestUntypedAccessTokenRule(t *testing.T) {
	var asked atomic.Int64
	// A candidate rule of the kind the oauth mode will set: an ID token
	// carries nonce/at_hash, and Keycloak marks its tokens with a typ claim.
	rule := func(tok *Token) bool {
		asked.Add(1)
		typ, _ := tok.StringClaim("typ")
		return typ == "Bearer" && !tok.HasClaim("nonce") && !tok.HasClaim("at_hash")
	}
	r := newRig(t, Config{Issuer: testIssuer, Audience: testAudience, UntypedAccessToken: rule})
	access := payload(start, set("typ", "Bearer"))
	idToken := payload(start, set("typ", "ID"), set("nonce", "n-0S6"), set("at_hash", "77QmUP"), del("scope"))
	if _, err := r.verify(sign(t, rsaKey, jose.RS256, "rsa", "JWT", access)); err != nil {
		t.Fatalf("untyped access token refused: %v", err)
	}
	_, err := r.verify(sign(t, rsaKey, jose.RS256, "rsa", "JWT", idToken))
	checkRefusal(t, err, ReasonNotAccessToken)
	if asked.Load() != 2 {
		t.Fatalf("rule asked %d times, want 2", asked.Load())
	}
	if _, err := r.verify(sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", idToken)); err != nil {
		t.Fatalf("at+jwt refused: %v", err)
	}
	_, err = r.verify(sign(t, rsaKey, jose.RS256, "rsa", "dpop+jwt", access))
	checkRefusal(t, err, ReasonNotAccessToken)
	if asked.Load() != 2 {
		t.Fatal("the rule was asked about a typed token")
	}
}

func TestForeignIssuerFetchesNothing(t *testing.T) {
	r := newRig(t, Config{})
	_, err := r.verify(sign(t, rsaKey, jose.RS256, "random-kid", "at+jwt", payload(start, set("iss", "https://other.example"))))
	checkRefusal(t, err, ReasonWrongIssuer)
	if r.idp.hits.Load() != 0 || r.parses.Load() != 0 {
		t.Fatal("a token from another issuer reached the key set or go-jose")
	}
}

func TestRotationAndCooldown(t *testing.T) {
	r := newRig(t, Config{}, jwk(&rsaKey.PublicKey, "rsa", ""))
	if _, err := r.verify(sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(start))); err != nil {
		t.Fatal(err)
	}
	r.idp.serve(t, jwk(&rsaKey.PublicKey, "rsa", ""), jwk(&ecKey.PublicKey, "ec", ""))
	rotated := sign(t, ecKey, jose.ES256, "ec", "at+jwt", payload(start))

	// Inside the cooldown a new kid is "try later", not "sign in again".
	_, err := r.verify(rotated)
	checkRefusal(t, err, ReasonKeyCooldown)
	var e *Error
	if !errors.As(err, &e) || e.RetryAfter <= 0 || e.RetryAfter > refetchCooldown || !e.Reason.Temporary() {
		t.Fatalf("cooldown refusal = %+v", e)
	}
	flood := make([]string, 20)
	for i := range flood {
		flood[i] = sign(t, ecKey, jose.ES256, fmt.Sprint("flood-", i), "at+jwt", payload(start))
	}
	var wg sync.WaitGroup
	for _, tok := range flood {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.verify(tok)
			if ReasonOf(err) != ReasonKeyCooldown {
				t.Errorf("flood: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := r.idp.hits.Load(); n != 1 {
		t.Fatalf("fetches during the cooldown: %d, want 1", n)
	}

	r.clk.add(refetchCooldown)
	if _, err := r.verify(rotated); err != nil {
		t.Fatalf("rotated key after the cooldown: %v", err)
	}
	r.clk.add(refetchCooldown)
	_, err = r.verify(sign(t, ecKey, jose.ES256, "gone", "at+jwt", payload(start)))
	checkRefusal(t, err, ReasonUnknownKey)
	if n := r.idp.hits.Load(); n != 3 {
		t.Fatalf("fetches = %d, want 3", n)
	}
	// A failed fetch keeps the last good set.
	r.idp.status.Store(http.StatusServiceUnavailable)
	r.clk.add(refetchCooldown)
	_, err = r.verify(sign(t, ecKey, jose.ES256, "gone", "at+jwt", payload(start)))
	checkRefusal(t, err, ReasonKeyFetchFailed)
	if _, err := r.verify(rotated); err != nil {
		t.Fatalf("known key after a failed fetch: %v", err)
	}
}

func TestRefreshDropsWithdrawnKey(t *testing.T) {
	r := newRig(t, Config{})
	ec := sign(t, ecKey, jose.ES256, "ec", "at+jwt", payload(start.Add(refreshInterval)))
	if err := r.v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.idp.serve(t, jwk(&rsaKey.PublicKey, "rsa", ""))
	r.clk.add(refreshInterval)
	if _, err := r.verify(ec); err != nil { // the old set serves while the new one loads
		t.Fatal(err)
	}
	r.waitFlight()
	_, err := r.verify(ec)
	checkRefusal(t, err, ReasonKeyCooldown)
	r.clk.add(refetchCooldown)
	_, err = r.verify(ec)
	checkRefusal(t, err, ReasonUnknownKey)
}

func TestKeySetFetchFailures(t *testing.T) {
	genKeys()
	target := newIDP(t, jwk(&rsaKey.PublicKey, "rsa", ""))
	cases := []struct {
		name  string
		setup func(*testing.T, *idp)
	}{
		{"too large, otherwise valid", func(_ *testing.T, p *idp) {
			b, _ := json.Marshal(map[string]any{"keys": []any{jwk(&rsaKey.PublicKey, "rsa", "")}, "pad": strings.Repeat("a", maxJWKSBytes)})
			p.body.Store(b)
		}},
		{"status 500 with a valid body", func(_ *testing.T, p *idp) { p.status.Store(http.StatusInternalServerError) }},
		{"redirect", func(_ *testing.T, p *idp) {
			p.status.Store(http.StatusFound)
			p.location.Store(target.srv.URL + "/jwks")
		}},
		{"not JSON", func(_ *testing.T, p *idp) { p.body.Store([]byte("<html>MARKER")) }},
		{"no usable key", func(t *testing.T, p *idp) { p.serve(t, jwk(&rsaSmall.PublicKey, "rsa", "")) }},
		{"too many keys", func(t *testing.T, p *idp) {
			keys := make([]any, maxKeys+1)
			for i := range keys {
				keys[i] = jwk(&ecKey.PublicKey, fmt.Sprint(i), "")
			}
			p.serve(t, keys...)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Config{})
			tc.setup(t, r.idp)
			if err := r.v.Refresh(context.Background()); err == nil || strings.Contains(err.Error(), "MARKER") {
				t.Fatalf("Refresh = %v", err)
			}
			tok := sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(start))
			_, err := r.verify(tok)
			checkRefusal(t, err, ReasonKeyFetchFailed)
			if n := r.idp.hits.Load() + target.hits.Load(); n != 1 {
				t.Fatalf("fetches = %d, want 1 (redirects not followed, failures cooled down)", n)
			}
		})
	}
}

func TestParseKeySetKeepsOnlySigningKeys(t *testing.T) {
	genKeys()
	n := b64(rsaKey.N.Bytes())
	// The GHSA-mgcc-cww5-fp2q shape: private members with a huge prime,
	// costly to validate if anything parsed them.
	p := new(big.Int).Lsh(big.NewInt(1), 16383)
	keys := []any{
		jwk(&rsaKey.PublicKey, "rsa", "RS256"),
		map[string]any{"kty": "RSA", "kid": "enc", "use": "enc", "n": n, "e": "AQAB"},
		map[string]any{"kty": "RSA", "kid": "ops", "key_ops": []string{"encrypt"}, "n": n, "e": "AQAB"},
		map[string]any{"kty": "RSA", "kid": "hs", "alg": "HS256", "n": n, "e": "AQAB"},
		map[string]any{"kty": "RSA", "kid": "priv", "n": n, "e": "AQAB", "d": "Aw", "p": b64(p.Bytes()), "q": "Aw"},
		map[string]any{"kty": "oct", "kid": "oct", "k": "c2VjcmV0"},
		map[string]any{"kty": "OKP", "kid": "x25519", "crv": "X25519", "x": b64(make([]byte, 32))},
		map[string]any{"kty": "RSA", "n": n, "e": "AQAB"},
		jwk(&rsaSmall.PublicKey, "small", ""),
		jwk(&ecKey.PublicKey, "dup", ""), jwk(&rsaKey.PublicKey, "dup", ""), jwk(&ec384Key.PublicKey, "dup", ""),
		jwk(&ecKey.PublicKey, "ec", ""),
	}
	b, _ := json.Marshal(map[string]any{"keys": keys})
	got, err := parseKeySet(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["rsa"].pub == nil || got["ec"].pub == nil {
		t.Fatalf("kept %v, want only rsa and ec", got)
	}
}

// The verified iss is checked again: the early check read it unverified.
func TestCheckClaimsRechecksIssuer(t *testing.T) {
	v := &Verifier{issuer: testIssuer, audience: testAudience, now: func() time.Time { return start }}
	exp := jwt.NumericDate(start.Add(time.Minute).Unix())
	err := v.checkClaims(&claims{Issuer: "https://other.example", Subject: "s", Audience: jwt.Audience{testAudience}, Expiry: &exp})
	checkRefusal(t, err, ReasonWrongIssuer)
}

func TestNewChecksConfig(t *testing.T) {
	ok := Config{Issuer: testIssuer, Audience: testAudience}
	for _, u := range []string{"https://idp.example/jwks", "http://127.0.0.1:8080/jwks", "http://[::1]/jwks", "http://localhost/jwks"} {
		c := ok
		c.JWKSURL = u
		if _, err := New(c); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	for _, c := range []Config{
		{Audience: testAudience, JWKSURL: "https://idp.example/jwks"},
		{Issuer: testIssuer, JWKSURL: "https://idp.example/jwks"},
		{Issuer: testIssuer, Audience: testAudience},
		{Issuer: testIssuer, Audience: testAudience, JWKSURL: "http://203.0.113.5/jwks"},
		{Issuer: testIssuer, Audience: testAudience, JWKSURL: "http://idp.example/jwks"},
		{Issuer: testIssuer, Audience: testAudience, JWKSURL: "https://u:p@idp.example/jwks"},
		{Issuer: testIssuer, Audience: testAudience, JWKSURL: "https://idp.example/jwks#k"},
		{Issuer: testIssuer, Audience: testAudience, JWKSURL: "/jwks"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	r := newRig(t, Config{})
	if r.idp.hits.Load() != 0 {
		t.Fatal("New fetched the key set")
	}
}

func TestConcurrentVerify(t *testing.T) {
	r := newRig(t, Config{})
	long := set("exp", start.Add(time.Hour).Unix())
	good := []string{
		sign(t, rsaKey, jose.RS256, "rsa", "at+jwt", payload(start, long)),
		sign(t, ecKey, jose.ES256, "ec", "at+jwt", payload(start, long)),
	}
	unknown := make([]string, 8*25)
	for i := range unknown {
		unknown[i] = sign(t, ecKey, jose.ES256, fmt.Sprint("u", i), "at+jwt", payload(start, long))
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				if g == 0 && i%5 == 0 {
					r.clk.add(refetchCooldown) // lets refetches and refreshes run meanwhile
				}
				if i%4 == 3 {
					_, err := r.verify(unknown[g*25+i])
					if rr := ReasonOf(err); rr != ReasonUnknownKey && rr != ReasonKeyCooldown {
						t.Errorf("unknown kid: %v", err)
					}
					continue
				}
				if _, err := r.verify(good[i%2]); err != nil {
					t.Errorf("valid token: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	r.waitFlight()
}

func BenchmarkVerify(b *testing.B) {
	genKeys()
	for _, bc := range []struct {
		name string
		key  any
		alg  jose.SignatureAlgorithm
		kid  string
	}{{"RS256", rsaKey, jose.RS256, "rsa"}, {"ES256", ecKey, jose.ES256, "ec"}} {
		b.Run(bc.name, func(b *testing.B) {
			r := newRig(b, Config{})
			tok := sign(b, bc.key, bc.alg, bc.kid, "at+jwt", payload(start))
			if _, err := r.verify(tok); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.verify(tok); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
