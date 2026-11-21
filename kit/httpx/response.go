package httpx

import (
	"crypto/rand"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Response is the envelope for business API JSON responses. Code is a stable
// application code, separate from the HTTP status; Message is for display only.
type Response struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Data       any         `json:"data"`
	RequestID  string      `json:"request_id"`
	Pagination *Pagination `json:"pagination,omitempty"`
}

// Pagination uses the same limit/offset vocabulary as list query parameters.
// Total counts all matching records before applying limit and offset.
type Pagination struct {
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
	Total  int64 `json:"total"`
}

const requestIDKey = "starter-kit.request_id"

// RequestID generates one server-owned ID per request. Middleware and response
// helpers share it so JSON, headers and request logs can be correlated.
func RequestID(c *gin.Context) string {
	id := c.GetString(requestIDKey)
	if id == "" {
		id = rand.Text()
		c.Set(requestIDKey, id)
		// Set the header once, when the ID is created, so later calls never
		// touch headers after the body may already be written.
		c.Header("X-Request-ID", id)
	}
	return id
}

func Data(c *gin.Context, status int, value any) {
	if status == http.StatusNoContent {
		NoContent(c)
		return
	}
	c.JSON(status, Response{Code: "ok", Message: "success", Data: value, RequestID: RequestID(c)})
}

// Page always renders a JSON array, including when the supplied slice is nil.
func Page[T any](c *gin.Context, items []T, pagination Pagination) {
	if items == nil {
		items = []T{}
	}
	c.JSON(http.StatusOK, Response{
		Code: "ok", Message: "success", Data: items,
		RequestID: RequestID(c), Pagination: &pagination,
	})
}

// NoContent deliberately has no JSON envelope or content type.
func NoContent(c *gin.Context) {
	RequestID(c)
	c.Status(http.StatusNoContent)
}

func Reject(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, Response{Code: code, Message: message, Data: nil, RequestID: RequestID(c)})
}
