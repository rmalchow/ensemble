package api

import (
	"log/slog"
	"net/http"
	"runtime"

	"github.com/labstack/echo/v4"
)

// recoverMiddleware turns a handler panic into a 500 JSON error rather than
// crashing the process. Replaces echo's middleware.Recover (which we cannot
// import without pulling golang.org/x/time into go.sum).
func recoverMiddleware(log *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			defer func() {
				if r := recover(); r != nil {
					buf := make([]byte, 4<<10)
					n := runtime.Stack(buf, false)
					log.Error("panic recovered", "err", r, "stack", string(buf[:n]))
					err = c.JSON(http.StatusInternalServerError, ErrorResp{Error: "internal_error"})
				}
			}()
			return next(c)
		}
	}
}

// bodyLimitMiddleware caps request bodies at limit bytes (§9, body size guard).
// A declared oversize Content-Length is rejected up front with 413; the body is
// also wrapped in a MaxBytesReader to bound chunked/unknown-length requests.
func bodyLimitMiddleware(limit int64) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			if req.ContentLength > limit {
				return c.JSON(http.StatusRequestEntityTooLarge, ErrorResp{Error: "too_large"})
			}
			if req.Body != nil {
				req.Body = http.MaxBytesReader(c.Response(), req.Body, limit)
			}
			return next(c)
		}
	}
}
