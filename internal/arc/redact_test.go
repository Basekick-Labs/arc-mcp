package arc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRedactURL(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"http://h:8000", "http://h:8000"},
		{"http://h:8000/arc/", "http://h:8000/arc/"},
		{"http://h:8000/arc?p=tok", "http://h:8000/arc"},
		{"http://u:pw@h:8000/arc/?p=tok#frag", "http://h:8000/arc/"},
		{"https://u@h/?", "https://h/"},
		{"http://h:8000/a%2Fb/api/v1/query?p=tok", "http://h:8000/a%2Fb/api/v1/query"},
		// A redirect's Location, as Go quotes it in the error.
		{"/arc/api/v1/databases?p=tok", "/arc/api/v1/databases"},
		// Does not parse.
		{"http://u:pw@h:8000/%zz/?p=tok", "http://h:8000/%zz/"},
		{"http://u:pw@h:8000/%zz#p=tok", "http://h:8000/%zz"},
		{"http://u:p/w@h:8000/arc?p=tok", "http://h:8000/arc"},
		{"%zz?p=tok", "%zz"},
	} {
		if got := RedactURL(tt.in); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactError(t *testing.T) {
	inner := errors.New("connection refused")
	ue := &url.Error{Op: "Get", URL: "http://u:pw@h/arc/?p=tok", Err: inner}
	for _, err := range []error{ue, fmt.Errorf("listing: %w", ue)} {
		got := RedactError(err)
		if got.Error() != `Get "http://h/arc/": connection refused` {
			t.Errorf("RedactError(%v) = %v", err, got)
		}
		if !errors.Is(got, inner) {
			t.Errorf("RedactError(%v) lost the cause", err)
		}
	}
	plain := errors.New("x")
	if got := RedactError(plain); got != plain {
		t.Errorf("RedactError changed an error with no URL: %v", got)
	}
}

// A base URL carrying userinfo and a query string, as an operator may set
// one for a proxy. The client logs where it sent a request, without either.
const (
	credUser  = "user-marker"
	credPass  = "pass-marker"
	credQuery = "query-marker"
	credBase  = "http://" + credUser + ":" + credPass + "@" + addrHost + ":7357/" + addrPrefix + "/?p=" + credQuery
)

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func assertNoCredentials(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{credUser, credPass, credQuery} {
		if strings.Contains(text, s) {
			t.Errorf("%s carries %q: %q", what, s, text)
		}
	}
	if !strings.Contains(text, addrHost) && !strings.Contains(text, "/"+addrPrefix) {
		t.Errorf("%s does not say where the request went, so this check tests nothing: %q", what, text)
	}
}

// Every logged failure, Health's and VerifyToken's errors (which main logs)
// included, names the URL without its userinfo and query string.
func TestClient_LoggedURLsOmitQueryAndUserinfo(t *testing.T) {
	refused := closedAddr(t)
	redirectLoop := serverAddr(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.RequestURI(), http.StatusFound)
	})
	for _, target := range []string{refused, redirectLoop} {
		c := clientAt(credBase, target, 10*time.Second)
		buf := captureLog(t)
		for _, dc := range dataCalls {
			if err := dc.call(c); err == nil {
				t.Fatalf("%s: no error", dc.name)
			}
			assertNoCredentials(t, dc.name+" log", buf.String())
			buf.Reset()
		}
		if err := c.Health(context.Background()); err != nil {
			assertNoCredentials(t, "Health error", err.Error())
		} else {
			t.Error("Health: no error")
		}
		if _, err := c.VerifyToken(context.Background()); err != nil {
			assertNoCredentials(t, "VerifyToken error", err.Error())
		} else {
			t.Error("VerifyToken: no error")
		}
	}

	// A base URL that does not parse: url.Parse's error quotes it.
	c := NewClient("http://"+credUser+":"+credPass+"@"+addrHost+"/%zz/"+addrPrefix+"?p="+credQuery, "tok", time.Second)
	buf := captureLog(t)
	for _, dc := range dataCalls {
		_ = dc.call(c)
		assertNoCredentials(t, dc.name+" log", buf.String())
		buf.Reset()
	}
	for name, err := range map[string]error{"Health": c.Health(context.Background()), "VerifyToken": func() error { _, err := c.VerifyToken(context.Background()); return err }()} {
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		assertNoCredentials(t, name+" error", err.Error())
	}
}
