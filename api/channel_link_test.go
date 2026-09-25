package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// seedUser creates a User + Session in the database and returns a bearer token
// the test can use in the Authorization header.
func seedUser(t *testing.T, store *postgres.Store) (user *domain.User, token string) {
	t.Helper()
	user = &domain.User{ID: "u-test-1", Email: "test@example.com"}
	if err := store.SaveUser(context.Background(), user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	sess := &domain.Session{
		Token:     "test-session-token",
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	if err := store.SaveSession(context.Background(), sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return user, sess.Token
}

// authedRequest builds an httptest.Request with the Authorization header set.
func authedRequest(method, path string, body any, token string) *http.Request {
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// doJSONNoAuth sends a JSON request without any Authorization header.
func doJSONNoAuth(method, path string, body any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// openDB connects to DATABASE_URL and runs migrations, or skips the test.
func openDB(t *testing.T) (*postgres.Store, func()) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres not reachable: %v", err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	store := postgres.New(pool)
	cleanup := func() {
		_, _ = pool.Exec(ctx, `TRUNCATE messenger_channels, channel_link_codes, sessions, users CASCADE`)
		pool.Close()
	}
	_, _ = pool.Exec(ctx, `TRUNCATE messenger_channels, channel_link_codes, sessions, users CASCADE`)
	t.Cleanup(cleanup)
	return store, cleanup
}

// TestRedeemChannelByCode_Unauthenticated_Returns401 verifies that the
// redeem endpoint requires authentication.
func TestRedeemChannelByCode_Unauthenticated_Returns401(t *testing.T) {
	srv := api.NewServer(&stubStore{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, doJSONNoAuth(http.MethodPost, "/users/me/channels/redeem", map[string]string{"code": "abc"}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// TestRedeemChannelByCode_EmptyCode_Returns400 verifies that an empty code
// body returns 400.
func TestRedeemChannelByCode_EmptyCode_Returns400(t *testing.T) {
	srv := api.NewServer(&stubStoreWithUser{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/users/me/channels/redeem", map[string]string{"code": ""}, "valid-token"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty code, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRedeemChannelByCode_InvalidCode_Returns401 verifies that an unknown
// code returns 401 (invalid or expired).
func TestRedeemChannelByCode_InvalidCode_Returns401(t *testing.T) {
	srv := api.NewServer(&stubStoreWithUser{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/users/me/channels/redeem", map[string]string{"code": "notavalidcode12345678"}, "valid-token"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// TestRedeemChannelByCode_ValidCode_CreatesChannel verifies the happy path:
// a code seeded with (identity_id, channel_ref) is redeemed by an authed user
// and a MessengerChannel row is created.
func TestRedeemChannelByCode_ValidCode_CreatesChannel(t *testing.T) {
	store, _ := openDB(t)
	srv := api.NewServer(store)
	user, token := seedUser(t, store)

	// Seed a link code directly — the bot generates this in production.
	lc := &domain.ChannelLinkCode{
		Code:       "testlinkcode0001",
		IdentityID: "id-tg-bot",
		ChannelRef: "chat-999",
		ExpiresAt:  time.Now().Add(10 * time.Minute),
	}
	if err := store.SaveChannelLinkCode(context.Background(), lc); err != nil {
		t.Fatalf("seed link code: %v", err)
	}

	// User redeems the code via the web app.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/users/me/channels/redeem", map[string]string{"code": lc.Code}, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var ch domain.MessengerChannel
	if err := json.Unmarshal(rec.Body.Bytes(), &ch); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if ch.UserID != user.ID {
		t.Fatalf("expected UserID %q, got %q", user.ID, ch.UserID)
	}
	if ch.IdentityID != "id-tg-bot" || ch.ChannelRef != "chat-999" {
		t.Fatalf("expected {id-tg-bot, chat-999}, got {%s, %s}", ch.IdentityID, ch.ChannelRef)
	}

	// Confirm channel appears in list.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/users/me/channels", nil, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("list channels: expected 200, got %d", rec.Code)
	}
	var channels []domain.MessengerChannel
	if err := json.Unmarshal(rec.Body.Bytes(), &channels); err != nil {
		t.Fatalf("decode channels: %v", err)
	}
	if len(channels) != 1 || channels[0].IdentityID != "id-tg-bot" || channels[0].ChannelRef != "chat-999" {
		t.Fatalf("expected one channel {id-tg-bot, chat-999}, got %+v", channels)
	}
}

// TestRedeemChannelByCode_CodeIsOneTime_SecondRedeemReturns401 verifies that
// the code is deleted after first use.
func TestRedeemChannelByCode_CodeIsOneTime_SecondRedeemReturns401(t *testing.T) {
	store, _ := openDB(t)
	srv := api.NewServer(store)
	_, token := seedUser(t, store)

	lc := &domain.ChannelLinkCode{
		Code:       "onetime-code-001",
		IdentityID: "id-tg",
		ChannelRef: "chat-1",
		ExpiresAt:  time.Now().Add(10 * time.Minute),
	}
	if err := store.SaveChannelLinkCode(context.Background(), lc); err != nil {
		t.Fatalf("seed link code: %v", err)
	}

	body := map[string]string{"code": lc.Code}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/users/me/channels/redeem", body, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("first redeem: expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/users/me/channels/redeem", body, token))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second redeem: expected 401, got %d", rec.Code)
	}
}

// TestListMyChannels_EmptyList verifies that a user with no channels gets [].
func TestListMyChannels_EmptyList(t *testing.T) {
	store, _ := openDB(t)
	srv := api.NewServer(store)
	_, token := seedUser(t, store)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/users/me/channels", nil, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("expected empty array, got %q", got)
	}
}

// TestDeleteMyChannel_RemovesChannel verifies that DELETE removes the channel.
func TestDeleteMyChannel_RemovesChannel(t *testing.T) {
	store, _ := openDB(t)
	srv := api.NewServer(store)
	user, token := seedUser(t, store)

	ch := &domain.MessengerChannel{
		ID:         "ch-001",
		UserID:     user.ID,
		IdentityID: "id-tg",
		ChannelRef: "chat-42",
	}
	if err := store.SaveMessengerChannel(context.Background(), ch); err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/users/me/channels/ch-001", nil, token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/users/me/channels", nil, token))
	var channels []domain.MessengerChannel
	json.Unmarshal(rec.Body.Bytes(), &channels) //nolint:errcheck
	if len(channels) != 0 {
		t.Fatalf("expected empty list after delete, got %+v", channels)
	}
}

// TestDeleteMyChannel_OtherUserChannel_Returns404 verifies channel ownership.
func TestDeleteMyChannel_OtherUserChannel_Returns404(t *testing.T) {
	store, _ := openDB(t)
	srv := api.NewServer(store)
	ctx := context.Background()

	alice := &domain.User{ID: "u-alice", Email: "alice@example.com"}
	bob := &domain.User{ID: "u-bob", Email: "bob@example.com"}
	store.SaveUser(ctx, alice) //nolint:errcheck
	store.SaveUser(ctx, bob)   //nolint:errcheck

	sess := &domain.Session{Token: "alice-token", UserID: alice.ID, ExpiresAt: time.Now().Add(time.Hour)}
	store.SaveSession(ctx, sess) //nolint:errcheck

	ch := &domain.MessengerChannel{ID: "ch-bob", UserID: bob.ID, IdentityID: "id-tg", ChannelRef: "chat-bob"}
	store.SaveMessengerChannel(ctx, ch) //nolint:errcheck

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/users/me/channels/ch-bob", nil, "alice-token"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
