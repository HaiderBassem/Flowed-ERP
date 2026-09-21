package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
	"flowed/internal/platform/config"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

func testTokenService() *auth.TokenService {
	return auth.NewTokenService(config.Auth{
		JWTSecret:       "test-secret-that-is-at-least-thirty-two-bytes",
		AccessTokenTTL:  time.Hour,
		RefreshTokenTTL: 24 * time.Hour,
		Issuer:          "flowed-test",
		BcryptCost:      4,
	})
}

func operator() *port.User {
	return &port.User{
		ID:       shared.NewID(),
		Username: "operator.ali",
		Roles:    []shared.Role{shared.RoleAdmin},
		IsActive: true,
	}
}

// guardedRouter mounts one endpoint behind Authenticate.
//
// There is no role middleware left to mount behind it: with one account every
// signed-in operator may do everything, and authority is recorded in the audit
// trail rather than checked at the edge.
func guardedRouter(ts *auth.TokenService) (*gin.Engine, *shared.Actor) {
	gin.SetMode(gin.TestMode)
	var seen shared.Actor

	router := gin.New()
	router.Use(httpx.RequestID())
	group := router.Group("/", httpx.Authenticate(ts))
	group.POST("/payments", func(c *gin.Context) {
		seen = httpx.MustActor(c)
		httpx.Created(c, gin.H{"ok": true})
	})
	return router, &seen
}

func authedPost(t *testing.T, router *gin.Engine, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/payments", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.RemoteAddr = "10.20.30.40:54321"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticatePopulatesActor(t *testing.T) {
	ts := testTokenService()
	user := operator()

	token, _, err := ts.Issue(user)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	router, seen := guardedRouter(ts)
	rec := authedPost(t, router, "Bearer "+token)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if seen.UserID != user.ID {
		t.Errorf("user id: got %s, want %s", seen.UserID, user.ID)
	}
	// The audit trail needs the origin, and only the transport knows it.
	if seen.IPAddress != "10.20.30.40" {
		t.Errorf("ip address: got %q, want 10.20.30.40", seen.IPAddress)
	}
}

func TestAuthenticateRejectsBadCredentials(t *testing.T) {
	ts := testTokenService()

	refresh, _, err := ts.IssueRefresh(operator())
	if err != nil {
		t.Fatalf("issuing the refresh token: %v", err)
	}

	cases := map[string]struct {
		authorization string
		wantCode      string
	}{
		"no header":       {"", "auth.missing_token"},
		"wrong scheme":    {"Basic abcdefgh", "auth.malformed_authorization_header"},
		"bearer no token": {"Bearer ", "auth.malformed_authorization_header"},
		"garbage token":   {"Bearer not-a-jwt", "auth.token_malformed"},
		"refresh token":   {"Bearer " + refresh, "auth.token_wrong_type"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := guardedRouter(ts)
			rec := authedPost(t, router, tc.authorization)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec).Code; got != tc.wantCode {
				t.Errorf("code: got %q, want %q", got, tc.wantCode)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("a 401 should name the scheme in WWW-Authenticate")
			}
		})
	}
}

// TestActorFromReachesTheRequestContext proves a service given only the
// context.Context — which is all a command handler ever sees — can still name
// the actor.
func TestActorFromReachesTheRequestContext(t *testing.T) {
	ts := testTokenService()
	user := operator()

	token, _, err := ts.Issue(user)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	var fromRequestContext shared.Actor
	var found bool
	router.POST("/payments", httpx.Authenticate(ts), func(c *gin.Context) {
		fromRequestContext, found = httpx.ActorFrom(c.Request.Context())
		httpx.NoContent(c)
	})

	rec := authedPost(t, router, "Bearer "+token)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	if !found {
		t.Fatal("the actor was not reachable from the request context")
	}
	if fromRequestContext.UserID != user.ID {
		t.Errorf("user id: got %s, want %s", fromRequestContext.UserID, user.ID)
	}
}
