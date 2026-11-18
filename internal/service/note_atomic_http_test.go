package service_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/haierkeys/fast-note-sync-service/internal/app"
	"github.com/haierkeys/fast-note-sync-service/internal/domain"
	"github.com/haierkeys/fast-note-sync-service/internal/dto"
	"github.com/haierkeys/fast-note-sync-service/internal/routers/api_router"
	"github.com/haierkeys/fast-note-sync-service/internal/service"
	pkgapp "github.com/haierkeys/fast-note-sync-service/pkg/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/util"
	"github.com/stretchr/testify/require"
)

func TestAtomicHTTP_OptInParsingConflictAndSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("json success then stale 531", func(t *testing.T) {
		_, handler, note := newAtomicHTTP(t)
		w := postAtomic(t, handler, note, "beta", nil)
		body := decodeBody(t, w)
		require.Equal(t, float64(1), body["code"])
		data := body["data"].(map[string]any)
		require.Equal(t, true, data["atomicApplied"])
		require.Equal(t, "beta", data["content"])

		w = postAtomic(t, handler, note, "gamma", nil)
		body = decodeBody(t, w)
		require.Equal(t, float64(531), body["code"])
		require.Equal(t, false, body["status"])
		conflict := body["data"].(map[string]any)
		require.Equal(t, true, conflict["conflict"])
		require.Equal(t, dto.NoteConflictStaleRevision, conflict["reason"])
		require.NotEqual(t, note.ContentHash, conflict["serverContentHash"])
		require.NotContains(t, w.Body.String(), `"atomicApplied":true`)
	})

	t.Run("legacy success omits atomicApplied", func(t *testing.T) {
		repo, handler, note := newAtomicHTTP(t)
		raw := fmt.Sprintf(`{"vault":"vault","path":%q,"pathHash":%q,"content":"legacy-body","ctime":111,"mtime":80}`, note.Path, note.PathHash)
		w := postNote(handler, raw, nil)
		body := decodeBody(t, w)
		require.Equal(t, float64(1), body["code"])
		require.NotContains(t, w.Body.String(), "atomicApplied")
		require.Equal(t, "legacy-body", repo.must(note.PathHash).Content)
	})

	t.Run("missing expectedVersion is 531 and does not write", func(t *testing.T) {
		repo, handler, note := newAtomicHTTP(t)
		raw := fmt.Sprintf(`{"vault":"vault","path":%q,"pathHash":%q,"content":"nope","ctime":111,"mtime":80,"atomic":true,"expectedContentHash":%q,"expectedMtime":%d}`,
			note.Path, note.PathHash, note.ContentHash, note.Mtime)
		w := postNote(handler, raw, nil)
		body := decodeBody(t, w)
		require.Equal(t, float64(531), body["code"])
		require.Equal(t, dto.NoteConflictInvalidPrecondition, body["data"].(map[string]any)["reason"])
		require.Equal(t, note.Content, repo.must(note.PathHash).Content)
	})

	t.Run("headers agree with json precondition", func(t *testing.T) {
		_, handler, note := newAtomicHTTP(t)
		w := postAtomic(t, handler, note, "from-header", map[string]string{
			dto.HeaderAtomicWrite:         "1",
			dto.HeaderExpectedContentHash: note.ContentHash,
			dto.HeaderExpectedVersion:     fmt.Sprintf("%d", note.Version),
			dto.HeaderExpectedMtime:       fmt.Sprintf("%d", note.Mtime),
		})
		// JSON also carries the precondition; headers agree.
		body := decodeBody(t, w)
		require.Equal(t, float64(1), body["code"], w.Body.String())
		require.Equal(t, true, body["data"].(map[string]any)["atomicApplied"])
	})

	t.Run("header only when json atomic is absent", func(t *testing.T) {
		repo, handler, note := newAtomicHTTP(t)
		raw := fmt.Sprintf(`{"vault":"vault","path":%q,"pathHash":%q,"content":"header-only","ctime":111,"mtime":90}`, note.Path, note.PathHash)
		w := postNote(handler, raw, map[string]string{
			dto.HeaderAtomicWrite:         "require",
			dto.HeaderExpectedContentHash: note.ContentHash,
			dto.HeaderExpectedVersion:     fmt.Sprintf("%d", note.Version),
			dto.HeaderExpectedMtime:       fmt.Sprintf("%d", note.Mtime),
		})
		body := decodeBody(t, w)
		require.Equal(t, float64(1), body["code"], w.Body.String())
		require.Equal(t, true, body["data"].(map[string]any)["atomicApplied"])
		require.Equal(t, "header-only", repo.must(note.PathHash).Content)
	})

	t.Run("header disagrees with json", func(t *testing.T) {
		repo, handler, note := newAtomicHTTP(t)
		raw := fmt.Sprintf(`{"vault":"vault","path":%q,"pathHash":%q,"content":"x","ctime":111,"mtime":90,"atomic":true,"expectedContentHash":"one","expectedVersion":1,"expectedMtime":1}`, note.Path, note.PathHash)
		w := postNote(handler, raw, map[string]string{dto.HeaderExpectedContentHash: "two"})
		body := decodeBody(t, w)
		require.Equal(t, float64(305), body["code"], w.Body.String())
		require.Equal(t, note.Content, repo.must(note.PathHash).Content)
	})

	t.Run("switch off rejects before write", func(t *testing.T) {
		t.Setenv("FNS_ATOMIC_NOTE_WRITE", "off")
		repo, handler, note := newAtomicHTTP(t)
		w := postAtomic(t, handler, note, "blocked", nil)
		body := decodeBody(t, w)
		require.Equal(t, float64(531), body["code"])
		require.Equal(t, dto.NoteConflictAtomicDisabled, body["data"].(map[string]any)["reason"])
		require.Equal(t, false, service.AtomicNoteWriteEnabled())
		require.Equal(t, note.Content, repo.must(note.PathHash).Content)
		require.NotContains(t, w.Body.String(), `"atomicApplied":true`)
	})
}

func newAtomicHTTP(t *testing.T) (*memNoteRepo, *api_router.NoteHandler, *domain.Note) {
	t.Helper()
	repo, svc := newAtomicSvc()
	note := repo.seed(liveNote("notes/todo.md", "path-todo", "alpha", 4, 50))
	testApp := app.NewTestApp(&app.Services{NoteService: svc})
	wss := pkgapp.NewWebsocketServer(pkgapp.WSConfig{}, testApp)
	return repo, api_router.NewNoteHandler(testApp, wss), note
}

func postAtomic(t *testing.T, h *api_router.NoteHandler, note *domain.Note, content string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw := fmt.Sprintf(`{"vault":"vault","path":%q,"pathHash":%q,"content":%q,"contentHash":%q,"ctime":111,"mtime":60,"atomic":true,"expectedContentHash":%q,"expectedVersion":%d,"expectedMtime":%d}`,
		note.Path, note.PathHash, content, util.EncodeHash32(content), note.ContentHash, note.Version, note.Mtime)
	return postNote(h, raw, headers)
}

func postNote(h *api_router.NoteHandler, raw string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/note", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_token", &pkgapp.UserEntity{UID: 1})
	h.CreateOrUpdate(c)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body
}
