package httpapi

import (
	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/platform/httpx"
)

// SettingsHandlers serve the institution's details — the name, college,
// address and logo that appear on every receipt and report.
type SettingsHandlers struct {
	Settings *app.SettingsService
}

// NewSettingsHandlers wires the settings endpoints.
func NewSettingsHandlers(settings *app.SettingsService) *SettingsHandlers {
	return &SettingsHandlers{Settings: settings}
}

// Register mounts the routes.
func (h *SettingsHandlers) Register(g *gin.RouterGroup) {
	g.GET("/settings", h.Get)
	g.PUT("/settings", h.Update)
}

// SettingsView is the whole settings map, plus the keys a client may write.
//
// The editable list is returned rather than hard-coded in the interface: a
// screen listing the fields it knows about will quietly stop offering the one
// added next, and nobody notices until somebody asks why they cannot set it.
type SettingsView struct {
	Values   map[string]string `json:"values"`
	Editable []string          `json:"editable"`
}

// Get returns the institution's details.
func (h *SettingsHandlers) Get(c *gin.Context) {
	values, err := h.Settings.All(requestContext(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, SettingsView{Values: values, Editable: app.EditableSettings})
}

// UpdateSettingsRequest carries the values to write.
//
// A bare map rather than a struct with a field per setting. The service holds
// the allow-list, so a struct here would be a second list to keep in step with
// it — and the one that drifts is always the one further from the check.
type UpdateSettingsRequest struct {
	Values map[string]string `json:"values" binding:"required"`
}

// Update writes the institution's details.
func (h *SettingsHandlers) Update(c *gin.Context) {
	var req UpdateSettingsRequest
	if !bindJSON(c, &req) {
		return
	}

	values, err := h.Settings.Update(requestContext(c), httpx.MustActor(c), req.Values)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, SettingsView{Values: values, Editable: app.EditableSettings})
}
