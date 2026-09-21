package httpx

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
)

// ginActorKey is where the authenticated actor lives in the gin context. As
// with the request id, the value is stored in both the gin context and the
// request context so that neither a handler holding *gin.Context nor a service
// holding context.Context has to know which one the engine bridges.
const ginActorKey = "flowed.actor"

type actorContextKey struct{}

// Authenticate verifies the bearer token and puts the actor it names into the
// request context. A missing, malformed, expired or forged token is answered
// with 401 and the chain stops here.
func Authenticate(ts *auth.TokenService) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := bearerToken(c.GetHeader("Authorization"))
		if err != nil {
			// RFC 6750 asks a 401 to name the scheme so a client knows what to
			// present next time.
			c.Header("WWW-Authenticate", "Bearer")
			Respond(c, err)
			return
		}

		actor, err := ts.Parse(token)
		if err != nil {
			c.Header("WWW-Authenticate", "Bearer")
			Respond(c, err)
			return
		}

		// The audit trail records where a command came from. The token cannot
		// carry it: a token outlives the connection it was issued on, and the
		// cashier may well have moved desks since.
		actor.IPAddress = c.ClientIP()

		SetActor(c, *actor)
		c.Next()
	}
}

// SetActor installs an actor on the request. Authenticate calls it; so may a
// test, or a future middleware that authenticates by some other means such as
// a signed internal call.
func SetActor(c *gin.Context, actor shared.Actor) {
	c.Set(ginActorKey, actor)
	if c.Request != nil {
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), actorContextKey{}, actor))
	}
}

// ActorFrom returns the authenticated actor. It accepts either the
// *gin.Context or the request context that command handlers are given.
func ActorFrom(ctx context.Context) (shared.Actor, bool) {
	if ctx == nil {
		return shared.Actor{}, false
	}
	if gc, ok := ctx.(*gin.Context); ok {
		if value, exists := gc.Get(ginActorKey); exists {
			if actor, ok := value.(shared.Actor); ok {
				return actor, true
			}
		}
		if gc.Request == nil {
			return shared.Actor{}, false
		}
		ctx = gc.Request.Context()
	}
	actor, ok := ctx.Value(actorContextKey{}).(shared.Actor)
	return actor, ok
}

// MustActor returns the authenticated actor or panics.
//
// A handler reaching this without an actor means its route was mounted outside
// Authenticate, which is a wiring mistake rather than something the client can
// fix. Failing loudly beats letting a command reach the audit log with no
// author; Recovery turns the panic into a 500 and a stack trace.
func MustActor(c *gin.Context) shared.Actor {
	actor, ok := ActorFrom(c)
	if !ok {
		panic("httpx: no actor in context; this route is missing the Authenticate middleware")
	}
	return actor
}

// RequireRoles refuses the request unless the actor holds one of the roles.
//
// The check is delegated to the domain so that an authority failure caught at
// the edge reads exactly like one caught inside a command handler — same code,
// same message, same details — and a client never has to handle two shapes of
// the same refusal.
func RequireRoles(roles ...shared.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := ActorFrom(c)
		if !ok {
			c.Header("WWW-Authenticate", "Bearer")
			Respond(c, shared.Unauthorized("auth.not_authenticated",
				"this endpoint requires authentication"))
			return
		}
		if err := actor.RequireAnyRole(operationName(c), roles...); err != nil {
			Respond(c, err)
			return
		}
		c.Next()
	}
}

// operationName names the route for the domain's authority error, so the
// message says which endpoint was refused rather than just that something was.
func operationName(c *gin.Context) string {
	path := requestPath(c)
	method := requestMethod(c)
	switch {
	case method == "" && path == "":
		return "this operation"
	case method == "":
		return path
	case path == "":
		return method
	default:
		return method + " " + path
	}
}

func bearerToken(header string) (string, error) {
	if strings.TrimSpace(header) == "" {
		return "", shared.Unauthorized("auth.missing_token",
			"an Authorization header carrying a bearer token is required")
	}
	scheme, token, found := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", shared.Unauthorized("auth.malformed_authorization_header",
			`the Authorization header must read "Bearer <token>"`)
	}
	return token, nil
}
