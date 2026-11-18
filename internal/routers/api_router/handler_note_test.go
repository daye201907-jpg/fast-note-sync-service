package api_router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/haierkeys/fast-note-sync-service/internal/app"
	"github.com/haierkeys/fast-note-sync-service/internal/domain"
	"github.com/haierkeys/fast-note-sync-service/internal/dto"
	svcmocks "github.com/haierkeys/fast-note-sync-service/internal/service/mocks"
	pkgapp "github.com/haierkeys/fast-note-sync-service/pkg/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newNoteTestContext creates a gin.Context suitable for NoteHandler tests.
func newNoteTestContext(method, url, body string, uid int64) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, url, nil)
	}
	req.Header.Set("X-Client", "Web")

	c, _ := gin.CreateTestContext(w)
	c.Request = req
	if uid > 0 {
		c.Set("user_token", &pkgapp.UserEntity{UID: uid})
	}
	return c, w
}

// newTestNoteHandler creates a NoteHandler with mock services.
func newTestNoteHandler(noteSvc *svcmocks.MockNoteService, fileSvc *svcmocks.MockFileService) *NoteHandler {
	testApp := app.NewTestApp(&app.Services{
		NoteService: noteSvc,
		FileService: fileSvc,
	})
	if noteSvc != nil {
		noteSvc.On("WithClient", mock.Anything, mock.Anything, mock.Anything).Return(noteSvc)
	}
	if fileSvc != nil {
		fileSvc.On("WithClient", mock.Anything, mock.Anything, mock.Anything).Return(fileSvc)
	}
	// WSS is used in NoteHandler for broadcasting
	wss := pkgapp.NewWebsocketServer(pkgapp.WSConfig{}, testApp)
	return NewNoteHandler(testApp, wss)
}

// TestNoteHandler_Get_Success verifies successful note fetch
// TestNoteHandler_Get_Success 验证成功获取笔记
func TestNoteHandler_Get_Success(t *testing.T) {
	mockNoteSvc := new(svcmocks.MockNoteService)
	mockFileSvc := new(svcmocks.MockFileService)

	mockData := &dto.NoteDTO{
		ID:       1,
		Path:     "test.md",
		PathHash: "hash123",
		Content:  "content",
	}

	mockNoteSvc.On("Get", mock.Anything, int64(1), mock.AnythingOfType("*dto.NoteGetRequest")).
		Return(mockData, nil)

	mockFileSvc.On("ResolveEmbedLinks", mock.Anything, int64(1), "main", "test.md", "content").
		Return(map[string]string{}, nil)

	handler := newTestNoteHandler(mockNoteSvc, mockFileSvc)
	c, w := newNoteTestContext("GET", "/api/note", "", 1)

	// Simulate binding query params
	c.Request.URL.RawQuery = "vault=main&path=test.md"
	handler.Get(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assertResponseCode(t, w, code.Success.Code())
}

// TestNoteHandler_List_Success verifies successful note list fetch
// TestNoteHandler_List_Success 验证成功获取笔记列表
func TestNoteHandler_List_Success(t *testing.T) {
	mockNoteSvc := new(svcmocks.MockNoteService)
	listData := []*dto.NoteNoContentDTO{
		{ID: 1, Path: "test1.md"},
		{ID: 2, Path: "test2.md"},
	}

	mockNoteSvc.On("List", mock.Anything, int64(1), mock.AnythingOfType("*dto.NoteListRequest"), mock.AnythingOfType("*app.Pager")).
		Return(listData, 2, nil)

	handler := newTestNoteHandler(mockNoteSvc, nil)
	c, w := newNoteTestContext("GET", "/api/notes", "", 1)
	c.Request.URL.RawQuery = "vault=main&page=1&size=10"

	handler.List(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assertResponseCode(t, w, code.Success.Code())
}

// TestNoteHandler_CreateOrUpdate_Success verifies successful note creation/update
// TestNoteHandler_CreateOrUpdate_Success 验证成功创建或更新笔记
func TestNoteHandler_CreateOrUpdate_Success(t *testing.T) {
	mockNoteSvc := new(svcmocks.MockNoteService)

	mockNoteSvc.On("UpdateCheckWithNote", mock.Anything, int64(1), mock.AnythingOfType("*dto.NoteUpdateCheckRequest")).
		Return("", (*domain.Note)(nil), (*dto.NoteDTO)(nil), nil)

	createdNote := &dto.NoteDTO{ID: 2, Path: "create.md"}
	mockNoteSvc.On("ModifyOrCreate", mock.Anything, int64(1), mock.AnythingOfType("*dto.NoteModifyOrCreateRequest"), false, mock.Anything).
		Return(true, createdNote, nil)

	handler := newTestNoteHandler(mockNoteSvc, nil)
	body := `{"vault":"main", "path":"create.md", "content":"hello"}`
	c, w := newNoteTestContext("POST", "/api/note", body, 1)

	handler.CreateOrUpdate(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assertResponseCode(t, w, code.Success.Code())
}

func TestNoteHandler_CreateOrUpdate_AtomicHeadersAndConflict(t *testing.T) {
	ver := int64(4)
	mt := int64(50)
	mockNoteSvc := new(svcmocks.MockNoteService)
	mockNoteSvc.On("ModifyOrCreate", mock.Anything, int64(1), mock.MatchedBy(func(p *dto.NoteModifyOrCreateRequest) bool {
		return p.Atomic &&
			p.ExpectedContentHash == "server-hash" &&
			p.ExpectedVersion != nil && *p.ExpectedVersion == ver &&
			p.ExpectedMtime != nil && *p.ExpectedMtime == mt &&
			p.Content == "next body" &&
			p.BaseHash == "ignored-by-atomic"
	}), false, mock.Anything).Return(false, (*dto.NoteDTO)(nil), code.ErrorNoteRevisionConflict.WithData(&dto.NoteRevisionConflict{
		Conflict:          true,
		Reason:            dto.NoteConflictStaleRevision,
		Path:              "todo.md",
		PathHash:          "path-todo",
		ServerContentHash: "newer-hash",
		ServerVersion:     5,
		ServerMtime:       80,
		ServerAction:      "modify",
	}))

	handler := newTestNoteHandler(mockNoteSvc, nil)
	body := `{"vault":"main","path":"todo.md","pathHash":"path-todo","content":"next body","contentHash":"99162322","baseHash":"ignored-by-atomic","ctime":1,"mtime":60,"atomic":true,"expectedContentHash":"server-hash","expectedVersion":4,"expectedMtime":50}`
	c, w := newNoteTestContext("POST", "/api/note", body, 1)
	c.Request.Header.Set(dto.HeaderAtomicWrite, "1")
	c.Request.Header.Set(dto.HeaderExpectedContentHash, "server-hash")

	handler.CreateOrUpdate(c)

	require.Equal(t, http.StatusOK, w.Code)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &decoded))
	require.Equal(t, float64(531), decoded["code"])
	require.Equal(t, false, decoded["status"])
	data, ok := decoded["data"].(map[string]any)
	require.True(t, ok, "body=%s", w.Body.String())
	require.Equal(t, true, data["conflict"])
	require.Equal(t, dto.NoteConflictStaleRevision, data["reason"])
	require.Equal(t, "newer-hash", data["serverContentHash"])
	require.Equal(t, float64(5), data["serverVersion"])
	require.Equal(t, float64(80), data["serverMtime"])
	mockNoteSvc.AssertNotCalled(t, "UpdateCheckWithNote", mock.Anything, mock.Anything, mock.Anything)
}

func TestNoteHandler_CreateOrUpdate_AtomicHeaderDisagreement(t *testing.T) {
	mockNoteSvc := new(svcmocks.MockNoteService)
	handler := newTestNoteHandler(mockNoteSvc, nil)
	body := `{"vault":"main","path":"todo.md","content":"next","atomic":true,"expectedContentHash":"one","expectedVersion":1,"expectedMtime":1}`
	c, w := newNoteTestContext("POST", "/api/note", body, 1)
	c.Request.Header.Set(dto.HeaderExpectedContentHash, "two")

	handler.CreateOrUpdate(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assertResponseCode(t, w, code.ErrorInvalidParams.Code())
	mockNoteSvc.AssertNotCalled(t, "ModifyOrCreate", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestNoteHandler_Delete_Success verifies successful note deletion
// TestNoteHandler_Delete_Success 验证成功删除笔记
func TestNoteHandler_Delete_Success(t *testing.T) {
	mockNoteSvc := new(svcmocks.MockNoteService)

	existingNote := &dto.NoteDTO{ID: 3, Path: "del.md", Action: ""}
	mockNoteSvc.On("Get", mock.Anything, int64(1), mock.AnythingOfType("*dto.NoteGetRequest")).
		Return(existingNote, nil)

	mockNoteSvc.On("Delete", mock.Anything, int64(1), mock.AnythingOfType("*dto.NoteDeleteRequest")).
		Return(existingNote, nil)

	handler := newTestNoteHandler(mockNoteSvc, nil)
	c, w := newNoteTestContext("DELETE", "/api/note", "", 1)
	c.Request.URL.RawQuery = "vault=main&path=del.md"

	handler.Delete(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assertResponseCode(t, w, code.Success.Code())
}
