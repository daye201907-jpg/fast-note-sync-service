package api_router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/haierkeys/fast-note-sync-service/internal/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newHealthTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("GET", "/api/health", nil)
	c.Request = req
	return c, w
}

func TestHealthHandler_Check_Success(t *testing.T) {
	// Use in-memory SQLite for health check test
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}

	testApp := app.NewTestApp(nil, db)
	handler := NewHealthHandler(testApp)
	c, w := newHealthTestContext()

	handler.Check(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assertResponseCode(t, w, code.Success.Code())

	assert.Contains(t, w.Body.String(), `"status":"healthy"`)
	assert.Contains(t, w.Body.String(), `"database":"connected"`)
	assert.Contains(t, w.Body.String(), `"atomicNoteWrite":{"supported":true,"enabled":true}`)
}

func TestHealthHandler_Check_AtomicDisabledKeepsLegacyFields(t *testing.T) {
	t.Setenv("FNS_ATOMIC_NOTE_WRITE", "0")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}
	handler := NewHealthHandler(app.NewTestApp(nil, db))
	c, w := newHealthTestContext()
	handler.Check(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assertResponseCode(t, w, code.Success.Code())
	assert.Contains(t, w.Body.String(), `"status":"healthy"`)
	assert.Contains(t, w.Body.String(), `"database":"connected"`)
	assert.Contains(t, w.Body.String(), `"atomicNoteWrite":{"supported":true,"enabled":false}`)
}
