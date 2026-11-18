package service

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/haierkeys/fast-note-sync-service/internal/domain"
	"github.com/haierkeys/fast-note-sync-service/internal/dto"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"
	"github.com/haierkeys/fast-note-sync-service/pkg/util"
)

// AtomicNoteWriteEnabled reports whether an explicit conditional write is enforced.
// Unset or any value other than 0/false/off/no keeps it on.
// Turning it off rejects the opt-in and writes nothing.
// AtomicNoteWriteEnabled 表示显式条件写入是否生效。关闭时拒绝 opt-in 且不写入。
func AtomicNoteWriteEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FNS_ATOMIC_NOTE_WRITE"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func noteWriteKey(uid, vaultID int64, pathHash string) string {
	return fmt.Sprintf("modify_or_create_%d_%d_%s", uid, vaultID, pathHash)
}

// HoldNoteWriteLock serializes fn with ModifyOrCreate for the same note paths.
// HoldNoteWriteLock 让 fn 与同一笔记路径上的 ModifyOrCreate 互斥。
func (s *noteService) HoldNoteWriteLock(uid, vaultID int64, pathHashes []string, fn func() error) error {
	if fn == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(pathHashes))
	keys := make([]string, 0, len(pathHashes))
	for _, pathHash := range pathHashes {
		key := noteWriteKey(uid, vaultID, pathHash)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	unlocks := make([]func(), 0, len(keys))
	for _, key := range keys {
		unlocks = append(unlocks, s.kmu.Lock(key))
	}
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	return fn()
}

func atomicNoteWriteEnabled() bool {
	return AtomicNoteWriteEnabled()
}

// prepareAtomicWrite checks the opt-in before the per-note lock is taken.
// It does not read or write the note. On success, ContentHash is the canonical
// EncodeHash32 of Content.
// prepareAtomicWrite 在获取按笔记锁之前校验 opt-in，不读不写笔记。
func prepareAtomicWrite(params *dto.NoteModifyOrCreateRequest) error {
	if params == nil || !params.Atomic {
		return nil
	}
	if !atomicNoteWriteEnabled() {
		return noteRevisionConflict(dto.NoteConflictAtomicDisabled, params, nil)
	}
	if params.ExpectedContentHash == "" || params.ExpectedVersion == nil || params.ExpectedMtime == nil {
		return noteRevisionConflict(dto.NoteConflictInvalidPrecondition, params, nil)
	}
	canonical := util.EncodeHash32(params.Content)
	if params.ContentHash == "" {
		params.ContentHash = canonical
		return nil
	}
	if params.ContentHash != canonical {
		return noteRevisionConflict(dto.NoteConflictInvalidPrecondition, params, nil)
	}
	return nil
}

// applyAtomicPrecondition runs only after a fresh read inside the per-note write lock.
// noop is a byte-identical retry: the revision matches and the body is already stored,
// so the caller gets success and the row is left untouched.
// applyAtomicPrecondition 只在按笔记写锁内的新读取之后执行。
// noop 表示修订匹配且正文已相同，成功返回且不改行。
func applyAtomicPrecondition(note *domain.Note, params *dto.NoteModifyOrCreateRequest) (noop bool, err error) {
	if note == nil {
		return false, noteRevisionConflict(dto.NoteConflictNotFound, params, nil)
	}
	// Soft-delete does not change contentHash, version, or mtime. Treat it as its own revision
	// so an atomic update cannot resurrect a note another client just trashed.
	// 软删除不改变 contentHash、version、mtime，必须单独拒绝，避免条件更新把笔记从回收站救回。
	if note.Action == domain.NoteActionDelete {
		return false, noteRevisionConflict(dto.NoteConflictDeleted, params, note)
	}
	if note.ContentHash != params.ExpectedContentHash ||
		note.Version != *params.ExpectedVersion ||
		note.Mtime != *params.ExpectedMtime {
		return false, noteRevisionConflict(dto.NoteConflictStaleRevision, params, note)
	}
	if params.Content == note.Content {
		return true, nil
	}
	return false, nil
}

func noteRevisionConflict(reason string, params *dto.NoteModifyOrCreateRequest, note *domain.Note) error {
	payload := &dto.NoteRevisionConflict{
		Conflict: true,
		Reason:   reason,
	}
	if params != nil {
		payload.Path = params.Path
		payload.PathHash = params.PathHash
		payload.ExpectedContentHash = params.ExpectedContentHash
		payload.ExpectedVersion = params.ExpectedVersion
		payload.ExpectedMtime = params.ExpectedMtime
	}
	if note != nil {
		payload.ServerContentHash = note.ContentHash
		payload.ServerVersion = note.Version
		payload.ServerMtime = note.Mtime
		payload.ServerAction = string(note.Action)
	}
	return code.ErrorNoteRevisionConflict.WithData(payload)
}
