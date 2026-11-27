// Package oauth verifies JWT access tokens (RFC 9068) for the HTTP
// transport's OAuth mode (#2): the signature against the authorization
// server's published keys (JWKS), the issuer, the audience, the validity
// window and the access-token type. It is the resource server's side only:
// it never obtains, refreshes or forwards a token.
//
// Nothing a refusal carries comes from the token, the key set or go-jose.
// Every refusal is an *Error holding one Reason from a fixed set, so a
// caller can log it or answer with it without echoing attacker input.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejson "github.com/go-jose/go-jose/v4/json"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// MaxTokenBytes bounds the raw token. A longer one is refused before
	// anything in it is decoded.
	MaxTokenBytes = 16 << 10
	// Leeway is the clock skew allowed on exp, nbf and iat.
	Leeway = 60 * time.Second
)

// Reason says why a token was refused.
type Reason int

// The reasons a token is refused. KeyCooldown and KeyFetchFailed are the
// only ones a new token would not fix: the keys cannot be checked now
// (see Temporary).
const (
	ReasonMissing        Reason = iota + 1 // no token at all
	ReasonMalformed                        // not a signed JWT this package accepts, or a required claim is missing
	ReasonExpired                          // exp has passed
	ReasonNotYetValid                      // nbf or iat is in the future
	ReasonWrongIssuer                      // iss is not the configured issuer
	ReasonWrongAudience                    // aud does not contain the configured audience
	ReasonBadSignature                     // the signature does not verify with the key the token names
	ReasonUnknownKey                       // no usable key with that kid, even after a fresh fetch
	ReasonKeyCooldown                      // kid unknown, and the key set was fetched too recently to fetch again
	ReasonKeyFetchFailed                   // the key set could not be fetched
	ReasonNotAccessToken                   // a valid JWT, but nothing shows it is an access token
)

var reasonText = map[Reason]string{
	ReasonMissing:        "missing",
	ReasonMalformed:      "malformed",
	ReasonExpired:        "expired",
	ReasonNotYetValid:    "not yet valid",
	ReasonWrongIssuer:    "wrong issuer",
	ReasonWrongAudience:  "wrong audience",
	ReasonBadSignature:   "bad signature",
	ReasonUnknownKey:     "unknown key",
	ReasonKeyCooldown:    "unknown key during refetch cooldown",
	ReasonKeyFetchFailed: "key fetch failed",
	ReasonNotAccessToken: "not an access token",
}

func (r Reason) String() string {
	if s, ok := reasonText[r]; ok {
		return s
	}
	return "unknown reason"
}

// Temporary reports whether the refusal is about the key set rather than
// the token: a new token would not help, retrying later may.
func (r Reason) Temporary() bool {
	return r == ReasonKeyCooldown || r == ReasonKeyFetchFailed
}

// Error is a refusal. It never wraps another error and its text is fixed
// per Reason.
type Error struct {
	Reason Reason
	// RetryAfter is set when Reason.Temporary(): how long until the key set
	// may be fetched again.
	RetryAfter time.Duration
}

func (e *Error) Error() string { return "oauth: token refused: " + e.Reason.String() }

// ReasonOf returns the Reason of a refusal from Verify, or 0 for any other
// error.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return 0
}

func refuse(r Reason) error { return &Error{Reason: r} }

// algorithms is the signature allowlist: asymmetric only. none and every
// HMAC algorithm are absent on purpose (an HMAC "key" would be the
// authorization server's public key, which anyone can read).
var algorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

func allowedAlgorithm(alg string) bool {
	for _, a := range algorithms {
		if string(a) == alg {
			return true
		}
	}
	return false
}

// Config configures a Verifier.
type Config struct {
	// Issuer is compared with iss exactly: a trailing slash counts.
	Issuer string
	// Audience must be one of the aud values, exactly.
	Audience string
	// JWKSURL is where the authorization server publishes its signing
	// keys: https, or http to a loopback host only.
	JWKSURL string
	// HTTPClient fetches the key set; nil uses a plain client. Either way
	// redirects are refused and the fetch is bounded by a 10 s timeout.
	HTTPClient *http.Client
	// UntypedAccessToken decides tokens whose typ header is "JWT" or absent,
	// which RFC 9068 §4 tells a resource server to refuse: it returns true
	// only when the verified token shows it is an access token and not, for
	// example, an ID token for the same audience. It sees only tokens that
	// passed every other check. nil refuses every untyped token. What
	// separates the two token kinds differs between authorization servers,
	// so the rule for one is set where that server is configured, from
	// tokens it actually issued; a typ of "at+jwt" (or
	// "application/at+jwt") never reaches it, and any other typ is refused
	// without asking it.
	UntypedAccessToken func(*Token) bool
}

// Token is a verified access token.
type Token struct {
	Issuer   string
	Subject  string
	Audience []string
	// ClientID is the client the token was issued to: azp, else client_id,
	// else empty. It is reported, not checked: Keycloak names the client in
	// azp and sets client_id only on service-account tokens, so requiring
	// client_id would refuse its user tokens, and nothing here restricts
	// which clients may present a token.
	ClientID string
	// Scopes is the space-delimited scope claim, split. A scope that is not
	// a string refuses the token as malformed.
	Scopes []string
	Expiry time.Time
	// Type is the typ header as sent ("" when absent).
	Type   string
	claims map[string]josejson.RawMessage
}

// HasClaim reports whether the verified payload has the named claim, null
// included.
func (t *Token) HasClaim(name string) bool {
	_, ok := t.claims[name]
	return ok
}

// StringClaim returns the named claim when it is a JSON string.
func (t *Token) StringClaim(name string) (string, bool) {
	raw, ok := t.claims[name]
	if !ok {
		return "", false
	}
	var s string
	if josejson.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Verifier checks access tokens. It is safe for concurrent use.
type Verifier struct {
	issuer   string
	audience string
	untyped  func(*Token) bool
	keys     *keySet
	now      func() time.Time
	// parse is go-jose's compact parser; tests count calls to prove that a
	// refused header never reaches it.
	parse func(string, []jose.SignatureAlgorithm) (*jose.JSONWebSignature, error)
}

// New checks cfg and returns a Verifier. It fetches nothing: the key set is
// loaded on the first Verify, or by Refresh.
func New(cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("oauth: issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("oauth: audience is required")
	}
	if err := checkJWKSURL(cfg.JWKSURL); err != nil {
		return nil, err
	}
	client := &http.Client{}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout <= 0 || client.Timeout > fetchTimeout {
		client.Timeout = fetchTimeout
	}
	now := time.Now
	return &Verifier{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		untyped:  cfg.UntypedAccessToken,
		keys:     &keySet{url: cfg.JWKSURL, client: client, now: now},
		now:      now,
		parse:    jose.ParseSignedCompact,
	}, nil
}

func checkJWKSURL(raw string) error {
	if raw == "" {
		return errors.New("oauth: JWKS URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("oauth: JWKS URL is not an absolute URL")
	}
	if u.User != nil || u.Fragment != "" {
		return errors.New("oauth: JWKS URL must not carry userinfo or a fragment")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return fmt.Errorf("oauth: JWKS URL must use https (http only to a loopback host), got %s://%s", u.Scheme, u.Host)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Refresh fetches the key set now (at startup, for example). The error
// names a fixed cause, never the response body.
func (v *Verifier) Refresh(ctx context.Context) error {
	return v.keys.refresh(ctx)
}

// claims is the payload as verified, decoded with go-jose's JSON (case-
// sensitive member names, duplicates refused).
type claims struct {
	Issuer    string           `json:"iss"`
	Subject   string           `json:"sub"`
	Audience  jwt.Audience     `json:"aud"`
	Expiry    *jwt.NumericDate `json:"exp"`
	NotBefore *jwt.NumericDate `json:"nbf"`
	IssuedAt  *jwt.NumericDate `json:"iat"`
	AZP       string           `json:"azp"`
	ClientID  string           `json:"client_id"`
	Scope     *string          `json:"scope"`
}

// Verify checks raw, the token without its "Bearer " prefix. Cheap
// refusals come first: the size and header checks, then the unverified
// issuer, so a token from another issuer never makes the key set be
// fetched. go-jose parses the token only after its header has passed and a
// key fitting its alg was found.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Token, error) {
	if raw == "" {
		return nil, refuse(ReasonMissing)
	}
	if len(raw) > MaxTokenBytes {
		return nil, refuse(ReasonMalformed)
	}
	h, payload, err := preParse(raw)
	if err != nil {
		return nil, err
	}
	if unverifiedIssuer(payload) != v.issuer {
		return nil, refuse(ReasonWrongIssuer)
	}
	k, err := v.keys.lookup(ctx, h.kid)
	if err != nil {
		return nil, err
	}
	if !k.fits(h.alg) {
		return nil, refuse(ReasonUnknownKey)
	}
	jws, err := v.parse(raw, algorithms)
	if err != nil || len(jws.Signatures) != 1 {
		return nil, refuse(ReasonMalformed)
	}
	// go-jose must have read the header as preParse did.
	if got := jws.Signatures[0].Header; got.Algorithm != h.alg || got.KeyID != h.kid || got.JSONWebKey != nil {
		return nil, refuse(ReasonMalformed)
	}
	body, err := jws.Verify(k.pub)
	if err != nil {
		return nil, refuse(ReasonBadSignature)
	}
	var c claims
	var all map[string]josejson.RawMessage
	if josejson.Unmarshal(body, &c) != nil || josejson.Unmarshal(body, &all) != nil {
		return nil, refuse(ReasonMalformed)
	}
	if err := v.checkClaims(&c); err != nil {
		return nil, err
	}
	t := &Token{
		Issuer:   c.Issuer,
		Subject:  c.Subject,
		Audience: c.Audience,
		ClientID: c.AZP,
		Expiry:   c.Expiry.Time(),
		Type:     h.typ,
		claims:   all,
	}
	if t.ClientID == "" {
		t.ClientID = c.ClientID
	}
	if c.Scope != nil {
		t.Scopes = strings.Fields(*c.Scope)
	}
	if !v.isAccessToken(t) {
		return nil, refuse(ReasonNotAccessToken)
	}
	return t, nil
}

func (v *Verifier) checkClaims(c *claims) error {
	now := v.now()
	switch {
	case c.Issuer != v.issuer:
		return refuse(ReasonWrongIssuer)
	case !c.Audience.Contains(v.audience):
		return refuse(ReasonWrongAudience)
	case c.Expiry == nil || c.Subject == "":
		return refuse(ReasonMalformed)
	case !now.Before(c.Expiry.Time().Add(Leeway)):
		return refuse(ReasonExpired)
	case c.NotBefore != nil && now.Add(Leeway).Before(c.NotBefore.Time()),
		c.IssuedAt != nil && now.Add(Leeway).Before(c.IssuedAt.Time()):
		return refuse(ReasonNotYetValid)
	}
	return nil
}

// isAccessToken applies RFC 9068 §4's typ rule. Media types are case-
// insensitive and the "application/" prefix may be left out (RFC 7515
// §4.1.9).
func (v *Verifier) isAccessToken(t *Token) bool {
	typ := strings.TrimPrefix(strings.ToLower(t.Type), "application/")
	switch typ {
	case "at+jwt":
		return true
	case "", "jwt":
		return v.untyped != nil && v.untyped(t)
	}
	return false
}

// unverifiedIssuer reads iss from a payload whose signature is not checked
// yet; only to refuse early. "" when absent or not a string.
func unverifiedIssuer(payload []byte) string {
	var all map[string]json.RawMessage
	var s string
	if json.Unmarshal(payload, &all) != nil || json.Unmarshal(all["iss"], &s) != nil {
		return ""
	}
	return s
}
