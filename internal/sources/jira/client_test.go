package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClient_SearchJQL_Paginates(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u@x.com:tok"))
		if auth != want {
			t.Errorf("auth: got %q want %q", auth, want)
		}
		var body SearchRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.NextPageToken == "" {
			_ = json.NewEncoder(w).Encode(SearchResponse{
				Issues:        []Issue{{Key: "PRJ-1"}, {Key: "PRJ-2"}},
				NextPageToken: "page2",
			})
			return
		}
		if body.NextPageToken != "page2" {
			t.Errorf("unexpected next page token %q", body.NextPageToken)
		}
		_ = json.NewEncoder(w).Encode(SearchResponse{
			Issues: []Issue{{Key: "PRJ-3"}},
			IsLast: true,
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Email: "u@x.com", Token: "tok"})
	ctx := context.Background()

	var all []Issue
	tok := ""
	for {
		resp, err := c.SearchJQL(ctx, SearchRequest{JQL: "project = PRJ", MaxResults: 2, NextPageToken: tok})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, resp.Issues...)
		if resp.NextPageToken == "" {
			break
		}
		tok = resp.NextPageToken
	}
	if len(all) != 3 {
		t.Errorf("expected 3 issues across pages, got %d", len(all))
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 requests, got %d", calls.Load())
	}
}

func TestClient_RetriesOn429WithRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(SearchResponse{Issues: []Issue{{Key: "K-1"}}})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Email: "u", Token: "t", MaxRetries: 3})
	resp, err := c.SearchJQL(context.Background(), SearchRequest{JQL: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Issues) != 1 {
		t.Errorf("want 1 issue, got %d", len(resp.Issues))
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts: got %d want 2", attempts.Load())
	}
}

func TestClient_GetIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PLAT-1") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if expand := r.URL.Query().Get("expand"); !strings.Contains(expand, "comments") {
			t.Errorf("expand missing comments: %q", expand)
		}
		_, _ = w.Write([]byte(`{"id":"1","key":"PLAT-1","fields":{"summary":"hello","updated":"2026-01-01T00:00:00.000+0000"}}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Email: "u", Token: "t"})
	iss, err := c.GetIssue(context.Background(), "PLAT-1")
	if err != nil {
		t.Fatal(err)
	}
	if iss.Key != "PLAT-1" || iss.Fields.Summary != "hello" {
		t.Errorf("unexpected issue: %+v", iss)
	}
}

func TestIsTokenError(t *testing.T) {
	err := &apiError{Status: 400, Body: "nextPageToken is invalid"}
	if !IsTokenError(err) {
		t.Error("expected token error detection")
	}
	err = &apiError{Status: 400, Body: "something else"}
	if IsTokenError(err) {
		t.Error("should not flag unrelated 400s")
	}
	err = &apiError{Status: 500, Body: "token rejected"}
	if IsTokenError(err) {
		t.Error("should not flag 5xx")
	}
}
