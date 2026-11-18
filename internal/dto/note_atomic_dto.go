package dto

// Explicit conditional-write opt-in. JSON fields and these headers are equivalent.
// A present header must not disagree with the corresponding JSON field.
// 显式条件写入。JSON 字段与下列请求头等价；两者同时出现时不得互相矛盾。
const (
	HeaderAtomicWrite         = "X-FNS-Atomic"
	HeaderExpectedContentHash = "X-FNS-Expected-Content-Hash"
	HeaderExpectedVersion     = "X-FNS-Expected-Version"
	HeaderExpectedMtime       = "X-FNS-Expected-Mtime"
)

// Reason values on a code 531 NoteRevisionConflict.
// code 531 的 NoteRevisionConflict.reason。
const (
	NoteConflictStaleRevision       = "stale_revision"
	NoteConflictNotFound            = "not_found"
	NoteConflictDeleted             = "deleted"
	NoteConflictAtomicDisabled      = "atomic_disabled"
	NoteConflictInvalidPrecondition = "invalid_precondition"
)

// AtomicPrecondition is an explicit compare-and-swap opt-in for one note write.
// Atomic false (the zero value, and every existing sync client) keeps
// last-writer-wins. baseHash is not consulted: WebSocket clients send it for
// 3-way merge, and REST has always ignored it.
//
// Atomic true is update-only. Inside the same per-note lock as REST
// POST/PATCH/replace/append/prepend and WebSocket NoteModify, the service
// re-reads the row and requires all of the following before any content or
// metadata mutation:
//
//   - the note exists and its action is not delete
//   - contentHash == ExpectedContentHash
//   - version == *ExpectedVersion
//   - mtime == *ExpectedMtime
//
// ExpectedVersion and ExpectedMtime are pointers so 0 is a real revision.
// A missing pointer is invalid_precondition, not a wildcard. Mismatch,
// absence, and soft-delete return code 531 and do not create or resurrect.
//
// contentHash is util.EncodeHash32: the JavaScript charCodeAt hash over
// UTF-16 code units, as a signed 32-bit decimal string. It is not MD5.
// The stored hash of an accepted atomic write is recomputed with that function.
//
// Client gate, because a 3.6.1 server ignores atomic and still returns code 1:
//  1. Immediately before every submit, GET /api/health (startup alone is not enough).
//  2. Require data.capabilities.atomicNoteWrite.supported == true and enabled == true.
//     A missing capabilities object means the binary cannot do this. Do not send the write.
//  3. On code 1, require data.atomicApplied == true. If it is absent, the write may
//     already have landed without a compare; do not treat it as an atomic commit
//     and do not send another write until health proves this binary again.
//
// 客户端门槛：3.6.1 会忽略 atomic 并返回 code 1。每次提交前立刻 GET /api/health。
// 必须同时看到 supported 与 enabled。成功响应还必须带 data.atomicApplied。
//
// AtomicPrecondition 是单次笔记写入的显式比较并交换。Atomic 为 false（零值，
// 以及所有现有同步客户端）保持后写覆盖。baseHash 不是前置条件。
// Atomic 为 true 时只允许更新：在与 REST/WS 笔记写入相同的按路径锁内重新读取，
// 修订不一致、笔记不存在或已软删除都返回 531，且不创建、不恢复、不修改。
type AtomicPrecondition struct {
	Atomic              bool   `json:"atomic,omitempty"`
	ExpectedContentHash string `json:"expectedContentHash,omitempty"`
	ExpectedVersion     *int64 `json:"expectedVersion,omitempty"`
	ExpectedMtime       *int64 `json:"expectedMtime,omitempty"`
}

// NoteRevisionConflict is the data object of business code 531.
// Read reason before the server fields. Server version/mtime are meaningful
// only when a row was read (stale_revision, deleted).
// NoteRevisionConflict 是业务码 531 的 data。先看 reason。
type NoteRevisionConflict struct {
	Conflict            bool   `json:"conflict"`
	Reason              string `json:"reason"`
	Path                string `json:"path,omitempty"`
	PathHash            string `json:"pathHash,omitempty"`
	ExpectedContentHash string `json:"expectedContentHash,omitempty"`
	ExpectedVersion     *int64 `json:"expectedVersion,omitempty"`
	ExpectedMtime       *int64 `json:"expectedMtime,omitempty"`
	ServerContentHash   string `json:"serverContentHash,omitempty"`
	ServerVersion       int64  `json:"serverVersion"`
	ServerMtime         int64  `json:"serverMtime"`
	ServerAction        string `json:"serverAction,omitempty"`
}
