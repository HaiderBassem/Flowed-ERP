package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/adapter/payments"
	"flowed/internal/app"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
)

// IntentHandlers expose electronic collection.
type IntentHandlers struct {
	Intents *app.IntentService
	Log     *slog.Logger
	// PublicBaseURL is where this deployment is reachable from the internet.
	// Providers post their confirmations to a URL built from it, so it is
	// configuration rather than something derived from the request: a request
	// arriving through a misconfigured proxy would otherwise send the provider
	// to an address only that proxy can reach.
	PublicBaseURL string
}

// NewIntentHandlers wires the electronic collection endpoints.
func NewIntentHandlers(intents *app.IntentService, publicBaseURL string, log *slog.Logger) *IntentHandlers {
	return &IntentHandlers{Intents: intents, PublicBaseURL: publicBaseURL, Log: log}
}

// Register mounts the authenticated routes.
//
// The webhook is mounted separately, outside authentication: a provider has no
// credentials of ours and never should. Its authority is the signature on the
// body, which the provider adapter verifies before anything reads the contents.
func (h *IntentHandlers) Register(g *gin.RouterGroup) {
	payments := g.Group("/payment-intents")

	payments.GET("/providers", h.Providers)
	payments.POST("",
		httpx.RequireRoles(shared.RoleCashier, shared.RoleFinanceManager,
			shared.RoleAdmin, shared.RoleStudent),
		h.Initiate)
	payments.GET("/:id", h.Get)
	payments.POST("/:id/poll", h.Poll)
	payments.GET("/accounts/:account_id", h.ListForAccount)
}

// RegisterWebhooks mounts the provider callback endpoint outside
// authentication.
func (h *IntentHandlers) RegisterWebhooks(engine gin.IRoutes) {
	engine.POST("/webhooks/payments/:provider", h.Callback)
}

// Providers lists the electronic channels this deployment offers.
func (h *IntentHandlers) Providers(c *gin.Context) {
	infos := h.Intents.Providers()

	views := make([]PaymentProviderView, 0, len(infos))
	for _, info := range infos {
		views = append(views, PaymentProviderView{Code: info.Code, DisplayName: info.DisplayName})
	}
	httpx.OK(c, views)
}

// Initiate begins a collection at a provider.
func (h *IntentHandlers) Initiate(c *gin.Context) {
	var req InitiatePaymentRequest
	if !bindJSON(c, &req) {
		return
	}
	accountID, err := shared.ParseID(req.AccountID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	var amount money.Amount
	if req.Amount != nil {
		amount = money.Amount(*req.Amount)
	}

	result, err := h.Intents.Initiate(requestContext(c), httpx.MustActor(c), app.InitiateIntentInput{
		AccountID:    accountID,
		ProviderCode: req.Provider,
		Amount:       amount,
		ReturnURL:    req.ReturnURL,
		CallbackURL:  h.PublicBaseURL + "/webhooks/payments/" + req.Provider,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	view := toIntentView(result.Intent)
	view.Instruction = result.Instruction
	if result.RedirectURL != "" {
		view.RedirectURL = ptr(result.RedirectURL)
	}
	httpx.Created(c, view)
}

// Get returns one collection request.
func (h *IntentHandlers) Get(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	// Reading goes through the poll path's ownership rules by way of the
	// service, which is where a student's claim to an account is checked.
	result, err := h.Intents.PollStatus(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toIntentView(result.Intent))
}

// Poll asks the provider directly about a collection whose callback never
// arrived — the ordinary end of a student closing the browser at the wrong
// moment.
func (h *IntentHandlers) Poll(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}

	result, err := h.Intents.PollStatus(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	view := toIntentView(result.Intent)
	httpx.OK(c, gin.H{
		"intent": view,
		// A poll that found nothing new says so rather than pretending to have
		// done something.
		"already_known": result.Duplicate,
	})
}

// ListForAccount returns an account's collection requests.
func (h *IntentHandlers) ListForAccount(c *gin.Context) {
	accountID, ok := pathID(c, "account_id")
	if !ok {
		return
	}

	intents, err := h.Intents.ListForAccount(requestContext(c), httpx.MustActor(c), accountID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]PaymentIntentView, 0, len(intents))
	for _, intent := range intents {
		views = append(views, toIntentView(intent))
	}
	httpx.OK(c, views)
}

// maxCallbackBody bounds what a provider may post. Generous for a JSON
// confirmation and far short of anything that could exhaust this process.
const maxCallbackBody = 256 << 10

// Callback receives a provider's confirmation.
//
// Unauthenticated by necessity and safe by construction: the body's signature
// is the authority, and it is verified before any field is read as meaningful.
// Three properties matter here and each is deliberate.
//
// The response says as little as possible. A provider needs to know whether to
// retry; anybody probing the endpoint learns nothing about which references
// exist.
//
// A duplicate delivery answers 200. Providers retry until they get one, and a
// second payment is prevented by the event's unique identifier rather than by
// hoping the retry never comes.
//
// A refused callback answers 4xx and is recorded. A burst of them is somebody
// probing, and that is only visible if the failures are kept.
func (h *IntentHandlers) Callback(c *gin.Context) {
	providerCode := c.Param("provider")

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxCallbackBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"accepted": false})
		return
	}

	headers := make(map[string]string, len(c.Request.Header))
	for name := range c.Request.Header {
		headers[name] = c.Request.Header.Get(name)
	}
	query := make(map[string]string)
	for name, values := range c.Request.URL.Query() {
		if len(values) > 0 {
			query[name] = values[0]
		}
	}

	result, err := h.Intents.HandleCallback(requestContext(c), app.HandleCallbackInput{
		ProviderCode: providerCode,
		Envelope: payments.CallbackEnvelope{
			Body:       body,
			Headers:    headers,
			Query:      query,
			ReceivedAt: time.Now().UTC(),
		},
	})
	if err != nil {
		// Logged in full for an operator; answered in outline for the caller.
		if h.Log != nil {
			h.Log.WarnContext(c.Request.Context(), "provider callback refused",
				slog.String("provider", providerCode),
				slog.String("code", shared.CodeOf(err)),
				slog.String("client_ip", c.ClientIP()))
		}
		status := httpx.StatusForKind(shared.KindOf(err))
		if status >= 500 {
			// A failure on our side must make the provider retry rather than
			// treat the confirmation as delivered.
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"accepted": false})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"accepted":  true,
		"duplicate": result.Duplicate,
	})
}

func toIntentView(intent *payment.Intent) PaymentIntentView {
	if intent == nil {
		return PaymentIntentView{}
	}
	view := PaymentIntentView{
		ID:          intent.ID.String(),
		Provider:    intent.ProviderCode,
		AccountID:   intent.AccountID.String(),
		Amount:      intent.Amount,
		Status:      string(intent.Status),
		ProviderRef: intent.ProviderRef,
		RedirectURL: intent.RedirectURL,
		ExpiresAt:   intent.ExpiresAt,
		CreatedAt:   intent.CreatedAt,
	}
	if intent.PaymentID != nil {
		view.PaymentID = ptr(intent.PaymentID.String())
	}
	return view
}
