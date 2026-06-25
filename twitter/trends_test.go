package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetPersonalizedTrendsNoParams(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]Trend]{Data: []Trend{
			{TrendName: "#golang", Category: "Technology", PostCount: 1234},
			{TrendName: "#x"},
		}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	trends, err := c.GetPersonalizedTrends(context.Background())
	if err != nil {
		t.Fatalf("GetPersonalizedTrends: %v", err)
	}
	if len(trends) != 2 || trends[0].TrendName != "#golang" || trends[0].PostCount != 1234 {
		t.Fatalf("unexpected trends: %#v", trends)
	}
	if gotMethod != http.MethodGet || gotPath != "/2/users/personalized_trends" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	// This endpoint takes no parameters: the query must be empty.
	if gotQuery != "" {
		t.Fatalf("expected empty query, got %q", gotQuery)
	}
}
