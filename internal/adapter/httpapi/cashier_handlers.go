package httpapi

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/platform/pg"
)

// CashierHandlers serves the cashier's own screen: the drawer they open at the
// start of a shift, the sheet they close it against, and the desks they may
// sign in at.
//
// The database handle is here for two reads that have no repository behind
// them — the desk list and the shift sheet's itemisation. Both are plain
// projections with no invariant of their own to protect, and inventing a
// repository interface for them would add a layer that only forwards.
type CashierHandlers struct {
	Sessions *app.CashierService
	// Master answers the desk list. The handler used to query cashier_desk
	// directly, which was the one place the HTTP layer reached past the
	// services into the database.
	Master *app.MasterDataService
	DB     *pg.DB
}

// NewCashierHandlers wires the cashier session routes.
func NewCashierHandlers(sessions *app.CashierService, master *app.MasterDataService, db *pg.DB) *CashierHandlers {
	return &CashierHandlers{Sessions: sessions, Master: master, DB: db}
}

// Register mounts the cashier session and desk routes.
func (h *CashierHandlers) Register(g *gin.RouterGroup) {
	sessions := g.Group("/cashier-sessions")

	sessions.POST("",
		httpx.RequireRoles(shared.RoleCashier),
		h.OpenSession)
	sessions.GET("/current",
		httpx.RequireRoles(shared.RoleCashier),
		h.CurrentSession)
	sessions.POST("/:id/close",
		httpx.RequireRoles(shared.RoleCashier),
		h.CloseSession)

	// Signing off a drawer is a supervisor's act, and the domain refuses it
	// from the cashier whose drawer it is even if the roles ever let them
	// through.
	sessions.POST("/:id/approve",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin),
		h.ApproveSession)

	// A cashier may read their own shift; the service refuses anyone else's.
	sessions.GET("/:id",
		httpx.RequireRoles(shared.RoleCashier, shared.RoleFinanceManager,
			shared.RoleAdmin, shared.RoleAuditor),
		h.SessionSummary)

	// Readable by anyone signed in.
	g.GET("/cashier-desks", h.ListDesks)
}

// RegisterPublic mounts the desk list outside authentication.
//
// A cashier cannot sign in without naming their desk — receipt series run per
// desk, and auth.cashier_desk_required refuses the attempt without one. So the
// sign-in form has to offer the list *before* anybody is signed in, and
// mounting the list behind authentication makes that impossible: the operator
// needs a token to see the desks and a desk to get a token.
//
// Nothing is disclosed by it. A desk is a window's code and its Arabic name,
// both of which are printed on every receipt the university hands out, and the
// service behind it never consulted the actor in the first place. Only active
// desks are listed here: include_inactive is an administrative view and stays
// on the authenticated route.
func (h *CashierHandlers) RegisterPublic(engine *gin.Engine) {
	engine.GET("/api/v1/public/cashier-desks", h.PublicDesks)
}

// PublicDesks lists the active desks a cashier may sign in at.
func (h *CashierHandlers) PublicDesks(c *gin.Context) {
	desks, err := h.Master.ListCashierDesks(requestContext(c), shared.Actor{}, true)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]CashierDeskView, 0, len(desks))
	for _, desk := range desks {
		views = append(views, toDeskView(desk))
	}
	httpx.OK(c, views)
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// OpenCashierSessionRequest starts a shift.
type OpenCashierSessionRequest struct {
	// CashierDeskID must match the desk on the caller's token. It is sent
	// explicitly so the terminal states which drawer it believes it is opening
	// and a mismatch is refused rather than silently resolved.
	CashierDeskID string `json:"cashier_desk_id" binding:"required,uuid"`
	OpeningFloat  int64  `json:"opening_float" binding:"min=0"`
}

// CloseCashierSessionRequest ends a shift with the counted cash.
type CloseCashierSessionRequest struct {
	CountedCash int64 `json:"counted_cash" binding:"min=0"`
	// VarianceReason is required by the domain whenever the count and the
	// expectation disagree, and refused as a field is not the place to enforce
	// it: the variance is not known until the drawer is totalled.
	VarianceReason *string `json:"variance_reason"`
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// CashierSessionView is a shift.
type CashierSessionView struct {
	ID             string        `json:"id"`
	CashierUserID  string        `json:"cashier_user_id"`
	CashierDeskID  string        `json:"cashier_desk_id"`
	AcademicYearID string        `json:"academic_year_id"`
	Status         string        `json:"status"`
	OpenedAt       time.Time     `json:"opened_at"`
	OpeningFloat   money.Amount  `json:"opening_float"`
	ClosedAt       *time.Time    `json:"closed_at,omitempty"`
	ExpectedCash   *money.Amount `json:"expected_cash,omitempty"`
	CountedCash    *money.Amount `json:"counted_cash,omitempty"`
	Variance       *money.Amount `json:"variance,omitempty"`
	VarianceReason *string       `json:"variance_reason,omitempty"`
	ApprovedBy     *string       `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time    `json:"approved_at,omitempty"`
}

// CashierSessionSummaryView is the shift sheet.
type CashierSessionSummaryView struct {
	Session      CashierSessionView `json:"session"`
	OpeningFloat money.Amount       `json:"opening_float"`
	// ExpectedCash is computed from the transaction rows, never typed by the
	// person being reconciled.
	ExpectedCash   money.Amount        `json:"expected_cash"`
	CountedCash    *money.Amount       `json:"counted_cash,omitempty"`
	Variance       *money.Amount       `json:"variance,omitempty"`
	VarianceReason *string             `json:"variance_reason,omitempty"`
	Methods        []SessionMethodView `json:"payments_by_method"`
	Refunds        SessionTotalView    `json:"refunds"`
	Voids          SessionTotalView    `json:"voids"`
}

// SessionMethodView is one row of the shift's takings, split by how the money
// arrived. Cash and non-cash sit side by side: only the cash rows should
// reconcile against the drawer, and a cashier looking at a variance needs to
// see which is which.
type SessionMethodView struct {
	MethodCode string       `json:"method_code"`
	IsCash     bool         `json:"is_cash"`
	Count      int          `json:"count"`
	Total      money.Amount `json:"total"`
	VoidCount  int          `json:"void_count"`
	VoidTotal  money.Amount `json:"void_total"`
}

// SessionTotalView is a count and a sum.
type SessionTotalView struct {
	Count int          `json:"count"`
	Total money.Amount `json:"total"`
}

// CashierDeskView is a physical window a cashier may sign in at.
type CashierDeskView struct {
	ID        string  `json:"id"`
	Code      string  `json:"code"`
	NameAr    string  `json:"name_ar"`
	CollegeID *string `json:"college_id,omitempty"`
	IsActive  bool    `json:"is_active"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// OpenSession starts a shift at the cashier's desk.
func (h *CashierHandlers) OpenSession(c *gin.Context) {
	var req OpenCashierSessionRequest
	if !bindJSON(c, &req) {
		return
	}
	deskID, err := shared.ParseID(req.CashierDeskID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	session, err := h.Sessions.OpenSession(
		requestContext(c), httpx.MustActor(c), deskID, money.FromInt64(req.OpeningFloat))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toCashierSessionView(session))
}

// CurrentSession returns the cashier's open drawer.
//
// Having none is a normal state at the start of the day, so it answers 200 with
// an absent session rather than 404: a login screen should render "no open
// drawer" rather than an error the cashier cannot act on.
func (h *CashierHandlers) CurrentSession(c *gin.Context) {
	session, err := h.Sessions.GetOpenSession(requestContext(c), httpx.MustActor(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	if session == nil {
		httpx.OK(c, gin.H{"open": false, "session": nil})
		return
	}
	httpx.OK(c, gin.H{"open": true, "session": toCashierSessionView(session)})
}

// CloseSession ends a shift against the counted cash.
func (h *CashierHandlers) CloseSession(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req CloseCashierSessionRequest
	if !bindJSON(c, &req) {
		return
	}

	session, err := h.Sessions.CloseSession(
		requestContext(c), httpx.MustActor(c), id,
		money.FromInt64(req.CountedCash), req.VarianceReason)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toCashierSessionView(session))
}

// ApproveSession is the supervisor's sign-off on a closed drawer.
func (h *CashierHandlers) ApproveSession(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	session, err := h.Sessions.ApproveSession(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toCashierSessionView(session))
}

// SessionSummary returns the shift sheet.
//
// The command runs first and the itemisation second, in that order: the service
// owns the authority check — a cashier reads their own shift and nobody else's —
// so the breakdown query never runs for a caller who was going to be refused.
func (h *CashierHandlers) SessionSummary(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	ctx := requestContext(c)

	summary, err := h.Sessions.SessionSummary(ctx, httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	methods, refunds, err := h.sessionActivity(ctx, id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	voids := SessionTotalView{}
	for _, m := range methods {
		voids.Count += m.VoidCount
		voids.Total = voids.Total.MustAdd(m.VoidTotal)
	}

	httpx.OK(c, CashierSessionSummaryView{
		Session:        toCashierSessionView(summary.Session),
		OpeningFloat:   summary.OpeningFloat,
		ExpectedCash:   summary.ExpectedCash,
		CountedCash:    summary.CountedCash,
		Variance:       summary.Variance,
		VarianceReason: summary.VarianceReason,
		Methods:        methods,
		Refunds:        refunds,
		Voids:          voids,
	})
}

// ListDesks returns the windows a cashier may sign in at.
//
// It reads through the reference repository rather than issuing its own SQL.
// The query it replaced was the only place in the HTTP layer that talked to the
// database directly, which meant the desk list quietly bypassed the service
// layer — and when desks became administrable, two code paths would have had to
// agree about what "active" meant.
func (h *CashierHandlers) ListDesks(c *gin.Context) {
	desks, err := h.Master.ListCashierDesks(
		requestContext(c), httpx.MustActor(c), !queryBool(c, "include_inactive"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]CashierDeskView, 0, len(desks))
	for _, desk := range desks {
		views = append(views, toDeskView(desk))
	}
	httpx.OK(c, views)
}

// sessionActivity itemises a shift: what was taken by method, what was voided,
// and what was refunded.
//
// A voided payment appears in both its method's takings and its void column,
// and the two cancel. That is the same netting the expected-cash figure
// performs, and showing both halves rather than a net is deliberate: a shift
// with six voids and a balanced drawer looks identical to a quiet one if only
// the net is printed, and six voids in one shift is exactly what the void
// register exists to surface.
func (h *CashierHandlers) sessionActivity(
	ctx context.Context, sessionID shared.ID,
) ([]SessionMethodView, SessionTotalView, error) {
	const byMethod = `
		SELECT pm.code,
		       pm.is_cash,
		       count(*) FILTER (WHERE p.status = 'posted')::bigint,
		       coalesce(sum(p.amount) FILTER (WHERE p.status = 'posted'), 0)::bigint,
		       count(*) FILTER (WHERE p.status = 'voided')::bigint,
		       coalesce(sum(p.amount) FILTER (WHERE p.status = 'voided'), 0)::bigint
		FROM payment p
		JOIN payment_method pm ON pm.id = p.payment_method_id
		WHERE p.cashier_session_id = $1
		GROUP BY pm.code, pm.is_cash
		ORDER BY pm.code`

	conn := h.DB.Conn(ctx)

	rows, err := conn.Query(ctx, byMethod, sessionID)
	if err != nil {
		return nil, SessionTotalView{}, pg.WrapQuery("cashier_session.ByMethod", err)
	}
	defer rows.Close()

	methods := make([]SessionMethodView, 0, 4)
	for rows.Next() {
		var (
			methodCode       string
			isCash           bool
			count, voidCount int64
			total, voidTotal money.Amount
		)
		if err := rows.Scan(&methodCode, &isCash, &count, &total, &voidCount, &voidTotal); err != nil {
			return nil, SessionTotalView{}, pg.WrapQuery("cashier_session.ByMethod.scan", err)
		}
		methods = append(methods, SessionMethodView{
			MethodCode: methodCode,
			IsCash:     isCash,
			Count:      int(count),
			Total:      total,
			VoidCount:  int(voidCount),
			VoidTotal:  voidTotal,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, SessionTotalView{}, pg.WrapQuery("cashier_session.ByMethod.rows", err)
	}

	const refundTotals = `
		SELECT count(*)::bigint, coalesce(sum(amount), 0)::bigint
		FROM refund
		WHERE cashier_session_id = $1 AND status = 'posted'`

	var (
		refundCount int64
		refundTotal money.Amount
	)
	if err := conn.QueryRow(ctx, refundTotals, sessionID).Scan(&refundCount, &refundTotal); err != nil {
		return nil, SessionTotalView{}, pg.WrapQuery("cashier_session.RefundTotals", err)
	}

	return methods, SessionTotalView{Count: int(refundCount), Total: refundTotal}, nil
}

func toCashierSessionView(s *payment.CashierSession) CashierSessionView {
	view := CashierSessionView{
		ID:             s.ID.String(),
		CashierUserID:  s.CashierUserID.String(),
		CashierDeskID:  s.CashierDeskID.String(),
		AcademicYearID: s.AcademicYearID.String(),
		Status:         string(s.Status),
		OpenedAt:       s.OpenedAt,
		OpeningFloat:   s.OpeningFloat,
		ClosedAt:       s.ClosedAt,
		ExpectedCash:   s.ExpectedCash,
		CountedCash:    s.CountedCash,
		Variance:       s.Variance,
		VarianceReason: s.VarianceReason,
		ApprovedAt:     s.ApprovedAt,
	}
	if s.ApprovedBy != nil {
		view.ApprovedBy = ptrString(s.ApprovedBy.String())
	}
	return view
}
