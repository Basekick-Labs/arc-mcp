package arc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The base URL the tests give Arc. Its host resolves nowhere: the client's
// transport dials a local listener instead, so each request fails, or is
// answered, the way a real one would, and Go's error text carries this URL
// wherever it would carry Arc's. None of its parts may reach the model.
const (
	addrHost   = "arc-address.mcpfix.invalid"
	addrPrefix = "arc-prefix-marker"
	addrQuery  = "query-marker"
	addrBase   = "http://" + addrHost + ":7357/" + addrPrefix + "/?m=" + addrQuery
)

const (
	unreachableText = "Arc could not be reached. Ask the user to check that the Arc server is running at the configured URL."
	unexpectedText  = "Arc returned an unexpected response format."
	genericText     = "Arc returned an error — see server logs for details."
)

// clientVia is a client for addrBase whose connections all go to target.
func clientVia(target string, timeout time.Duration) *Client {
	return clientAt(addrBase, target, timeout)
}

// clientAt is a client for base whose connections all go to target.
func clientAt(base, target string, timeout time.Duration) *Client {
	c := NewClient(base, "tok", timeout)
	c.httpClient.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
	}
	return c
}

// assertNoAddress fails if text carries any part of Arc's address: the
// configured URL's host, prefix or query, the address dialled, or Go's
// error wording around them.
func assertNoAddress(t *testing.T, text, target string) {
	t.Helper()
	_, port, _ := net.SplitHostPort(target)
	for _, s := range []string{addrHost, addrPrefix, addrQuery, "127.0.0.1", ":" + port, "http", "dial", "tcp", "Arc error"} {
		if strings.Contains(text, s) {
			t.Errorf("answer carries %q: %q", s, text)
		}
	}
}

// dataCalls are the client reads behind the data tools, with the text each
// gives the model when arc-mcp's --timeout (timeoutFor) fires.
var dataCalls = []struct {
	name        string
	call        func(*Client) error
	timeoutText string
}{
	{"list databases", func(c *Client) error { _, err := c.ListDatabases(context.Background()); return err }, listingTimedOutText(timeoutFor)},
	{"list measurements", func(c *Client) error { _, err := c.ListMeasurements(context.Background(), "db"); return err }, listingTimedOutText(timeoutFor)},
	{"query", func(c *Client) error { _, err := c.Query(context.Background(), "db", "SELECT 1"); return err }, "The query failed: " + timedOutHint},
}

// timeoutFor is the client timeout of the rows that must time out.
const timeoutFor = 300 * time.Millisecond

// closedAddr is a local address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// serverAddr starts h and returns its address.
func serverAddr(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// Every way a data read can fail without an answer from Arc gives the model
// a fixed text: never Go's error, which names Arc's URL and address.
func TestDataReads_FailuresWithoutAnAnswerCarryNoAddress(t *testing.T) {
	// The handler does not read the query's body, so its context is not
	// cancelled when the client gives up: block is closed before the
	// server's Close (cleanups run last-in first-out), which waits for it.
	block := make(chan struct{})
	slow := serverAddr(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(block) })
	redirectLoop := serverAddr(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.RequestURI(), http.StatusFound)
	})
	// Headers and part of the body, then a reset: the decoder's error
	// names both ends of the connection.
	resetMidBody := serverAddr(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"databases":[{"name":"a`))
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	})
	notJSON := serverAddr(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>proxy for http://" + addrHost + "</html>"))
	})
	refused := closedAddr(t)

	for _, dc := range dataCalls {
		for _, tt := range []struct {
			name, target, want string
			// alt is a second fixed answer the row accepts: on Windows a
			// reset can discard the headers already received, so the
			// request fails rather than its body.
			alt string
			// carried: Go's error names the configured URL (a redirect's
			// Location, for the loop), so the row shows UserMessage hiding
			// it rather than passing vacuously.
			carried bool
		}{
			{"refused", refused, unreachableText, "", true},
			{"timeout", slow, dc.timeoutText, "", true},
			{"redirect loop", redirectLoop, unreachableText, "", true},
			{"reset mid-body", resetMidBody, unexpectedText, unreachableText, false},
			{"not JSON", notJSON, unexpectedText, "", false},
		} {
			t.Run(dc.name+" "+tt.name, func(t *testing.T) {
				// Only the timeout row may run out of time; the others get
				// long enough for a slow runner.
				timeout := 10 * time.Second
				if tt.name == "timeout" {
					timeout = timeoutFor
				}
				err := dc.call(clientVia(tt.target, timeout))
				if err == nil {
					t.Fatal("no error")
				}
				if tt.carried && !strings.Contains(err.Error(), addrPrefix) {
					t.Fatalf("Go's error does not name the configured URL, so this row tests nothing: %v", err)
				}
				got := UserMessage(err)
				if got != tt.want && (tt.alt == "" || got != tt.alt) {
					t.Errorf("UserMessage = %q, want %q (error: %v)", got, tt.want, err)
				}
				assertNoAddress(t, got, tt.target)
			})
		}
	}
}

// A base URL that does not parse fails every data read with the fixed
// text; url.Parse's error, which quotes it, stays in the log.
func TestDataReads_UnparseableBaseURL(t *testing.T) {
	c := NewClient("http://"+addrHost+"/%zz/"+addrPrefix, "tok", time.Second)
	for _, dc := range dataCalls {
		err := dc.call(c)
		if err == nil || !strings.Contains(err.Error(), addrHost) {
			t.Fatalf("%s: want url.Parse's error naming the URL, got %v", dc.name, err)
		}
		got := UserMessage(err)
		if got != unreachableText {
			t.Errorf("%s: UserMessage = %q, want %q", dc.name, got, unreachableText)
		}
		assertNoAddress(t, got, "127.0.0.1:0")
	}
}

// UserMessage classifies a wrapped *Error, and never shows the text of an
// error the client did not classify.
func TestUserMessage_UnclassifiedErrorsAreNotShown(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"plain error naming Arc", errors.New(`Get "` + addrBase + `": dial tcp 127.0.0.1:7357: connect: connection refused`), genericText},
		{"url.Error", &url.Error{Op: "Get", URL: addrBase, Err: errors.New("connection refused")}, genericText},
		{"wrapped url.Error", fmt.Errorf("listing databases: %w", &url.Error{Op: "Get", URL: addrBase, Err: errors.New("x")}), genericText},
		{"wrapped *Error", fmt.Errorf("listing databases: %w", &Error{Kind: KindNetwork, Detail: addrBase}), unreachableText},
		{"wrapped listing timeout", fmt.Errorf("x: %w", &Error{Kind: KindTimeout, Detail: addrBase, Message: listingTimedOutText(time.Second)}), listingTimedOutText(time.Second)},
		{"typed nil *Error", error((*Error)(nil)), genericText},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := UserMessage(tt.err)
			if got != tt.want {
				t.Errorf("UserMessage = %q, want %q", got, tt.want)
			}
			assertNoAddress(t, got, "127.0.0.1:7357")
		})
	}
}
