package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Typed group-engine errors the handlers map to HTTP statuses (§4). The group
// engine (H) returns these sentinels; the API matches with errors.Is. They are
// declared here (consumer side) and re-exported so H can return the exact
// values, or H may return its own — the handler also falls back to a 500 for
// unknown errors.
var (
	ErrNotMaster       = errors.New("not master")
	ErrNotAlive        = errors.New("target not alive")
	ErrTargetNotMaster = errors.New("target not a master")
	ErrUnknownNode     = errors.New("unknown node")
	ErrNoCodec         = errors.New("unsupported codec")
	ErrNotPlaying      = errors.New("nothing is playing")
	ErrNotPaused       = errors.New("not paused")
	ErrMediaNotFound   = errors.New("media not found")
	ErrBadScheme       = errors.New("bad scheme")
	ErrBadPath         = errors.New("bad path")
)

// errStatus maps a group-engine error to (httpStatus, code, hint). Unknown
// errors become 500 internal_error. contracts.FollowClient and the engine share
// these sentinels via the api package (H imports api's errors only through the
// consumer interface return values, which are plain error).
func errStatus(err error) (int, string, string) {
	switch {
	case err == nil:
		return http.StatusOK, "", ""
	case errors.Is(err, ErrNotMaster):
		return http.StatusConflict, "not_master", "use POST /api/group/master to take over first"
	case errors.Is(err, ErrNotAlive):
		return http.StatusConflict, "not_alive", ""
	case errors.Is(err, ErrTargetNotMaster):
		return http.StatusConflict, "target_not_master", ""
	case errors.Is(err, ErrUnknownNode):
		return http.StatusNotFound, "unknown_node", ""
	case errors.Is(err, ErrNoCodec):
		return http.StatusBadRequest, "unsupported_codec", ""
	case errors.Is(err, ErrNotPlaying):
		return http.StatusConflict, "not_playing", ""
	case errors.Is(err, ErrNotPaused):
		return http.StatusConflict, "not_paused", ""
	case errors.Is(err, ErrMediaNotFound):
		return http.StatusNotFound, "media_not_found", ""
	case errors.Is(err, ErrBadScheme):
		return http.StatusBadRequest, "bad_scheme", ""
	case errors.Is(err, ErrBadPath):
		return http.StatusBadRequest, "bad_path", ""
	default:
		return http.StatusInternalServerError, "internal_error", ""
	}
}

// fail writes the JSON error envelope with the mapped status.
func (s *Server) fail(c echo.Context, err error) error {
	status, code, hint := errStatus(err)
	return c.JSON(status, ErrorResp{Error: code, Hint: hint})
}

// failCode writes a fixed status + machine code (validation errors).
func failCode(c echo.Context, status int, code, hint string) error {
	return c.JSON(status, ErrorResp{Error: code, Hint: hint})
}

// jsonErrorHandler renders Echo's own errors (404, bind 400, panics) as the
// JSON ErrorResp envelope instead of HTML/text (§4).
func jsonErrorHandler(log *slog.Logger) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		status := http.StatusInternalServerError
		code := "internal_error"
		var he *echo.HTTPError
		if errors.As(err, &he) {
			status = he.Code
			code = httpCodeName(status)
		} else if errors.Is(err, context.Canceled) {
			return
		}
		if status >= 500 {
			log.Warn("request error", "status", status, "err", err)
		}
		_ = c.JSON(status, ErrorResp{Error: code})
	}
}

func httpCodeName(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusConflict:
		return "conflict"
	default:
		return "internal_error"
	}
}
