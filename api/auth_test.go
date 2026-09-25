package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unitz007/open-kael/api"
)

// stubStore satisfies domain.Store with no-op implementations — enough to
// construct an api.Server without a real database for auth-only tests.
type stubStore struct{ noopStore }

func TestBearerAuth_NoToken_AllowsRequest(t *testing.T) {
	s := api.NewServer(&stubStore{})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if rr.Code == http.StatusUnauthorized {
		t.Fatal("expected no auth when WithBearerAuth not set, got 401")
	}
}

func TestBearerAuth_MissingHeader_Returns401(t *testing.T) {
	s := api.NewServer(&stubStore{}, api.WithBearerAuth("secret"))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestBearerAuth_WrongToken_Returns401(t *testing.T) {
	s := api.NewServer(&stubStore{}, api.WithBearerAuth("secret"))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestBearerAuth_CorrectToken_AllowsRequest(t *testing.T) {
	s := api.NewServer(&stubStore{}, api.WithBearerAuth("secret"))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.ServeHTTP(rr, req)
	if rr.Code == http.StatusUnauthorized {
		t.Fatal("expected request to pass with correct token, got 401")
	}
}
