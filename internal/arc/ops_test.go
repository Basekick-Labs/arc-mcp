package arc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// GetJSON's failures are bare *Error values of the kind that names them, so
// the ops tools classify them without unwrapping, and a body past the cap is
// never buffered whole.
func TestClient_GetJSON_Failures(t *testing.T) {
	tests := []struct {
		name  string
		serve func(w http.ResponseWriter)
		kind  string
	}{
		{"over the 64 MiB cap", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"a":"`))
			chunk := []byte(strings.Repeat("x", 1<<20))
			for range maxArcResponseBytes/len(chunk) + 1 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
			_, _ = w.Write([]byte(`"}`))
		}, KindTooLarge},
		{"not JSON", func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>")) }, KindParse},
		{"JSON followed by a page", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"a":1}<html>`)) }, KindParse},
		{"two JSON values", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"a":1} {"b":2}`)) }, KindParse},
		{"404", func(w http.ResponseWriter) { w.WriteHeader(404) }, KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tt.serve(w) }))
			defer srv.Close()
			_, err := NewClient(srv.URL, "tok", 30*time.Second).GetJSON(context.Background(), "/api/v1/metrics", nil)
			ae, ok := err.(*Error) // the point is that it is not wrapped
			if !ok || ae.Kind != tt.kind {
				t.Errorf("err = %#v, want a bare *Error of kind %s", err, tt.kind)
			}
		})
	}
}

// GetJSON keeps every number as Arc wrote it.
func TestClient_GetJSON_ExactNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"n":9007199254740993,"f":0.1000000000000000055511151231257827}`))
	}))
	defer srv.Close()
	v, err := NewClient(srv.URL, "", time.Second).GetJSON(context.Background(), "/api/v1/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(v)
	if string(got) != `{"f":0.1000000000000000055511151231257827,"n":9007199254740993}` {
		t.Errorf("got %s", got)
	}
}

// Probe reports any HTTP answer with its status, keeps only a JSON object
// body, and errs only when nothing came back.
func TestClient_Probe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"status":"not_ready"}`))
		case "/health":
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`<html>`))
		case "/trail":
			_, _ = w.Write([]byte("{\"status\":\"ok\"} \n<html>"))
		case "/spaces":
			_, _ = w.Write([]byte("{\"status\":\"ok\"} \n\t"))
		default:
			_, _ = w.Write([]byte(strings.Repeat(" ", maxProbeBodyBytes+1)))
		}
	}))
	c := NewClient(srv.URL, "", time.Second)
	if status, body, err := c.Probe(context.Background(), "/ready"); err != nil || status != 503 || body["status"] != "not_ready" {
		t.Errorf("/ready: %d %v %v", status, body, err)
	}
	if status, body, err := c.Probe(context.Background(), "/health"); err != nil || status != 502 || body != nil {
		t.Errorf("/health: %d %v %v", status, body, err)
	}
	if status, body, err := c.Probe(context.Background(), "/trail"); err != nil || status != 200 || body != nil {
		t.Errorf("/trail: Arc's JSON followed by a page must not pass as Arc's: %d %v %v", status, body, err)
	}
	if status, body, err := c.Probe(context.Background(), "/spaces"); err != nil || status != 200 || body["status"] != "ok" {
		t.Errorf("/spaces: trailing whitespace is fine: %d %v %v", status, body, err)
	}
	var ae *Error
	if status, _, err := c.Probe(context.Background(), "/big"); status != 200 || !errors.As(err, &ae) || ae.Kind != KindTooLarge {
		t.Errorf("/big: %d %v", status, err)
	}
	srv.Close()
	status, _, err := c.Probe(context.Background(), "/health")
	if status != 0 || !errors.As(err, &ae) || ae.Kind != KindNetwork {
		t.Fatalf("down: %d %v", status, err)
	}
	if msg := UserMessage(ae); strings.Contains(msg, srv.URL) {
		t.Errorf("down: the model's text carries Arc's URL: %s", msg)
	}
}
