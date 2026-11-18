package api_router

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/haierkeys/fast-note-sync-service/internal/dto"
	pkgapp "github.com/haierkeys/fast-note-sync-service/pkg/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"
	apperrors "github.com/haierkeys/fast-note-sync-service/pkg/errors"
)

// applyAtomicWriteHeaders copies the conditional-write headers onto the JSON
// precondition. A header that disagrees with a JSON field is rejected before
// any note read or write. Absent headers leave the JSON value unchanged.
// applyAtomicWriteHeaders 把条件写入请求头合并进 JSON 前置条件。
// 请求头与 JSON 矛盾时，在任何笔记读写之前拒绝。
func applyAtomicWriteHeaders(c *gin.Context, p *dto.AtomicPrecondition) error {
	if c == nil || c.Request == nil || p == nil {
		return nil
	}
	h := c.Request.Header
	if raw := strings.TrimSpace(h.Get(dto.HeaderAtomicWrite)); raw != "" {
		on, err := parseAtomicFlag(raw)
		if err != nil {
			return err
		}
		if p.Atomic && !on {
			return fmt.Errorf("%s disagrees with JSON atomic", dto.HeaderAtomicWrite)
		}
		if on {
			p.Atomic = true
		}
	}
	if raw := strings.TrimSpace(h.Get(dto.HeaderExpectedContentHash)); raw != "" {
		if p.ExpectedContentHash != "" && p.ExpectedContentHash != raw {
			return fmt.Errorf("%s disagrees with JSON expectedContentHash", dto.HeaderExpectedContentHash)
		}
		p.ExpectedContentHash = raw
	}
	if raw := strings.TrimSpace(h.Get(dto.HeaderExpectedVersion)); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s is not a base-10 integer", dto.HeaderExpectedVersion)
		}
		if p.ExpectedVersion != nil && *p.ExpectedVersion != v {
			return fmt.Errorf("%s disagrees with JSON expectedVersion", dto.HeaderExpectedVersion)
		}
		vv := v
		p.ExpectedVersion = &vv
	}
	if raw := strings.TrimSpace(h.Get(dto.HeaderExpectedMtime)); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s is not a base-10 integer", dto.HeaderExpectedMtime)
		}
		if p.ExpectedMtime != nil && *p.ExpectedMtime != v {
			return fmt.Errorf("%s disagrees with JSON expectedMtime", dto.HeaderExpectedMtime)
		}
		vv := v
		p.ExpectedMtime = &vv
	}
	return nil
}

func parseAtomicFlag(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "require":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be 1 or 0", dto.HeaderAtomicWrite)
	}
}

// respondWriteErr writes a note mutation error.
// Code 531 uses the standard Res envelope (code/status/message/data) so REST
// and WebSocket clients see the same conflict object. Other errors keep the
// existing AppError response.
// respondWriteErr 写笔记变更错误。531 使用与 WebSocket 相同的 Res 信封。
func respondWriteErr(c *gin.Context, err error) {
	var codeErr *code.Code
	if errors.As(err, &codeErr) && codeErr.Is(code.ErrorNoteRevisionConflict) {
		pkgapp.NewResponse(c).ToResponse(codeErr)
		return
	}
	apperrors.ErrorResponse(c, err)
}
