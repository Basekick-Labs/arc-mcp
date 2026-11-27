// Package callerauth is passthrough authentication (#1): each HTTP caller's
// Arc token is verified with Arc, then carried to every request arc-mcp
// makes to Arc on that caller's behalf.
package callerauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/basekick-labs/arc-mcp/internal/httpserve"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// cacheTTL is how long a verified token is trusted at the endpoint
	// without asking Arc again. Arc still checks the token on every query,
	// so a token revoked in Arc fails its queries at once; the cache only
	// lets it through initialize and tools/list for up to this long.
	cacheTTL = 60 * time.Second
	// maxCached bounds the cache. Only valid tokens enter it, so a flood of
	// made-up tokens cannot push valid ones out.
	maxCached = 1024
	// maxVerifying bounds verify calls to Arc in flight. A verification
	// waits up to slotWait for a slot, then gets 503: a burst of new
	// callers is served, a flood of made-up tokens cannot pile up on Arc.
	maxVerifying = 4
	slotWait     = 2 * time.Second
	// maxWarned bounds the set of token ids already warned about.
	maxWarned = 1024
)

// Verifier verifies callers' tokens with Arc and remembers valid ones.
type Verifier struct {
	client *arc.Client
	now    func() time.Time
	slots  chan struct{}

	mu      sync.Mutex
	cache   map[[32]byte]entry
	warned  map[int64]bool
	flights map[[32]byte]*flight
}

// flight is one verification in progress: requests with the same token
// wait for it instead of asking Arc again (one client's parallel calls).
type flight struct {
	done     chan struct{}
	identity string
	err      error
}

type entry struct {
	identity string
	until    time.Time
}

// New returns a Verifier for client, which must be in passthrough mode;
// Forget is the client's onRejected hook.
func New(client *arc.Client) *Verifier {
	return &Verifier{
		client:  client,
		now:     time.Now,
		slots:   make(chan struct{}, maxVerifying),
		cache:   make(map[[32]byte]entry),
		warned:  make(map[int64]bool),
		flights: make(map[[32]byte]*flight),
	}
}

// key is the cache key: a digest, so the cache holds no token.
func key(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// Verify is an httpserve.Verifier: a valid token is accepted with its Arc
// token id as the identity; one Arc refuses is refused; when Arc cannot
// tell, or too many verifications are in flight, httpserve.ErrUnavailable.
func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	k := key(token)
	now := v.now()
	v.mu.Lock()
	if e, ok := v.cache[k]; ok {
		if now.Before(e.until) {
			v.mu.Unlock()
			return e.identity, nil
		}
		delete(v.cache, k)
	}
	if f, ok := v.flights[k]; ok {
		v.mu.Unlock()
		select {
		case <-f.done:
			return f.identity, f.err
		case <-ctx.Done():
			return "", httpserve.ErrUnavailable
		}
	}
	f := &flight{done: make(chan struct{})}
	v.flights[k] = f
	v.mu.Unlock()

	f.identity, f.err = v.verify(ctx, k, token, now)
	v.mu.Lock()
	delete(v.flights, k)
	v.mu.Unlock()
	close(f.done)
	return f.identity, f.err
}

// verify asks Arc, in one of the slots.
func (v *Verifier) verify(ctx context.Context, k [32]byte, token string, now time.Time) (string, error) {
	wait := time.NewTimer(slotWait)
	defer wait.Stop()
	select {
	case v.slots <- struct{}{}:
		defer func() { <-v.slots }()
	case <-wait.C:
		return "", httpserve.ErrUnavailable
	case <-ctx.Done():
		return "", httpserve.ErrUnavailable
	}
	// A request cancelled mid-verification must not decide for the others
	// waiting on the same flight: Arc is asked under its own deadline.
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	caller, err := v.client.VerifyCaller(vctx, token)
	switch {
	case errors.Is(err, arc.ErrCallerRejected):
		return "", err
	case err != nil:
		return "", httpserve.ErrUnavailable
	}
	identity := fmt.Sprintf("arc-token:%d", caller.ID)
	v.remember(k, identity, caller, now)
	v.warnOverScoped(caller)
	return identity, nil
}

// remember caches a valid token until cacheTTL from now, or its expiry if
// sooner. A full cache drops expired entries; if it is still full, the
// token is not cached (it is verified again next time).
func (v *Verifier) remember(k [32]byte, identity string, caller arc.Caller, now time.Time) {
	until := now.Add(cacheTTL)
	if !caller.ExpiresAt.IsZero() && caller.ExpiresAt.Before(until) {
		until = caller.ExpiresAt
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= maxCached {
		for ck, e := range v.cache {
			if !now.Before(e.until) {
				delete(v.cache, ck)
			}
		}
		if len(v.cache) >= maxCached {
			return
		}
	}
	v.cache[k] = entry{identity: identity, until: until}
}

// Forget drops token from the cache: the Arc client calls it when Arc
// answers 401 to a request made with the token.
func (v *Verifier) Forget(token string) {
	v.mu.Lock()
	delete(v.cache, key(token))
	v.mu.Unlock()
}

// warnOverScoped logs once per token id when a caller's token carries more
// than read: arc-mcp only reads, and the caller should use a read token.
func (v *Verifier) warnOverScoped(caller arc.Caller) {
	var over []string
	for _, p := range caller.Permissions {
		switch p {
		case "write", "delete", "admin":
			over = append(over, p)
		}
	}
	if len(over) == 0 {
		return
	}
	v.mu.Lock()
	seen := v.warned[caller.ID]
	if !seen && len(v.warned) < maxWarned {
		v.warned[caller.ID] = true
	}
	v.mu.Unlock()
	if !seen {
		log.Printf("WARNING: caller arc-token:%d carries %s; arc-mcp only reads, so a read token is enough", caller.ID, strings.Join(over, ", "))
	}
}

// Middleware carries each MCP request's bearer token into the context its
// handler runs with, where a passthrough arc.Client finds it. It reads the
// header with httpserve.BearerToken, as the endpoint's verification did,
// so the token sent to Arc is the token verified. A request with no HTTP
// header (stdio) or no token is passed on unchanged; the client then
// refuses to call Arc.
func Middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if extra := req.GetExtra(); extra != nil && extra.Header != nil {
			if token, ok := httpserve.BearerToken(extra.Header); ok {
				ctx = arc.WithToken(ctx, token)
			}
		}
		return next(ctx, method, req)
	}
}
