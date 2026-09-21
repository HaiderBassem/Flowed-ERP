package httpapi

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
)

// DataHandlers serve the one-button export and import of the whole system.
type DataHandlers struct {
	Data *app.DataExportService
}

// NewDataHandlers wires the data endpoints.
func NewDataHandlers(data *app.DataExportService) *DataHandlers {
	return &DataHandlers{Data: data}
}

// Register mounts the routes.
func (h *DataHandlers) Register(g *gin.RouterGroup) {
	group := g.Group("/data")
	group.GET("/export", h.Export)
	group.POST("/import", h.Import)
}

// Export streams every table as a ZIP of CSV files.
func (h *DataHandlers) Export(c *gin.Context) {
	archive, filename, err := h.Data.ExportAll(requestContext(c), httpx.MustActor(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	// Content-Disposition names the file the browser saves. Without it the
	// download lands as "export", with no extension, and the operator has a
	// file their computer refuses to open.
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/zip", archive)
}

// ImportResponse reports what an import loaded.
type ImportResponse struct {
	SchemaVersion int             `json:"schema_version"`
	TotalRows     int64           `json:"total_rows"`
	Tables        []ImportedTable `json:"tables"`
}

// ImportedTable is one table and the rows that went into it.
type ImportedTable struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// Import replaces the system's data from an uploaded archive.
//
// The file arrives as a multipart upload rather than a raw body, because that
// is what an HTML form posts: the screen behind this is a file picker and a
// button, and anything else would need a client that is not a browser.
func (h *DataHandlers) Import(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		httpx.Respond(c, shared.Validation("import.no_file",
			"attach the exported archive as the 'file' field").WithCause(err))
		return
	}

	opened, err := file.Open()
	if err != nil {
		httpx.Respond(c, shared.Validation("import.unreadable_upload",
			"the uploaded file could not be read").WithCause(err))
		return
	}
	defer opened.Close()

	archive, err := io.ReadAll(opened)
	if err != nil {
		httpx.Respond(c, shared.Validation("import.unreadable_upload",
			"the uploaded file could not be read").WithCause(err))
		return
	}

	result, err := h.Data.ImportAll(requestContext(c), httpx.MustActor(c), archive)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	response := ImportResponse{
		SchemaVersion: result.SchemaVersion,
		TotalRows:     result.TotalRows,
		Tables:        make([]ImportedTable, 0, len(result.Tables)),
	}
	for _, table := range result.Tables {
		response.Tables = append(response.Tables, ImportedTable{Name: table.Name, Rows: table.Rows})
	}
	httpx.OK(c, response)
}
