package httpx

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/domain/shared"
)

// SessionChecker reports whether a session may still be used. Implemented by
// the application's auth service; an interface here so the transport package
// does not depend on the application package.
type SessionChecker interface {
	SessionActive(ctx context.Context, sessionID shared.ID) (bool, error)
}

// RequireLiveSession refuses a request whose session has been revoked.
//
// Without it, revocation only bites when the access token expires: a dismissed
// cashier's browser keeps working for the rest of the token's lifetime, and
// "sign out my other sessions" is a promise the system does not keep. With it,
// logout, disablement, a password reset and a role change all take effect on
// the very next request.
//
// The cost is one primary-key lookup per authenticated request. That is a real
// cost at a busy desk, so it is switchable: a deployment that would rather have
// the round trip back can turn it off and accept revocation lagging by one
// access-token lifetime, which is the behaviour that existed before. The
// trade-off is named in configuration rather than hidden in code.
//
// Failure is deliberately open. If the database cannot be reached the request
// proceeds on the token's own validity: the alternative is that a database blip
// signs out every cashier in the university, which is a far worse outcome than
// a revocation taking effect a few minutes late.
func RequireLiveSession(checker SessionChecker, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := ActorFrom(c)
		if !ok || actor.SessionID == "" {
			c.Next()
			return
		}

		sessionID, err := shared.ParseID(actor.SessionID)
		if err != nil {
			// A token from before session tracking. It still carries a valid
			// signature and expiry, which is what it was issued under.
			c.Next()
			return
		}

		active, err := checker.SessionActive(c.Request.Context(), sessionID)
		if err != nil {
			if log != nil {
				log.WarnContext(c.Request.Context(),
					"session check failed; admitting on the token's own validity",
					slog.String("error", err.Error()))
			}
			c.Next()
			return
		}
		if !active {
			c.Header("WWW-Authenticate", "Bearer")
			Respond(c, shared.Unauthorized("auth.session_revoked",
				"this session was ended; sign in again").
				WithDetail("remedy", "sign in again to start a new session"))
			return
		}
		c.Next()
	}
}

// PasswordChangeGate refuses every route but the password change while the
// actor's credential is one somebody else chose.
//
// An administrator who resets a password knows it. Until the holder replaces
// it, the administrator can act as them — and at a cashier desk the receipt
// would carry the cashier's name. The gate closes that window at the holder's
// next request rather than leaving it open for as long as they never get round
// to changing it.
//
// The exempt routes are named rather than pattern-matched: a prefix rule would
// silently exempt anything mounted under the same path later.
func PasswordChangeGate(mustChange func(ctx context.Context, userID shared.ID) (bool, error), exempt []string) gin.HandlerFunc {
	exemptSet := make(map[string]struct{}, len(exempt))
	for _, route := range exempt {
		exemptSet[route] = struct{}{}
	}

	return func(c *gin.Context) {
		if _, ok := exemptSet[c.FullPath()]; ok {
			c.Next()
			return
		}
		actor, ok := ActorFrom(c)
		if !ok {
			c.Next()
			return
		}

		required, err := mustChange(c.Request.Context(), actor.UserID)
		if err != nil {
			// Same reasoning as the session check: a database blip must not
			// lock every operator out of the system.
			c.Next()
			return
		}
		if required {
			Respond(c, shared.Forbidden("auth.password_change_required",
				"this account is using a password somebody else set; change it before continuing").
				WithDetail("remedy", "POST /api/v1/auth/change-password"))
			return
		}
		c.Next()
	}
}

// SecurityHeaders sets the response headers that cost nothing and close the
// browser-side gaps.
//
// Each one is here for a reason rather than as a checklist item:
//
//   - X-Content-Type-Options stops a browser deciding that a JSON error body
//     is really HTML and running it. The API returns user-supplied strings in
//     error details, so this is not theoretical.
//   - X-Frame-Options and frame-ancestors stop the operator UI being framed by
//     another site, which is how a click on somebody else's page becomes a
//     void executed on this one.
//   - Referrer-Policy keeps student identifiers out of the Referer header when
//     an operator follows a link off a page whose URL contains one.
//   - The default-src 'none' policy is for the API's own responses, which are
//     JSON and load nothing. The UI ships its own, wider, policy.
//   - HSTS is set only when the request arrived over TLS, so a development
//     server on plain HTTP does not pin a browser to a scheme it cannot serve.
func SecurityHeaders(enableHSTS bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		// Nothing here needs a camera, a microphone or a location.
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")

		if enableHSTS && (c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	}
}
