package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

// UserHandlers administer operator accounts.
//
// Every one of these was previously a shell command or a psql session. At a
// university, staff turnover means creating an account, disabling a leaver and
// resetting a forgotten password are weekly events — so in practice they were
// all performed by whoever held the database password, which is exactly the
// concentration of authority the financial commands are designed to prevent.
type UserHandlers struct {
	Users *app.UserService
}

// NewUserHandlers wires the operator-administration endpoints.
func NewUserHandlers(users *app.UserService) *UserHandlers { return &UserHandlers{Users: users} }

// Register mounts the routes. Administration is the administrator's, with
// read-only access for the auditor, whose job is to see who holds what.
func (h *UserHandlers) Register(g *gin.RouterGroup) {
	users := g.Group("/users")

	users.GET("",
		h.List)
	users.GET("/:id",
		h.Get)
	users.GET("/:id/sessions",
		h.Sessions)
	users.GET("/:id/login-history",
		h.LoginHistory)

	users.POST("", h.Create)
	users.POST("/:id/roles", h.SetRoles)
	users.POST("/:id/scope", h.SetScope)
	users.POST("/:id/disable", h.Disable)
	users.POST("/:id/enable", h.Enable)
	users.POST("/:id/reset-password", h.ResetPassword)

	// Revoking a session is mounted outside the /users group because an
	// operator may revoke their own without holding any administrative role;
	// the service decides which of the two applies.
	g.POST("/sessions/:id/revoke", h.RevokeSession)
}

// List returns the operators.
func (h *UserHandlers) List(c *gin.Context) {
	users, err := h.Users.ListUsers(requestContext(c), httpx.MustActor(c), queryBool(c, "active_only"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]UserDetailView, 0, len(users))
	for _, u := range users {
		views = append(views, toUserDetailView(u))
	}
	httpx.OK(c, views)
}

// Get returns one operator.
func (h *UserHandlers) Get(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	user, err := h.Users.GetUser(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toUserDetailView(user))
}

// Create registers an operator.
func (h *UserHandlers) Create(c *gin.Context) {
	var req CreateUserRequest
	if !bindJSON(c, &req) {
		return
	}

	colleges, ok := parseIDList(c, req.Colleges, "colleges")
	if !ok {
		return
	}
	departments, ok := parseIDList(c, req.Departments, "departments")
	if !ok {
		return
	}

	result, err := h.Users.CreateUser(requestContext(c), httpx.MustActor(c), app.CreateUserInput{
		Username:    req.Username,
		FullName:    req.FullName,
		Email:       req.Email,
		Roles:       toRoles(req.Roles),
		Password:    req.Password,
		ScopeMode:   shared.ScopeMode(req.ScopeMode),
		Colleges:    colleges,
		Departments: departments,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	view := toUserDetailView(result.User)
	httpx.Created(c, CreateUserResponse{
		User: view,
		// Returned once and never stored in readable form. The administrator
		// hands it over; the holder must replace it before doing anything else.
		TemporaryPassword: result.TemporaryPassword,
	})
}

// SetRoles replaces an operator's roles.
func (h *UserHandlers) SetRoles(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req SetRolesRequest
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.Users.SetRoles(requestContext(c), httpx.MustActor(c), app.SetRolesInput{
		UserID: id,
		Roles:  toRoles(req.Roles),
		Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toUserDetailView(user))
}

// SetScope replaces an operator's organisational grants.
func (h *UserHandlers) SetScope(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req SetScopeRequest
	if !bindJSON(c, &req) {
		return
	}

	colleges, ok := parseIDList(c, req.Colleges, "colleges")
	if !ok {
		return
	}
	departments, ok := parseIDList(c, req.Departments, "departments")
	if !ok {
		return
	}

	user, err := h.Users.SetScope(requestContext(c), httpx.MustActor(c), app.SetScopeInput{
		UserID:      id,
		Mode:        shared.ScopeMode(req.ScopeMode),
		Colleges:    colleges,
		Departments: departments,
		Reason:      req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toUserDetailView(user))
}

// Disable switches an account off and ends its sessions.
func (h *UserHandlers) Disable(c *gin.Context) { h.setActive(c, false) }

// Enable restores a disabled account.
func (h *UserHandlers) Enable(c *gin.Context) { h.setActive(c, true) }

func (h *UserHandlers) setActive(c *gin.Context, active bool) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ReasonRequest
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.Users.SetActive(requestContext(c), httpx.MustActor(c), app.SetActiveInput{
		UserID: id,
		Active: active,
		Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toUserDetailView(user))
}

// ResetPassword issues a new credential for somebody who cannot sign in.
func (h *UserHandlers) ResetPassword(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ResetPasswordRequest
	if !bindJSON(c, &req) {
		return
	}

	result, err := h.Users.ResetPassword(requestContext(c), httpx.MustActor(c), app.ResetPasswordInput{
		UserID:   id,
		Password: req.Password,
		Reason:   req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, CreateUserResponse{
		User:              toUserDetailView(result.User),
		TemporaryPassword: result.TemporaryPassword,
	})
}

// Sessions lists an operator's sign-ins.
func (h *UserHandlers) Sessions(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	sessions, err := h.Users.ListSessions(requestContext(c), httpx.MustActor(c), id, queryBool(c, "include_ended"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toSessionViews(sessions, ""))
}

// LoginHistory returns recent sign-in attempts against an account.
//
// The register an investigation reads: a burst of failures before a
// questionable receipt is the shape of somebody borrowing a colleague's login.
func (h *UserHandlers) LoginHistory(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	limit, _ := pagination(c)

	attempts, err := h.Users.LoginHistory(requestContext(c), httpx.MustActor(c), id, limit)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]LoginAttemptView, 0, len(attempts))
	for _, a := range attempts {
		views = append(views, LoginAttemptView{
			Username:    a.Username,
			Succeeded:   a.Succeeded,
			FailureCode: a.FailureCode,
			IPAddress:   a.IPAddress,
			OccurredAt:  a.OccurredAt,
		})
	}
	httpx.OK(c, views)
}

// RevokeSession ends one session.
func (h *UserHandlers) RevokeSession(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ReasonRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := h.Users.RevokeSession(requestContext(c), httpx.MustActor(c), id, req.Reason); err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, gin.H{"revoked": true})
}

// toRoles converts the wire representation. Unknown names are passed through
// rather than dropped: the service refuses them with a message naming the
// mistake, where silently dropping one would create an account with fewer
// authorities than the administrator believes they granted.
func toRoles(names []string) []shared.Role {
	roles := make([]shared.Role, 0, len(names))
	for _, name := range names {
		roles = append(roles, shared.Role(name))
	}
	return roles
}

func parseIDList(c *gin.Context, raw []string, field string) ([]shared.ID, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	ids := make([]shared.ID, 0, len(raw))
	for _, value := range raw {
		id, err := shared.ParseID(value)
		if err != nil {
			httpx.Respond(c, shared.Validation("invalid_identifier",
				"%q in %s is not a valid identifier", value, field).WithCause(err))
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

func toUserDetailView(u *port.User) UserDetailView {
	roles := make([]string, 0, len(u.Roles))
	for _, r := range u.Roles {
		roles = append(roles, string(r))
	}
	return UserDetailView{
		ID:                 u.ID.String(),
		Username:           u.Username,
		FullName:           u.FullName,
		Email:              u.Email,
		Roles:              roles,
		IsActive:           u.IsActive,
		MustChangePassword: u.MustChangePassword,
		ScopeMode:          string(u.Scope().Mode),
		Colleges:           idStrings(u.Colleges),
		Departments:        idStrings(u.Departments),
		LastLoginAt:        u.LastLoginAt,
		LockedUntil:        u.LockedUntil,
		DisabledReason:     u.DisabledReason,
		CreatedAt:          u.CreatedAt,
	}
}

func toSessionViews(sessions []*port.Session, currentID string) []SessionView {
	views := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, SessionView{
			ID:            s.ID.String(),
			IssuedAt:      s.IssuedAt,
			ExpiresAt:     s.ExpiresAt,
			LastSeenAt:    s.LastSeenAt,
			RevokedAt:     s.RevokedAt,
			RevokedReason: s.RevokedReason,
			IPAddress:     s.IPAddress,
			UserAgent:     s.UserAgent,
			Current:       currentID != "" && s.ID.String() == currentID,
		})
	}
	return views
}

func idStrings(ids []shared.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

func ptr[T any](v T) *T { return &v }

var _ = time.Time{}
