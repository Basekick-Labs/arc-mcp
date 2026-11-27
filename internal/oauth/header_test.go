package oauth

import (
	"encoding/json"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

// FuzzPreParse checks preParse against two independent readers of the same
// header: whatever it accepts has no refused member as encoding/json sees
// it, and go-jose, when it parses the token at all, reads the same alg and
// kid and holds no embedded key.
func FuzzPreParse(f *testing.F) {
	genKeys()
	for _, s := range []string{
		sign(f, rsaKey, jose.RS256, "rsa", "at+jwt", payload(start), "x5t", "dGh1bWI"),
		unsigned(`{"alg":"RS256","kid":"k","jwk":{"kty":"RSA","n":"AQAB","e":"AQAB"}}`, payload(start)),
		unsigned(`{"alg":"RS256","kid":"k","jwk":null}`, payload(start)),
		unsigned(`{"alg":"RS256","kid":"k","JWK":{}}`, payload(start)),
		unsigned(`{"alg":"RS256","kid":"k","alg":"RS256"}`, payload(start)),
		unsigned(`{"alg":"ES256","kid":"k","typ":null} `, payload(start)),
		unsigned(`{"alg":"RS256","kid":"k"}{}`, payload(start)),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > MaxTokenBytes {
			return
		}
		h, _, err := preParse(raw)
		if err != nil {
			return
		}
		hb, err := b64url.DecodeString(raw[:strings.IndexByte(raw, '.')])
		var m map[string]any
		if err != nil || json.Unmarshal(hb, &m) != nil {
			t.Fatalf("accepted a header encoding/json cannot read: %q", hb)
		}
		for name := range refusedMembers {
			if _, ok := m[name]; ok {
				t.Fatalf("accepted a header with %q: %q", name, hb)
			}
		}
		if !allowedAlgorithm(h.alg) || h.kid == "" {
			t.Fatalf("accepted alg %q, kid %q", h.alg, h.kid)
		}
		jws, err := jose.ParseSignedCompact(raw, algorithms)
		if err != nil {
			return
		}
		got := jws.Signatures[0].Header
		if got.Algorithm != h.alg || got.KeyID != h.kid || got.JSONWebKey != nil {
			t.Fatalf("go-jose read alg %q kid %q (jwk %v), preParse alg %q kid %q", got.Algorithm, got.KeyID, got.JSONWebKey != nil, h.alg, h.kid)
		}
		for name := range refusedMembers {
			if _, ok := got.ExtraHeaders[jose.HeaderKey(name)]; ok {
				t.Fatalf("go-jose holds %q", name)
			}
		}
	})
}
