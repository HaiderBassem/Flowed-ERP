package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// OK writes 200 with the payload.
func OK(c *gin.Context, payload any) {
	c.JSON(http.StatusOK, payload)
}

// Created writes 201 with the payload. Used by the commands that bring
// something new into existence — a student, an enrollment, a payment.
func Created(c *gin.Context, payload any) {
	c.JSON(http.StatusCreated, payload)
}

// NoContent writes 204 with no body, for a command whose only interesting
// outcome is that it succeeded.
func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// Page is the envelope for every list endpoint. Total is the count matching
// the filter rather than the length of Data, so a client can page without
// probing for the end.
type Page[T any] struct {
	Data   []T `json:"data"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// NewPage builds a page envelope.
//
// A nil slice is replaced with an empty one so the JSON reads "data": []
// rather than "data": null. Clients that iterate the field without a nil check
// are common, and an empty result is not an error worth crashing a cashier's
// screen over.
func NewPage[T any](items []T, total, limit, offset int) Page[T] {
	if items == nil {
		items = []T{}
	}
	return Page[T]{Data: items, Total: total, Limit: limit, Offset: offset}
}

// OKPage writes 200 with a page envelope.
func OKPage[T any](c *gin.Context, items []T, total, limit, offset int) {
	OK(c, NewPage(items, total, limit, offset))
}
