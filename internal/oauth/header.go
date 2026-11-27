package oauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
)

// header is the part of the JOSE header Verify uses, as preParse read it.
type header struct {
	alg, kid, typ string
}

// refusedMembers are header members that preParse refuses whatever their
// value, null included. jwk, jku, x5u and x5c carry or point to a key or a
// certificate: keys come only from the configured key set, and go-jose
// parsing such a member is the path of GHSA-mgcc-cww5-fp2q (an embedded RSA
// JWK that costs seconds of CPU before any signature check, fixed in go-jose
// 4.1.6). crit and b64 change how the token is read, and nothing here
// implements an extension. x5t and x5t#S256 are only certificate
// thumbprints and are allowed: some authorization servers put x5t next to
// kid.
var refusedMembers = map[string]bool{
	"jwk": true, "jku": true, "x5u": true, "x5c": true,
	"crit": true, "b64": true,
}

var b64url = base64.RawURLEncoding.Strict()

// preParse checks the shape of a compact JWS and decodes its header with
// encoding/json, before go-jose sees anything: three non-empty base64url
// segments, a header object with no duplicate member (go-jose's JSON
// refuses them too, and matches names case-sensitively, as this does), no
// refused member, an allowed alg and a kid. It returns the decoded payload
// too, unverified.
func preParse(raw string) (header, []byte, error) {
	var h header
	for i := 0; i < len(raw); i++ {
		if !compactChar(raw[i]) {
			return h, nil, refuse(ReasonMalformed)
		}
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return h, nil, refuse(ReasonMalformed)
	}
	hb, err := b64url.DecodeString(parts[0])
	if err != nil {
		return h, nil, refuse(ReasonMalformed)
	}
	if h, err = parseHeader(hb); err != nil {
		return h, nil, err
	}
	payload, err := b64url.DecodeString(parts[1])
	if err != nil {
		return h, nil, refuse(ReasonMalformed)
	}
	return h, payload, nil
}

// compactChar: the base64url alphabet and the segment dot.
func compactChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return c == '-' || c == '_' || c == '.'
}

func parseHeader(b []byte) (header, error) {
	var h header
	bad := refuse(ReasonMalformed)
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return h, bad
	}
	seen := map[string]bool{}
	var hasAlg, hasKid bool
	for dec.More() {
		t, err := dec.Token()
		name, ok := t.(string)
		if err != nil || !ok || seen[name] || refusedMembers[name] {
			return h, bad
		}
		seen[name] = true
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return h, bad
		}
		var dst *string
		switch name {
		case "alg":
			dst, hasAlg = &h.alg, true
		case "kid":
			dst, hasKid = &h.kid, true
		case "typ":
			dst = &h.typ
		default:
			continue
		}
		if json.Unmarshal(val, dst) != nil {
			return h, bad
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return h, bad
	}
	if _, err := dec.Token(); err != io.EOF {
		return h, bad
	}
	if !hasAlg || !allowedAlgorithm(h.alg) {
		return h, bad
	}
	if !hasKid || h.kid == "" {
		return h, refuse(ReasonUnknownKey)
	}
	return h, nil
}
