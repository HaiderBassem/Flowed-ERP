package httpapi

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/httpx"
	"github.com/swibit/flowed/internal/port"
)

// AuthHandlers issues and renews credentials.
type AuthHandlers struct {
	Users  port.UserRepository
	Tokens *auth.TokenService
	Hasher *auth.Hasher
	Clock  shared.Clock
	Log    *slog.Logger
}

// NewAuthHandlers wires the credential endpoints.
func NewAuthHandlers(users port.UserRepository, tokens *auth.TokenService, hasher *auth.Hasher, clock shared.Clock, log *slog.Logger) *AuthHandlers {
	return &AuthHandlers{Users: users, Tokens: tokens, Hasher: hasher, Clock: clock, Log: log}
}

// Login exchanges a username and password for tokens.
//
// Every failure returns the same message and takes roughly the same time. An
// unknown username runs a dummy hash comparison, so the response cannot be
// used to enumerate who works here — and in a university finance office,
// knowing which accounts exist is the first half of an attack.
func (h *AuthHandlers) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := requestContext(c)

	invalid := shared.Unauthorized("auth.invalid_credentials", "the username or password is incorrect")

	user, err := h.Users.GetByUsername(ctx, req.Username)
	if err != nil {
		if verifyErr := h.Hasher.VerifyDummy(); verifyErr != nil {
			h.Log.DebugContext(ctx, "dummy verification failed", slog.String("error", verifyErr.Error()))
		}
		h.logFailure(c, req.Username, "unknown username")
		httpx.Respond(c, invalid)
		return
	}

	if err := h.Hasher.Verify(user.PasswordHash, req.Password); err != nil {
		h.logFailure(c, req.Username, "password mismatch")
		httpx.Respond(c, invalid)
		return
	}

	// Checked after the password, so a disabled account cannot be told apart
	// from a wrong password by an attacker who has neither.
	if !user.IsActive {
		h.logFailure(c, req.Username, "account disabled")
		httpx.Respond(c, invalid)
		return
	}

	var deskID *shared.ID
	if req.CashierDeskID != nil {
		parsed, err := shared.ParseID(*req.CashierDeskID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		deskID = &parsed
	}

	// A cashier without a desk cannot take cash: receipt series run per year
	// per desk, and a collection with no desk has no sequential paper book to
	// reconcile against.
	if user.HasRole(shared.RoleCashier) && deskID == nil {
		httpx.Respond(c, shared.Validation("auth.cashier_desk_required",
			"a cashier must sign in at a specific desk; receipt numbering is per desk").
			WithDetail("field", "cashier_desk_id"))
		return
	}

	pair, err := h.Tokens.IssuePair(user, deskID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	// A failure to stamp the login must not deny a cashier their shift.
	if err := h.Users.RecordLogin(ctx, user.ID, h.now()); err != nil {
		h.Log.WarnContext(ctx, "recording login timestamp",
			slog.String("username", user.Username), slog.String("error", err.Error()))
	}

	httpx.OK(c, TokenResponse{
		AccessToken:      pair.AccessToken,
		AccessExpiresAt:  pair.AccessExpiresAt,
		RefreshToken:     pair.RefreshToken,
		RefreshExpiresAt: pair.RefreshExpiresAt,
		TokenType:        "Bearer",
		User:             toUserView(user),
	})
}

// Refresh renews an access token from a refresh token.
//
// The user is re-read rather than trusted from the token. A refresh token
// issued last month must not keep working after the account was disabled or
// its roles were narrowed, and only a fresh read can notice either.
func (h *AuthHandlers) Refresh(c *gin.Context) {
	var req RefreshRequest
	if !bindJSON(c, &req) {
		return
	}
	ctx := requestContext(c)

	claims, err := h.Tokens.ParseRefresh(req.RefreshToken)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	userID, err := shared.ParseID(claims.Subject)
	if err != nil {
		httpx.Respond(c, shared.Unauthorized("auth.invalid_token", "the refresh token is malformed"))
		return
	}

	user, err := h.Users.GetByID(ctx, userID)
	if err != nil {
		httpx.Respond(c, shared.Unauthorized("auth.invalid_token", "the refresh token is no longer valid"))
		return
	}
	if !user.IsActive {
		httpx.Respond(c, shared.Unauthorized("auth.account_disabled", "this account is disabled"))
		return
	}

	var deskID *shared.ID
	if claims.CashierDeskID != nil {
		parsed, err := shared.ParseID(*claims.CashierDeskID)
		if err == nil {
			deskID = &parsed
		}
	}

	// The session id carries across, so rotating an access token does not
	// fragment the audit trail's view of one login.
	access, expiresAt, err := h.Tokens.RenewFromRefresh(user, deskID, claims.SessionID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	httpx.OK(c, TokenResponse{
		AccessToken:     access,
		AccessExpiresAt: expiresAt,
		TokenType:       "Bearer",
		User:            toUserView(user),
	})
}

// Me returns the signed-in operator, so a client can render the interface its
// actor is actually allowed to use rather than offering buttons the server
// will refuse.
func (h *AuthHandlers) Me(c *gin.Context) {
	actor := httpx.MustActor(c)

	roles := make([]string, 0, len(actor.Roles))
	for _, r := range actor.Roles {
		roles = append(roles, string(r))
	}

	payload := gin.H{
		"id":       actor.UserID.String(),
		"username": actor.Username,
		"roles":    roles,
	}
	if actor.CashierDeskID != nil {
		payload["cashier_desk_id"] = actor.CashierDeskID.String()
	}
	httpx.OK(c, payload)
}

func (h *AuthHandlers) logFailure(c *gin.Context, username, reason string) {
	// The username is recorded; the password never is, not even its length.
	h.Log.WarnContext(c.Request.Context(), "authentication failed",
		slog.String("username", username),
		slog.String("reason", reason),
		slog.String("client_ip", c.ClientIP()))
}

func (h *AuthHandlers) now() time.Time {
	if h.Clock == nil {
		return time.Now().UTC()
	}
	return h.Clock.Now()
}

func toUserView(u *port.User) UserView {
	roles := make([]string, 0, len(u.Roles))
	for _, r := range u.Roles {
		roles = append(roles, string(r))
	}
	return UserView{
		ID:       u.ID.String(),
		Username: u.Username,
		FullName: u.FullName,
		Roles:    roles,
	}
}
