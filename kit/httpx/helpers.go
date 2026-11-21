package httpx

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

// ListParams holds the raw limit/offset query values. Zero means "not given"
// for limit; services apply their own defaults and bounds, including for
// callers that do not come through HTTP.
type ListParams struct{ Limit, Offset int }

// ParseListParams reads ?limit= and ?offset=. On a non-integer value it writes
// a 400 response and returns false; the handler should then return.
func ParseListParams(c *gin.Context) (ListParams, bool) {
	var in ListParams
	for _, field := range []struct {
		name   string
		target *int
	}{{"limit", &in.Limit}, {"offset", &in.Offset}} {
		if raw, ok := c.GetQuery(field.name); ok {
			n, err := strconv.Atoi(raw)
			if err != nil {
				Fail(c, apperror.Invalid(field.name+" must be an integer"))
				return in, false
			}
			*field.target = n
		}
	}
	return in, true
}

// BearerToken returns the token from "Authorization: Bearer <token>", or ""
// when the header is missing or malformed.
func BearerToken(c *gin.Context) string {
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

// BindJSON decodes the request body into T and calls fn only on success;
// decoding errors are already answered by DecodeJSON.
func BindJSON[T any](c *gin.Context, fn func(T)) {
	var in T
	if DecodeJSON(c, &in) {
		fn(in)
	}
}

// Result writes value with status, or the mapped error when err is not nil.
func Result(c *gin.Context, status int, value any, err error) {
	if err != nil {
		Fail(c, err)
		return
	}
	Data(c, status, value)
}

// PageResult writes a list page, or the mapped error when err is not nil.
func PageResult[T any](c *gin.Context, items []T, pagination Pagination, err error) {
	if err != nil {
		Fail(c, err)
		return
	}
	Page(c, items, pagination)
}
