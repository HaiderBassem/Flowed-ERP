package httpapi

import (
	"time"

	"github.com/swibit/flowed/internal/port"
)

// The import port's structs carry no JSON tags, so returning them directly
// serialises Go field names and the import endpoints answer in PascalCase
// while every other endpoint answers in snake_case. A client would have to
// handle two conventions from one API.
//
// The tags belong here rather than on the port because the port package holds
// the domain's view of storage and says so: no SQL, no driver type, no HTTP
// concept. A JSON tag is an HTTP concept. Keeping it out also means the wire
// format can change without touching the repository contract.

// ImportBatchView is a staged import as the API presents it.
type ImportBatchView struct {
	ID             string  `json:"id"`
	BatchType      string  `json:"batch_type"`
	SourceFilename *string `json:"source_filename,omitempty"`
	Status         string  `json:"status"`
	AcademicYearID *string `json:"academic_year_id,omitempty"`

	TotalRows   int `json:"total_rows"`
	ValidRows   int `json:"valid_rows"`
	ErrorRows   int `json:"error_rows"`
	CreatedRows int `json:"created_rows"`
	UpdatedRows int `json:"updated_rows"`
	SkippedRows int `json:"skipped_rows"`
	FailedRows  int `json:"failed_rows"`

	// HeartbeatAt is how a stalled worker is spotted: a batch still marked
	// importing whose heartbeat stopped is one the reaper will fail.
	HeartbeatAt  *time.Time `json:"heartbeat_at,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	ErrorSummary *string    `json:"error_summary,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

// ImportRowView is one row of a staged import, with whatever is wrong with it.
type ImportRowView struct {
	RowNo            int                  `json:"row_no"`
	RawData          map[string]any       `json:"raw_data,omitempty"`
	ValidationStatus string               `json:"validation_status"`
	Disposition      string               `json:"disposition"`
	Errors           []port.ImportFinding `json:"errors,omitempty"`
	Warnings         []port.ImportFinding `json:"warnings,omitempty"`
	MatchedEntityID  *string              `json:"matched_entity_id,omitempty"`
	CreatedEntityID  *string              `json:"created_entity_id,omitempty"`
	ProcessedAt      *time.Time           `json:"processed_at,omitempty"`
	ErrorMessage     *string              `json:"error_message,omitempty"`
}

func toImportBatchView(b *port.ImportBatch) ImportBatchView {
	if b == nil {
		return ImportBatchView{}
	}
	v := ImportBatchView{
		ID:             b.ID.String(),
		BatchType:      b.BatchType,
		SourceFilename: b.SourceFilename,
		Status:         string(b.Status),
		TotalRows:      b.TotalRows,
		ValidRows:      b.ValidRows,
		ErrorRows:      b.ErrorRows,
		CreatedRows:    b.CreatedRows,
		UpdatedRows:    b.UpdatedRows,
		SkippedRows:    b.SkippedRows,
		FailedRows:     b.FailedRows,
		HeartbeatAt:    b.HeartbeatAt,
		StartedAt:      b.StartedAt,
		CompletedAt:    b.CompletedAt,
		ErrorSummary:   b.ErrorSummary,
		CreatedAt:      b.CreatedAt,
		ConfirmedAt:    b.ConfirmedAt,
	}
	if b.AcademicYearID != nil {
		v.AcademicYearID = ptrString(b.AcademicYearID.String())
	}
	return v
}

func toImportBatchViews(batches []*port.ImportBatch) []ImportBatchView {
	out := make([]ImportBatchView, 0, len(batches))
	for _, b := range batches {
		out = append(out, toImportBatchView(b))
	}
	return out
}

func toImportRowView(r *port.ImportRow) ImportRowView {
	if r == nil {
		return ImportRowView{}
	}
	v := ImportRowView{
		RowNo:            r.RowNo,
		RawData:          r.RawData,
		ValidationStatus: string(r.ValidationStatus),
		Disposition:      string(r.Disposition),
		Errors:           r.Errors,
		Warnings:         r.Warnings,
		ProcessedAt:      r.ProcessedAt,
		ErrorMessage:     r.ErrorMessage,
	}
	if r.MatchedEntityID != nil {
		v.MatchedEntityID = ptrString(r.MatchedEntityID.String())
	}
	if r.CreatedEntityID != nil {
		v.CreatedEntityID = ptrString(r.CreatedEntityID.String())
	}
	return v
}

func toImportRowViews(rows []*port.ImportRow) []ImportRowView {
	out := make([]ImportRowView, 0, len(rows))
	for _, r := range rows {
		out = append(out, toImportRowView(r))
	}
	return out
}
