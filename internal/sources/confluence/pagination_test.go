package confluence

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Confluence Cloud returns _links.next with the /wiki context path; the client
// base URL already ends in /wiki, so the second page must not request
// /wiki/wiki/... (it 404s).
func TestListPagesInSpaceFollowsNextLinkUnderWiki(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		if r.URL.Path != "/wiki/api/v2/spaces/123/pages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			w.Write([]byte(`{"results":[{"id":"1"}],"_links":{"next":"/wiki/api/v2/spaces/123/pages?limit=100&cursor=abc"}}`))
			return
		}
		w.Write([]byte(`{"results":[{"id":"2"}],"_links":{}}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL + "/wiki", Email: "e", Token: "t"})
	first, err := c.ListPagesInSpace(context.Background(), "123", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.ListPagesInSpace(context.Background(), "123", first.Links.Next)
	if err != nil {
		t.Fatalf("second page: %v (requests: %v)", err, seen)
	}
	if len(second.Results) != 1 {
		t.Fatalf("second page results = %d", len(second.Results))
	}
}

func TestNextPathRelative(t *testing.T) {
	cases := map[string]string{
		"/wiki/api/v2/spaces/1/pages?cursor=x": "/api/v2/spaces/1/pages?cursor=x",
		"/api/v2/spaces/1/pages?cursor=x":      "/api/v2/spaces/1/pages?cursor=x",
		"":                                     "",
	}
	for in, want := range cases {
		if got := nextPathRelative(in); got != want {
			t.Errorf("nextPathRelative(%q) = %q, want %q", in, got, want)
		}
	}
}
