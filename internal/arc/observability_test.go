package arc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestGetJSON_EncodesQueryAndSendsToken(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		fmt.Fprint(w, `{"family":"api","count":9007199254740993}`)
	}))
	defer srv.Close()

	query := make(url.Values)
	query.Set("duration_minutes", "30")
	body, err := NewClient(srv.URL+"/proxy/", "secret", time.Second).GetJSON(context.Background(), "/api/v1/metrics/timeseries/api", query)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/proxy/api/v1/metrics/timeseries/api" || gotQuery != "duration_minutes=30" || gotAuth != "Bearer secret" {
		t.Errorf("request = path %q query %q auth %q", gotPath, gotQuery, gotAuth)
	}
	if string(body) != `{"family":"api","count":9007199254740993}` {
		t.Errorf("JSON number changed: %s", body)
	}
}

func TestGetJSON_Readiness503IsAValidSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"not_ready","reason":"startup recovery"}`)
	}))
	defer srv.Close()

	body, err := NewClient(srv.URL, "", time.Second).GetJSON(context.Background(), "/ready", nil)
	if err != nil || !json.Valid(body) || string(body) != `{"status":"not_ready","reason":"startup recovery"}` {
		t.Fatalf("GetJSON = %s, %v", body, err)
	}
}

func TestGetJSON_ClassifiesUnsupportedAndRejectsBadJSON(t *testing.T) {
	t.Run("unsupported endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		_, err := NewClient(srv.URL, "token", time.Second).GetJSON(context.Background(), "/api/v1/metrics/memory", nil)
		apiErr, ok := err.(*Error)
		if !ok || apiErr.Kind != KindUnsupported || UserMessage(apiErr) != "This Arc version does not expose the requested observability endpoint." {
			t.Fatalf("error = %#v", err)
		}
	})
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("invalid JSON status %d", status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, "not-json")
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, "", time.Second).GetJSON(context.Background(), "/ready", nil)
			apiErr, ok := err.(*Error)
			if !ok || apiErr.Kind != KindParse {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestGetJSON_RejectsEndpointInjection(t *testing.T) {
	c := NewClient("http://127.0.0.1", "", time.Second)
	for _, endpoint := range []string{"api/v1/metrics", "/api/v1/metrics?admin=true", "/health#fragment"} {
		if _, err := c.GetJSON(context.Background(), endpoint, nil); err == nil {
			t.Errorf("GetJSON(%q) accepted an invalid endpoint", endpoint)
		}
	}
}
