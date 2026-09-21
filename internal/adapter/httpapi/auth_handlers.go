package httpapi

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

// AuthHandlers issue, renew and withdraw credentials.
//
// The handlers hold no policy of their own. Throttling, the session record,
// which failures are indistinguishable from which, and the audit entry all live
// in the application service, so a second entry point — the CLI, the student
// portal — gets the same behaviour rather than a second implementation of it.
type AuthHandlers struct {
	Auth  *app.AuthService
	Users *app.UserService
	Log   *slog.Logger
}

// NewAuthHandlers wires the credential endpoints.
func NewAuthHandlers(authService *app.AuthService, users *app.UserService, log *slog.Logger) *AuthHandlers {
	return &AuthHandlers{Auth: authService, Users: users, Log: log}
}

// Login exchanges a username and password for tokens.
func (h *AuthHandlers) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}

	result, err := h.Auth.Login(requestContext(c), app.LoginInput{
		Username:  req.Username,
		Password:  req.Password,
		IPAddress: c.ClientIP(),
		UserAgent: c.GetHeader("User-Agent"),
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	httpx.OK(c, TokenResponse{
		AccessToken:      result.Pair.AccessToken,
		AccessExpiresAt:  result.Pair.AccessExpiresAt,
		RefreshToken:     result.Pair.RefreshToken,
		RefreshExpiresAt: result.Pair.RefreshExpiresAt,
		TokenType:        "Bearer",
		User:             toUserView(result.User),
	})
}

// Refresh renews an access token from a refresh token.
func (h *AuthHandlers) Refresh(c *gin.Context) {
	var req RefreshRequest
	if !bindJSON(c, &req) {
		return
	}

	result, err := h.Auth.Refresh(requestContext(c), app.RefreshInput{
		RefreshToken: req.RefreshToken,
		IPAddress:    c.ClientIP(),
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	httpx.OK(c, TokenResponse{
		AccessToken:     result.AccessToken,
		AccessExpiresAt: result.ExpiresAt,
		TokenType:       "Bearer",
		User:            toUserView(result.User),
	})
}

// Logout ends the session the request arrived on.
func (h *AuthHandlers) Logout(c *gin.Context) {
	if err := h.Auth.Logout(requestContext(c), httpx.MustActor(c)); err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, gin.H{"signed_out": true})
}

// Me returns the signed-in operator, so a client can render the interface its
// actor is actually allowed to use rather than offering buttons the server will
// refuse.
//
// It reads the user row rather than answering from the token alone: the token
// is up to one access lifetime stale, and this is the response a UI decides its
// whole navigation from.
func (h *AuthHandlers) Me(c *gin.Context) {
	actor := httpx.MustActor(c)

	user, err := h.Users.GetUser(requestContext(c), actor, actor.UserID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	view := toUserDetailView(user)
	view.SessionID = actor.SessionID
	httpx.OK(c, view)
}

// ChangePassword lets an operator replace their own password.
//
// Mounted outside the password-change gate: an account holding a credential
// somebody else set has to be able to do this much and nothing else.
func (h *AuthHandlers) ChangePassword(c *gin.Context) {
	var req ChangePasswordRequest
	if !bindJSON(c, &req) {
		return
	}

	err := h.Users.ChangeOwnPassword(requestContext(c), httpx.MustActor(c), app.ChangeOwnPasswordInput{
		CurrentPassword:   req.CurrentPassword,
		NewPassword:       req.NewPassword,
		KeepOtherSessions: req.KeepOtherSessions,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, gin.H{"password_changed": true})
}

// MySessions lists the caller's own sign-ins, so an operator can see a session
// they do not recognise and end it.
func (h *AuthHandlers) MySessions(c *gin.Context) {
	actor := httpx.MustActor(c)

	sessions, err := h.Users.ListSessions(requestContext(c), actor, actor.UserID, queryBool(c, "include_ended"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toSessionViews(sessions, actor.SessionID))
}

// RevokeMyOtherSessions signs the caller out everywhere but here.
func (h *AuthHandlers) RevokeMyOtherSessions(c *gin.Context) {
	revoked, err := h.Users.RevokeOtherSessions(requestContext(c), httpx.MustActor(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, gin.H{"sessions_revoked": revoked})
}

func toUserView(u *port.User) UserView {
	roles := make([]string, 0, len(u.Roles))
	for _, r := range u.Roles {
		roles = append(roles, string(r))
	}
	return UserView{
		ID:                 u.ID.String(),
		Username:           u.Username,
		FullName:           u.FullName,
		Roles:              roles,
		MustChangePassword: u.MustChangePassword,
		ScopeMode:          string(u.Scope().Mode),
	}
}
