package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ── Lists CRUD ───────────────────────────────────────────────────────────────

func TestCreateListPostsNameAndParsesData(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusCreated, dataEnvelope[List]{Data: List{ID: "L1", Name: "team"}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	l, err := c.CreateList(context.Background(), &CreateListRequest{Name: "team"})
	if err != nil {
		t.Fatalf("CreateList: %v", err)
	}
	if l.ID != "L1" || l.Name != "team" {
		t.Fatalf("unexpected list: %#v", l)
	}
	if gotMethod != http.MethodPost || gotPath != "/2/lists" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	// private omitted when nil; only name present.
	if strings.TrimSpace(gotBody) != `{"name":"team"}` {
		t.Fatalf("body = %q, want %q", gotBody, `{"name":"team"}`)
	}
}

func TestCreateListValidatesName(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.CreateList(context.Background(), &CreateListRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for empty name, got %v", err)
	}
	if _, err := c.CreateList(context.Background(), nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for nil, got %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint called despite invalid request")
	}
}

func TestGetListParsesData(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[List]{Data: List{ID: "L1", Name: "team", MemberCount: 3}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	l, err := c.GetList(context.Background(), "L1")
	if err != nil {
		t.Fatalf("GetList: %v", err)
	}
	if l.ID != "L1" || l.MemberCount != 3 {
		t.Fatalf("unexpected list: %#v", l)
	}
	if gotMethod != http.MethodGet || gotPath != "/2/lists/L1" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
}

func TestUpdateListParsesUpdated(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, dataEnvelope[updatedResult]{Data: updatedResult{Updated: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	name := "renamed"
	ok, err := c.UpdateList(context.Background(), "L1", &UpdateListRequest{Name: &name})
	if err != nil {
		t.Fatalf("UpdateList: %v", err)
	}
	if !ok {
		t.Fatal("expected updated=true")
	}
	if gotMethod != http.MethodPut || gotPath != "/2/lists/L1" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if strings.TrimSpace(gotBody) != `{"name":"renamed"}` {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestUpdateListNilRejectedEmptySent(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, dataEnvelope[updatedResult]{Data: updatedResult{Updated: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	// nil req → ErrInvalidRequest, no HTTP call.
	if _, err := c.UpdateList(context.Background(), "L1", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for nil req, got %v", err)
	}
	// empty (non-nil) req → sends {}, no error.
	if _, err := c.UpdateList(context.Background(), "L1", &UpdateListRequest{}); err != nil {
		t.Fatalf("empty req should be valid: %v", err)
	}
	if strings.TrimSpace(gotBody) != `{}` {
		t.Fatalf("empty req should send {}, got %q", gotBody)
	}
}

func TestDeleteListParsesDeletedNoBody(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, dataEnvelope[deleteResult]{Data: deleteResult{Deleted: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.DeleteList(context.Background(), "L1")
	if err != nil {
		t.Fatalf("DeleteList: %v", err)
	}
	if !ok {
		t.Fatal("expected deleted=true")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/lists/L1" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody != "" || gotCT != "" {
		t.Fatalf("DELETE must send no body and no Content-Type: body=%q ct=%q", gotBody, gotCT)
	}
}

// ── Membership writers ───────────────────────────────────────────────────────

func TestAddListMemberPostsUserID(t *testing.T) {
	var gotMethod, gotPath string
	var got userIDBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, dataEnvelope[memberResult]{Data: memberResult{IsMember: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.AddListMember(context.Background(), "L1", "U2")
	if err != nil {
		t.Fatalf("AddListMember: %v", err)
	}
	if !ok {
		t.Fatal("expected is_member=true")
	}
	if gotMethod != http.MethodPost || gotPath != "/2/lists/L1/members" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if got.UserID != "U2" {
		t.Fatalf("body = %#v, want user_id=U2", got)
	}
}

func TestRemoveListMemberDeletesByPathNoBody(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, dataEnvelope[memberResult]{Data: memberResult{IsMember: false}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	stillMember, err := c.RemoveListMember(context.Background(), "L1", "U2")
	if err != nil {
		t.Fatalf("RemoveListMember: %v", err)
	}
	if stillMember {
		t.Fatal("expected is_member=false on removal")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/lists/L1/members/U2" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody != "" || gotCT != "" {
		t.Fatalf("DELETE must send no body and no Content-Type: body=%q ct=%q", gotBody, gotCT)
	}
}

func TestAddListMemberValidates(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.AddListMember(context.Background(), "", "U2"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty listID: %v", err)
	}
	if _, err := c.AddListMember(context.Background(), "L1", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty userID: %v", err)
	}
}

// ── Paginated member/follower reads → UserPage ───────────────────────────────

func TestGetListMembersPagesAndParsesCursor(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{
			Data: []User{{ID: "1"}, {ID: "2"}},
			Meta: &PageMeta{ResultCount: 2, NextToken: "NEXT9"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetListMembers(context.Background(), "L1", &PageParams{MaxResults: 50, PaginationToken: "CUR0"})
	if err != nil {
		t.Fatalf("GetListMembers: %v", err)
	}
	if gotPath != "/2/lists/L1/members" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(page.Users) != 2 || page.Meta == nil || page.Meta.NextToken != "NEXT9" {
		t.Fatalf("unexpected page: %#v meta=%#v", page.Users, page.Meta)
	}
	if !strings.Contains(gotQuery, "pagination_token=CUR0") {
		t.Fatalf("expected pagination_token in query, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "next_token=") {
		t.Fatalf("request must not send next_token: %q", gotQuery)
	}
}

func TestGetListFollowersReturnsUserPage(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{Data: []User{{ID: "7"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetListFollowers(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("GetListFollowers: %v", err)
	}
	if gotPath != "/2/lists/L1/followers" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(page.Users) != 1 || page.Users[0].ID != "7" {
		t.Fatalf("unexpected page: %#v", page.Users)
	}
}

func TestGetListTweetsReturnsTweetPage(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{
			Data: []Tweet{{ID: "1", Text: "a"}},
			Meta: &PageMeta{NextToken: "T-NEXT"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetListTweets(context.Background(), "L1", &TimelineParams{PaginationToken: "C0"})
	if err != nil {
		t.Fatalf("GetListTweets: %v", err)
	}
	if gotPath != "/2/lists/L1/tweets" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(page.Tweets) != 1 || page.Meta == nil || page.Meta.NextToken != "T-NEXT" {
		t.Fatalf("unexpected page: %#v meta=%#v", page.Tweets, page.Meta)
	}
	if !strings.Contains(gotQuery, "pagination_token=C0") {
		t.Fatalf("expected pagination_token, got %q", gotQuery)
	}
}

// ── User-side list collections → ListPage ────────────────────────────────────

func TestGetOwnedListsPagesAndParses(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]List]{
			Data: []List{{ID: "L1", Name: "a"}, {ID: "L2", Name: "b"}},
			Meta: &PageMeta{NextToken: "LN"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetOwnedLists(context.Background(), "U1", &PageParams{PaginationToken: "C1"})
	if err != nil {
		t.Fatalf("GetOwnedLists: %v", err)
	}
	if gotPath != "/2/users/U1/owned_lists" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(page.Lists) != 2 || page.Meta == nil || page.Meta.NextToken != "LN" {
		t.Fatalf("unexpected page: %#v meta=%#v", page.Lists, page.Meta)
	}
	if !strings.Contains(gotQuery, "pagination_token=C1") {
		t.Fatalf("expected pagination_token, got %q", gotQuery)
	}
}

func TestGetListMembershipsPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]List]{Data: []List{{ID: "L3"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetListMemberships(context.Background(), "U1", nil)
	if err != nil {
		t.Fatalf("GetListMemberships: %v", err)
	}
	if gotPath != "/2/users/U1/list_memberships" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(page.Lists) != 1 {
		t.Fatalf("unexpected page: %#v", page.Lists)
	}
}

func TestGetFollowedListsPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]List]{Data: []List{{ID: "L4"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.GetFollowedLists(context.Background(), "U1", nil); err != nil {
		t.Fatalf("GetFollowedLists: %v", err)
	}
	if gotPath != "/2/users/U1/followed_lists" {
		t.Fatalf("path = %q", gotPath)
	}
}

// ── Follow / pin writers (both share listIDBody → {list_id}) ─────────────────

func TestFollowListPostsListID(t *testing.T) {
	var gotMethod, gotPath string
	var got listIDBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, dataEnvelope[followResult]{Data: followResult{Following: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.FollowList(context.Background(), "U1", "L2")
	if err != nil {
		t.Fatalf("FollowList: %v", err)
	}
	if !ok {
		t.Fatal("expected following=true")
	}
	if gotMethod != http.MethodPost || gotPath != "/2/users/U1/followed_lists" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if got.ListID != "L2" {
		t.Fatalf("body = %#v, want list_id=L2", got)
	}
}

func TestUnfollowListDeletesByPathNoBody(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, dataEnvelope[followResult]{Data: followResult{Following: false}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	following, err := c.UnfollowList(context.Background(), "U1", "L2")
	if err != nil {
		t.Fatalf("UnfollowList: %v", err)
	}
	if following {
		t.Fatal("expected following=false")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/users/U1/followed_lists/L2" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody != "" || gotCT != "" {
		t.Fatalf("DELETE must send no body and no Content-Type: body=%q ct=%q", gotBody, gotCT)
	}
}

func TestPinListPostsListID(t *testing.T) {
	var gotMethod, gotPath string
	var got listIDBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, dataEnvelope[pinnedResult]{Data: pinnedResult{Pinned: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.PinList(context.Background(), "U1", "L2")
	if err != nil {
		t.Fatalf("PinList: %v", err)
	}
	if !ok {
		t.Fatal("expected pinned=true")
	}
	if gotMethod != http.MethodPost || gotPath != "/2/users/U1/pinned_lists" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if got.ListID != "L2" {
		t.Fatalf("body = %#v, want list_id=L2", got)
	}
}

func TestUnpinListDeletesByExactPath(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, dataEnvelope[pinnedResult]{Data: pinnedResult{Pinned: false}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	pinned, err := c.UnpinList(context.Background(), "U1", "L2")
	if err != nil {
		t.Fatalf("UnpinList: %v", err)
	}
	if pinned {
		t.Fatal("expected pinned=false")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/users/U1/pinned_lists/L2" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody != "" || gotCT != "" {
		t.Fatalf("DELETE must send no body and no Content-Type: body=%q ct=%q", gotBody, gotCT)
	}
}

// ── GetPinnedLists: no cursor, returns a plain slice ─────────────────────────

func TestGetPinnedListsNoCursorReturnsSlice(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]List]{Data: []List{{ID: "L1"}, {ID: "L2"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	lists, err := c.GetPinnedLists(context.Background(), "U1")
	if err != nil {
		t.Fatalf("GetPinnedLists: %v", err)
	}
	if len(lists) != 2 {
		t.Fatalf("expected 2 lists, got %d", len(lists))
	}
	if gotPath != "/2/users/U1/pinned_lists" {
		t.Fatalf("path = %q", gotPath)
	}
	// No cursor on this endpoint: no pagination_token / next_token in the query.
	if strings.Contains(gotQuery, "pagination_token") || strings.Contains(gotQuery, "next_token") {
		t.Fatalf("pinned_lists must not send a cursor, got %q", gotQuery)
	}
}

func TestListReadsValidateIDs(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.GetList(context.Background(), ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetList empty id: %v", err)
	}
	if _, err := c.GetListMembers(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetListMembers empty id: %v", err)
	}
	if _, err := c.GetOwnedLists(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetOwnedLists empty id: %v", err)
	}
	if _, err := c.GetPinnedLists(context.Background(), ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetPinnedLists empty id: %v", err)
	}
}

// ── Wire-shape: List.Private keeps an explicit false on re-marshal ───────────

func TestListPrivateNotOmitted(t *testing.T) {
	b, _ := json.Marshal(List{ID: "L1", Name: "team"})
	if !strings.Contains(string(b), `"private":false`) {
		t.Fatalf("List must keep explicit private:false, got %s", b)
	}
}
