package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

const testID = "26b9ff24-75ad-40c8-abaa-dea745979bb8"

// routingTestServer mirrors the shapes of route the server registers: a
// parameter that is the last piece of its route, one that has children, and an
// any-route whose parameter is meant to hold the rest of the path.
func routingTestServer() (*echo.Echo, *bool) {
	e := echo.New()
	e.Use(RejectMultiSegmentParams)

	reached := false
	handler := func(c echo.Context) error {
		reached = true
		return c.String(http.StatusOK, c.Param("groupID")+c.Param("jobID")+c.Param("*"))
	}

	e.GET("/job-groups/:groupID", handler) // leaf parameter
	e.GET("/jobs/:jobID", handler)         // parameter with children
	e.GET("/jobs/:jobID/logs", handler)
	e.GET("/swagger/*", handler) // wildcard, exempt

	return e, &reached
}

func get(e *echo.Echo, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Echo lets a path parameter that is the last piece of a route match the whole
// remaining path, slashes included, so /job-groups/{id}/results matched
// /job-groups/:groupID and bound "{id}/results" as the group id. The handler
// then answered for a group nobody asked for and reported it as missing.
func TestMultiSegmentParamDoesNotReachTheHandler(t *testing.T) {
	for _, path := range []string{
		"/job-groups/" + testID + "/results",
		"/job-groups/" + testID + "/logs",
		"/job-groups/" + testID + "/metadata",
		"/job-groups/" + testID + "/a/b/c",
	} {
		e, reached := routingTestServer()

		if rec := get(e, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
		if *reached {
			t.Errorf("GET %s reached the handler", path)
		}
	}
}

// The point of doing this globally: a path that does not exist answers the same
// way whether or not the route it brushed against happens to have children.
// Job groups are not a special case.
func TestNotFoundIsIndistinguishable(t *testing.T) {
	e, _ := routingTestServer()

	// Swallowed by a leaf parameter, and matched by no route at all.
	guarded := get(e, "/job-groups/"+testID+"/results")
	ordinary := get(e, "/jobs/"+testID+"/nonsense")

	if guarded.Code != ordinary.Code {
		t.Errorf("status %d for a leaf parameter, %d for an unmatched path", guarded.Code, ordinary.Code)
	}
	if guarded.Body.String() != ordinary.Body.String() {
		t.Errorf("body %q for a leaf parameter, %q for an unmatched path", guarded.Body.String(), ordinary.Body.String())
	}
}

func TestOrdinaryRoutesAreUnaffected(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{path: "/job-groups/" + testID, want: testID},
		{path: "/jobs/" + testID, want: testID},
		{path: "/jobs/" + testID + "/logs", want: testID},
	}

	for _, tc := range cases {
		e, reached := routingTestServer()

		rec := get(e, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, http.StatusOK)
		}
		if !*reached {
			t.Errorf("GET %s did not reach the handler", tc.path)
		}
		if rec.Body.String() != tc.want {
			t.Errorf("GET %s bound %q, want %q", tc.path, rec.Body.String(), tc.want)
		}
	}
}

// An any-route's parameter holds the rest of the path by design, which is how
// /swagger/* and the static file routes work.
func TestWildcardParamKeepsTheRestOfThePath(t *testing.T) {
	e, reached := routingTestServer()

	rec := get(e, "/swagger/index.html")
	if rec.Code != http.StatusOK || !*reached {
		t.Fatalf("GET /swagger/index.html = %d, reached = %v", rec.Code, *reached)
	}

	if rec = get(e, "/swagger/a/b/c"); rec.Body.String() != "a/b/c" {
		t.Errorf("wildcard bound %q, want %q", rec.Body.String(), "a/b/c")
	}
}
