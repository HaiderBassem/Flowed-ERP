package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

// ReconciliationHandlers serve the invariant queue: what the checks found, who
// is on it, and what was concluded.
//
// The nightly run answers "is anything wrong". These answer the questions that
// follow it — since when, has anyone looked, and what happened last time this
// account did this — which are the questions somebody actually has at nine in
// the morning.
type ReconciliationHandlers struct {
	Service *app.ReconciliationService
}

// NewReconciliationHandlers wires the oversight routes.
func NewReconciliationHandlers(service *app.ReconciliationService) *ReconciliationHandlers {
	return &ReconciliationHandlers{Service: service}
}

// Register mounts the reconciliation routes.
func (h *ReconciliationHandlers) Register(g *gin.RouterGroup) {
	group := g.Group("/reconciliation",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor))

	group.GET("/runs", h.ListRuns)
	group.GET("/findings", h.ListFindings)

	// Running on demand is oversight, not administration: the person who wants
	// it is the one about to close the books or answer a question about an
	// account, and making them wait for the nightly pass is how a check gets
	// worked around instead of used.
	group.POST("/run", h.RunNow)

	// Taking and closing a finding are money-oversight acts and an auditor
	// watches rather than acts, so the service narrows these two further.
	group.POST("/findings/:id/acknowledge", h.Acknowledge)
	group.POST("/findings/:id/resolve", h.Resolve)
}

// ListRuns returns recent passes, newest first.
func (h *ReconciliationHandlers) ListRuns(c *gin.Context) {
	runs, err := h.Service.ListRuns(requestContext(c), httpx.MustActor(c),
		c.Query("kind"), limitOr(c, 50))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]ReconciliationRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toRunView(run))
	}
	httpx.OK(c, views)
}

// ListFindings returns the queue. Without a state filter it is what is open.
func (h *ReconciliationHandlers) ListFindings(c *gin.Context) {
	findings, err := h.Service.ListFindings(requestContext(c), httpx.MustActor(c),
		c.Query("state"), limitOr(c, 100))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]ReconciliationFindingView, 0, len(findings))
	for _, finding := range findings {
		views = append(views, toFindingView(finding))
	}
	httpx.OK(c, views)
}

// RunNow performs the checks immediately.
func (h *ReconciliationHandlers) RunNow(c *gin.Context) {
	var req RunReconciliationRequest
	if c.Request.ContentLength > 0 && !bindJSON(c, &req) {
		return
	}
	kinds := make([]port.ReconciliationKind, 0, len(req.Kinds))
	for _, kind := range req.Kinds {
		kinds = append(kinds, port.ReconciliationKind(kind))
	}

	summary, err := h.Service.Run(requestContext(c), httpx.MustActor(c), kinds)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	// Mapped to the view rather than answered with the port struct: the port
	// types carry no JSON tags on purpose, and returning one would put Go field
	// names into a client contract that then cannot change.
	runs := make([]ReconciliationRunView, 0, len(summary.Runs))
	for _, run := range summary.Runs {
		runs = append(runs, toRunView(run))
	}
	httpx.OK(c, RunReconciliationResponse{
		Clean:        summary.Clean(),
		Runs:         runs,
		Findings:     summary.Findings,
		NewFindings:  summary.NewFindings,
		OpenCritical: summary.Critical,
		OpenWarnings: summary.Warnings,
	})
}

// Acknowledge marks a finding as being worked on.
func (h *ReconciliationHandlers) Acknowledge(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ReasonRequest
	if !bindJSON(c, &req) {
		return
	}

	finding, err := h.Service.Acknowledge(requestContext(c), httpx.MustActor(c),
		app.AcknowledgeInput{FindingID: id, Reason: req.Reason})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toFindingView(finding))
}

// Resolve closes a finding with what was done about it.
func (h *ReconciliationHandlers) Resolve(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ResolveFindingRequest
	if !bindJSON(c, &req) {
		return
	}

	finding, err := h.Service.Resolve(requestContext(c), httpx.MustActor(c),
		app.ResolveInput{FindingID: id, Resolution: req.Resolution})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toFindingView(finding))
}

// limitOr reads the page size, falling back when it is absent. The oversight
// lists are small by nature — a queue with a thousand entries is a system in
// trouble, not a paging problem — so they take a bare limit rather than the
// page envelope the student-facing lists use.
func limitOr(c *gin.Context, fallback int) int {
	limit, _ := pagination(c)
	if limit <= 0 {
		return fallback
	}
	return limit
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// RunReconciliationRequest names which checks to run. Empty means all of them.
type RunReconciliationRequest struct {
	Kinds []string `json:"kinds"`
}

// RunReconciliationResponse is what a pass found.
type RunReconciliationResponse struct {
	// Clean is the whole answer for a dashboard: nothing is open.
	Clean        bool                    `json:"clean"`
	Runs         []ReconciliationRunView `json:"runs"`
	Findings     int                     `json:"findings"`
	NewFindings  int                     `json:"new_findings"`
	OpenCritical int                     `json:"open_critical"`
	OpenWarnings int                     `json:"open_warnings"`
}

// ResolveFindingRequest closes a finding.
type ResolveFindingRequest struct {
	Resolution string `json:"resolution" binding:"required"`
}

// ReconciliationRunView is one pass.
type ReconciliationRunView struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	RowsChecked int64      `json:"rows_checked"`
	Findings    int        `json:"findings"`
	NewFindings int        `json:"new_findings"`
	Error       *string    `json:"error,omitempty"`
}

// ReconciliationFindingView is one violation and its life so far.
type ReconciliationFindingView struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	Detail      map[string]any `json:"detail"`
	Severity    string         `json:"severity"`
	State       string         `json:"state"`
	// SeenCount is how many passes have found it. A number climbing on a
	// finding nobody has taken is the thing to look at first.
	SeenCount   int       `json:"seen_count"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`

	AcknowledgedAt     *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedReason *string    `json:"acknowledged_reason,omitempty"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	Resolution         *string    `json:"resolution,omitempty"`
}

func toRunView(run *port.ReconciliationRun) ReconciliationRunView {
	return ReconciliationRunView{
		ID:          run.ID.String(),
		Kind:        string(run.Kind),
		Status:      run.Status,
		StartedAt:   run.StartedAt,
		FinishedAt:  run.FinishedAt,
		RowsChecked: run.RowsChecked,
		Findings:    run.Findings,
		NewFindings: run.NewFindings,
		Error:       run.Error,
	}
}

func toFindingView(finding *port.ReconciliationFinding) ReconciliationFindingView {
	return ReconciliationFindingView{
		ID:                 finding.ID.String(),
		Kind:               string(finding.Kind),
		SubjectType:        finding.SubjectType,
		SubjectID:          finding.SubjectID.String(),
		Detail:             finding.Detail,
		Severity:           finding.Severity,
		State:              finding.State,
		SeenCount:          finding.SeenCount,
		FirstSeenAt:        finding.FirstSeenAt,
		LastSeenAt:         finding.LastSeenAt,
		AcknowledgedAt:     finding.AcknowledgedAt,
		AcknowledgedReason: finding.AcknowledgedReason,
		ResolvedAt:         finding.ResolvedAt,
		Resolution:         finding.Resolution,
	}
}
