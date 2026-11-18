package service_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haierkeys/fast-note-sync-service/internal/domain"
	"github.com/haierkeys/fast-note-sync-service/internal/dto"
	"github.com/haierkeys/fast-note-sync-service/internal/service"
	pkgapp "github.com/haierkeys/fast-note-sync-service/pkg/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"
	"github.com/haierkeys/fast-note-sync-service/pkg/util"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// EncodeHash32 contract used by conditional writes. Decimal int32 of the
// JavaScript charCodeAt hash over UTF-16 code units, not MD5.
func TestEncodeHash32_AtomicContractVector(t *testing.T) {
	require.Equal(t, "99162322", util.EncodeHash32("hello"))
	require.Equal(t, "779193", util.EncodeHash32("待办"))
	require.Equal(t, "1164511306", util.EncodeHash32("emoji-😀"))
	require.NotEqual(t, util.EncodeMD5("hello"), util.EncodeHash32("hello"))
}

func TestAtomicWrite_StaleBaseRejectedAfterOtherClient(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("notes/todo.md", "path-todo", "- [ ] synthetic task\n", 4, 1000))
	before := repo.must(original.PathHash)

	_, _, err := svc.ModifyOrCreate(context.Background(), 1, legacyWrite(&before, "other-client-body", before.Mtime+50), true)
	require.NoError(t, err)

	stale := atomicWrite(&before, "- [x] synthetic task\n", before.Mtime+80)
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, stale, false)
	require.Equal(t, dto.NoteConflictStaleRevision, conflictReason(t, err))

	got := repo.must(original.PathHash)
	require.Equal(t, "other-client-body", got.Content)
	require.Equal(t, util.EncodeHash32("other-client-body"), got.ContentHash)
	require.Equal(t, before.Ctime, got.Ctime)
	require.NotEqual(t, stale.Content, got.Content)
}

func TestAtomicWrite_SuccessPreservesUnrelatedMetadata(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("research/round.md", "path-round", "workbench_round_parked: false\n", 7, 2000))
	original.Ctime = 111
	original.ClientName = "obsidian"
	repo.seed(original)

	next := "workbench_round_parked: true\n合成正文\n"
	req := atomicWrite(original, next, 2500)
	req.Ctime = 999
	_, written, err := svc.ModifyOrCreate(context.Background(), 1, req, false)
	require.NoError(t, err)

	got := repo.must(original.PathHash)
	require.Equal(t, next, got.Content)
	require.Equal(t, util.EncodeHash32(next), got.ContentHash)
	require.Equal(t, int64(len(next)), got.Size)
	require.Equal(t, original.Version+1, got.Version)
	require.Equal(t, int64(2500), got.Mtime)
	require.Equal(t, int64(111), got.Ctime)
	require.Equal(t, original.Path, got.Path)
	require.Equal(t, "workbench", got.ClientName)
	require.Equal(t, next, written.Content)
	require.Equal(t, got.ContentHash, written.ContentHash)
	require.Equal(t, got.Version, written.Version)
}

func TestAtomicWrite_RepeatAndSameHash(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("notes/repeat.md", "path-repeat", "same-body", 2, 3000))

	same := atomicWrite(original, original.Content, original.Mtime+1)
	updates := repo.updateCount()
	_, _, err := svc.ModifyOrCreate(context.Background(), 1, same, false)
	require.NoError(t, err)
	require.Equal(t, updates, repo.updateCount())
	got := repo.must(original.PathHash)
	require.Equal(t, original.Content, got.Content)
	require.Equal(t, original.Version, got.Version)
	require.Equal(t, original.Mtime, got.Mtime)
	require.Equal(t, original.ClientName, got.ClientName)

	changed := atomicWrite(original, "changed-once", original.Mtime+5)
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, changed, false)
	require.NoError(t, err)
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, changed, false)
	require.Equal(t, dto.NoteConflictStaleRevision, conflictReason(t, err))
	got = repo.must(original.PathHash)
	require.Equal(t, "changed-once", got.Content)
	require.Equal(t, original.Version+1, got.Version)
}

func TestAtomicWrite_DifferentPathsDoNotCollide(t *testing.T) {
	repo, svc := newAtomicSvc()
	a := repo.seed(liveNote("a.md", "path-a", "body-a", 1, 10))
	b := repo.seed(liveNote("b.md", "path-b", "body-b", 1, 10))

	errc := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, err := svc.ModifyOrCreate(context.Background(), 1, atomicWrite(a, "new-a", 11), false)
		errc <- err
	}()
	go func() {
		defer wg.Done()
		_, _, err := svc.ModifyOrCreate(context.Background(), 1, atomicWrite(b, "new-b", 11), false)
		errc <- err
	}()
	wg.Wait()
	close(errc)
	for err := range errc {
		require.NoError(t, err)
	}

	require.Equal(t, "new-a", repo.must("path-a").Content)
	require.Equal(t, "new-b", repo.must("path-b").Content)
}

func TestAtomicWrite_SamePathConcurrentExactlyOneWins(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("race.md", "path-race", "base", 3, 40))

	const n = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	var wins atomic.Int32
	var winBody atomic.Value
	errc := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf("writer-%02d", i)
			<-start
			_, _, err := svc.ModifyOrCreate(context.Background(), 1, atomicWrite(original, body, 41), false)
			if err == nil {
				wins.Add(1)
				winBody.Store(body)
				errc <- nil
				return
			}
			if reason, ok := revisionReason(err); !ok || reason != dto.NoteConflictStaleRevision {
				errc <- fmt.Errorf("writer %d: %w", i, err)
				return
			}
			errc <- nil
		}(i)
	}
	close(start)
	wg.Wait()
	close(errc)
	for err := range errc {
		require.NoError(t, err)
	}

	require.Equal(t, int32(1), wins.Load())
	got := repo.must("path-race")
	require.Equal(t, winBody.Load().(string), got.Content)
	require.Equal(t, util.EncodeHash32(got.Content), got.ContentHash)
	require.Equal(t, original.Version+1, got.Version)
	require.Equal(t, 1, repo.updateCount())
}

func TestAtomicWrite_LegacyBaseHashStillOverwrites(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("legacy.md", "path-legacy", "server-body", 1, 10))

	withBase := legacyWrite(original, "client-with-stale-base", 20)
	withBase.BaseHash = "not-the-server-hash"
	withBase.BaseHashMissing = false
	_, _, err := svc.ModifyOrCreate(context.Background(), 1, withBase, false)
	require.NoError(t, err)
	require.Equal(t, "client-with-stale-base", repo.must("path-legacy").Content)

	current := repo.must("path-legacy")
	without := legacyWrite(&current, "client-without-base", 30)
	without.BaseHash = ""
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, without, true)
	require.NoError(t, err)
	require.Equal(t, "client-without-base", repo.must("path-legacy").Content)
}

func TestAtomicWrite_MtimeOnlyChangeConflicts(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("mtime.md", "path-mtime", "unchanged-body", 5, 100))
	before := repo.must(original.PathHash)
	ws := legacyWrite(&before, before.Content, 180)
	ws.ContentHash = original.ContentHash
	_, _, err := svc.ModifyOrCreate(context.Background(), 1, ws, true)
	require.NoError(t, err)
	require.Equal(t, int64(180), repo.must("path-mtime").Mtime)
	require.Equal(t, original.Content, repo.must("path-mtime").Content)
	require.Equal(t, original.Version, repo.must("path-mtime").Version)

	_, _, err = svc.ModifyOrCreate(context.Background(), 1, atomicWrite(&before, "workbench", 190), false)
	require.Equal(t, dto.NoteConflictStaleRevision, conflictReason(t, err))
	got := repo.must("path-mtime")
	require.Equal(t, original.Content, got.Content)
	require.Equal(t, int64(180), got.Mtime)
	require.Equal(t, original.Version, got.Version)
}

func TestAtomicWrite_CreateAndDelete(t *testing.T) {
	repo, svc := newAtomicSvc()
	missing := liveNote("new.md", "path-missing", "nope", 0, 0)
	req := atomicWrite(missing, "should-not-create", 1)
	req.ExpectedContentHash = "0"
	_, _, err := svc.ModifyOrCreate(context.Background(), 1, req, false)
	require.Equal(t, dto.NoteConflictNotFound, conflictReason(t, err))
	require.Equal(t, 0, repo.createCount())
	_, ok := repo.get("path-missing")
	require.False(t, ok)

	_, _, err = svc.ModifyOrCreate(context.Background(), 1, legacyWrite(missing, "legacy-create", 1), false)
	require.NoError(t, err)
	require.Equal(t, 1, repo.createCount())
	require.Equal(t, "legacy-create", repo.must("path-missing").Content)

	deleted := liveNote("gone.md", "path-gone", "trashed-body", 2, 8)
	deleted.Action = domain.NoteActionDelete
	repo.seed(deleted)
	before := repo.updateCount()
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, atomicWrite(deleted, "resurrect", 9), false)
	require.Equal(t, dto.NoteConflictDeleted, conflictReason(t, err))
	got := repo.must("path-gone")
	require.Equal(t, domain.NoteActionDelete, got.Action)
	require.Equal(t, "trashed-body", got.Content)
	require.Equal(t, before, repo.updateCount())
}

func TestAtomicWrite_InvalidPreconditionAndKillSwitch(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("switch.md", "path-switch", "keep", 1, 1))
	bad := atomicWrite(original, "nope", 2)
	bad.ExpectedVersion = nil
	_, _, err := svc.ModifyOrCreate(context.Background(), 1, bad, false)
	require.Equal(t, dto.NoteConflictInvalidPrecondition, conflictReason(t, err))
	require.Equal(t, "keep", repo.must("path-switch").Content)

	mismatch := atomicWrite(original, "body", 2)
	mismatch.ContentHash = "999"
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, mismatch, false)
	require.Equal(t, dto.NoteConflictInvalidPrecondition, conflictReason(t, err))
	require.Equal(t, "keep", repo.must("path-switch").Content)
	require.Equal(t, 0, repo.updateCount())

	t.Setenv("FNS_ATOMIC_NOTE_WRITE", "0")
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, atomicWrite(original, "blocked", 2), false)
	require.Equal(t, dto.NoteConflictAtomicDisabled, conflictReason(t, err))
	require.Equal(t, "keep", repo.must("path-switch").Content)

	t.Setenv("FNS_ATOMIC_NOTE_WRITE", "1")
	_, _, err = svc.ModifyOrCreate(context.Background(), 1, atomicWrite(original, "allowed", 2), false)
	require.NoError(t, err)
	require.Equal(t, "allowed", repo.must("path-switch").Content)
}

func TestAtomicWrite_WSStyleWriterHoldsLockThenStaleRESTRejects(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("shared.md", "path-shared", "base-body", 1, 10))
	repo.getAllGate.arm()

	legacyDone := make(chan error, 1)
	go func() {
		_, _, err := svc.ModifyOrCreate(context.Background(), 1, legacyWrite(original, "from-websocket", 30), true)
		legacyDone <- err
	}()
	select {
	case <-repo.getAllGate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("websocket-style write did not reach the locked read")
	}

	atomicDone := make(chan error, 1)
	go func() {
		_, _, err := svc.ModifyOrCreate(context.Background(), 1, atomicWrite(original, "from-rest", 40), false)
		atomicDone <- err
	}()
	close(repo.getAllGate.release)

	require.NoError(t, <-legacyDone)
	require.Equal(t, dto.NoteConflictStaleRevision, conflictReason(t, <-atomicDone))
	got := repo.must("path-shared")
	require.Equal(t, "from-websocket", got.Content)
	require.Equal(t, util.EncodeHash32("from-websocket"), got.ContentHash)
	require.Equal(t, original.Version+1, got.Version)
}

func TestAtomicWrite_PatchOutsideReadDoesNotClobber(t *testing.T) {
	repo, svc := newAtomicSvc()
	body := "---\nworkbench_round_parked: false\n---\nkeep-body-token\n"
	original := repo.seed(liveNote("park.md", "path-park", body, 2, 15))
	repo.getByGate.arm()

	ver, mt := int64(original.Version), original.Mtime
	patchDone := make(chan error, 1)
	go func() {
		_, err := svc.PatchFrontmatter(context.Background(), 1, &dto.NotePatchFrontmatterRequest{
			Vault:    "vault",
			Path:     original.Path,
			PathHash: original.PathHash,
			Updates:  map[string]interface{}{"workbench_round_parked": true},
			AtomicPrecondition: dto.AtomicPrecondition{
				Atomic:              true,
				ExpectedContentHash: original.ContentHash,
				ExpectedVersion:     &ver,
				ExpectedMtime:       &mt,
			},
		})
		patchDone <- err
	}()
	select {
	case <-repo.getByGate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("patch did not reach its pre-lock read")
	}

	_, _, err := svc.ModifyOrCreate(context.Background(), 1, legacyWrite(original, "OTHER-CLIENT-WROTE-THIS", 90), false)
	require.NoError(t, err)
	updates := repo.updateCount()
	close(repo.getByGate.release)

	require.Equal(t, dto.NoteConflictStaleRevision, conflictReason(t, <-patchDone))
	require.Equal(t, updates, repo.updateCount())
	require.Equal(t, "OTHER-CLIENT-WROTE-THIS", repo.must("path-park").Content)
}

func TestAtomicWrite_LegacyPatchStillOverwritesStaleRead(t *testing.T) {
	repo, svc := newAtomicSvc()
	body := "---\nworkbench_round_parked: false\n---\nkeep-body-token\n"
	original := repo.seed(liveNote("park-legacy.md", "path-park-legacy", body, 2, 15))
	repo.getByGate.arm()

	patchDone := make(chan error, 1)
	go func() {
		_, err := svc.PatchFrontmatter(context.Background(), 1, &dto.NotePatchFrontmatterRequest{
			Vault:    "vault",
			Path:     original.Path,
			PathHash: original.PathHash,
			Updates:  map[string]interface{}{"workbench_round_parked": true},
		})
		patchDone <- err
	}()
	select {
	case <-repo.getByGate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy patch did not reach its pre-lock read")
	}
	_, _, err := svc.ModifyOrCreate(context.Background(), 1, legacyWrite(original, "OTHER-CLIENT-WROTE-THIS", 90), false)
	require.NoError(t, err)
	close(repo.getByGate.release)
	require.NoError(t, <-patchDone)

	got := repo.must("path-park-legacy").Content
	require.NotEqual(t, "OTHER-CLIENT-WROTE-THIS", got)
	require.Contains(t, got, "keep-body-token")
	require.Contains(t, got, "workbench_round_parked")
}

func TestAtomicWrite_ReplaceAndAppendRejectStaleBase(t *testing.T) {
	repo, svc := newAtomicSvc()
	original := repo.seed(liveNote("edit.md", "path-edit", "alpha beta", 1, 5))
	_, _, err := svc.ModifyOrCreate(context.Background(), 1, legacyWrite(original, "gamma", 6), false)
	require.NoError(t, err)

	ver, mt := original.Version, original.Mtime
	_, err = svc.ReplaceContent(context.Background(), 1, &dto.NoteReplaceRequest{
		Vault:    "vault",
		Path:     original.Path,
		PathHash: original.PathHash,
		Find:     "alpha",
		Replace:  "ALPHA",
		All:      true,
		AtomicPrecondition: dto.AtomicPrecondition{
			Atomic:              true,
			ExpectedContentHash: original.ContentHash,
			ExpectedVersion:     &ver,
			ExpectedMtime:       &mt,
		},
	})
	require.Equal(t, dto.NoteConflictStaleRevision, conflictReason(t, err))
	require.Equal(t, "gamma", repo.must("path-edit").Content)

	_, err = svc.AppendContent(context.Background(), 1, &dto.NoteAppendRequest{
		Vault:    "vault",
		Path:     original.Path,
		PathHash: original.PathHash,
		Content:  "\nappended",
		AtomicPrecondition: dto.AtomicPrecondition{
			Atomic:              true,
			ExpectedContentHash: original.ContentHash,
			ExpectedVersion:     &ver,
			ExpectedMtime:       &mt,
		},
	})
	require.Equal(t, dto.NoteConflictStaleRevision, conflictReason(t, err))
	require.Equal(t, "gamma", repo.must("path-edit").Content)

	fresh := repo.must("path-edit")
	replaced, err := svc.ReplaceContent(context.Background(), 1, &dto.NoteReplaceRequest{
		Vault: "vault", Path: fresh.Path, PathHash: fresh.PathHash,
		Find: "gamma", Replace: "delta", All: true,
	})
	require.NoError(t, err)
	require.Equal(t, "delta", replaced.Note.Content)
	require.Equal(t, "delta", repo.must("path-edit").Content)
}

type onceGate struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (g *onceGate) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.entered = make(chan struct{})
	g.release = make(chan struct{})
}

func (g *onceGate) pause() {
	g.mu.Lock()
	if g == nil || !g.armed {
		g.mu.Unlock()
		return
	}
	g.armed = false
	entered := g.entered
	release := g.release
	g.mu.Unlock()
	close(entered)
	<-release
}

type memNoteRepo struct {
	mu         sync.Mutex
	rows       map[string]*domain.Note
	nextID     int64
	updates    int
	creates    int
	getAllGate onceGate
	getByGate  onceGate
}

func newMemNoteRepo() *memNoteRepo {
	return &memNoteRepo{rows: map[string]*domain.Note{}}
}

func repoKey(uid, vaultID int64, pathHash string) string {
	return fmt.Sprintf("%d/%d/%s", uid, vaultID, pathHash)
}

func (m *memNoteRepo) seed(n *domain.Note) *domain.Note {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n.ID == 0 {
		m.nextID++
		n.ID = m.nextID
	}
	if n.VaultID == 0 {
		n.VaultID = 1
	}
	cp := *n
	m.rows[repoKey(1, cp.VaultID, cp.PathHash)] = &cp
	// Return a detached copy so a later in-place store update cannot change
	// the revision the test already observed.
	out := cp
	return &out
}

func (m *memNoteRepo) must(pathHash string) domain.Note {
	n, ok := m.get(pathHash)
	if !ok {
		panic("missing " + pathHash)
	}
	return n
}

func (m *memNoteRepo) get(pathHash string) (domain.Note, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.rows[repoKey(1, 1, pathHash)]
	if n == nil {
		return domain.Note{}, false
	}
	return *n, true
}

func (m *memNoteRepo) updateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updates
}

func (m *memNoteRepo) createCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creates
}

func (m *memNoteRepo) GetByPathHash(_ context.Context, pathHash string, vaultID, uid int64) (*domain.Note, error) {
	m.mu.Lock()
	n := m.rows[repoKey(uid, vaultID, pathHash)]
	var cp *domain.Note
	if n != nil && n.Action != domain.NoteActionDelete {
		cloned := *n
		cp = &cloned
	}
	m.mu.Unlock()
	m.getByGate.pause()
	if cp == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return cp, nil
}

func (m *memNoteRepo) GetAllByPathHash(_ context.Context, pathHash string, vaultID, uid int64) (*domain.Note, error) {
	m.mu.Lock()
	n := m.rows[repoKey(uid, vaultID, pathHash)]
	var cp *domain.Note
	if n != nil {
		cloned := *n
		cp = &cloned
	}
	m.mu.Unlock()
	m.getAllGate.pause()
	return cp, nil
}

func (m *memNoteRepo) Create(_ context.Context, note *domain.Note, uid int64) (*domain.Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates++
	m.nextID++
	cp := *note
	cp.ID = m.nextID
	m.rows[repoKey(uid, cp.VaultID, cp.PathHash)] = &cp
	out := cp
	return &out, nil
}

func (m *memNoteRepo) Update(_ context.Context, note *domain.Note, uid int64) (*domain.Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates++
	cp := *note
	m.rows[repoKey(uid, cp.VaultID, cp.PathHash)] = &cp
	out := cp
	return &out, nil
}

func (m *memNoteRepo) UpdateActionMtime(_ context.Context, action domain.NoteAction, mtime int64, id, uid int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.rows {
		if n.ID == id {
			n.Action = action
			n.Mtime = mtime
			return nil
		}
	}
	return gorm.ErrRecordNotFound
}

func (m *memNoteRepo) CountSizeSum(context.Context, int64, int64) (*domain.CountSizeResult, error) {
	return &domain.CountSizeResult{}, nil
}

func (m *memNoteRepo) GetByID(context.Context, int64, int64) (*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) GetByPathHashIncludeRecycle(context.Context, string, int64, int64, bool) (*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByPathHash(context.Context, string, int64, int64) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByPathHashesMeta(context.Context, []string, int64, int64) (map[string]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) GetByPath(context.Context, string, int64, int64) (*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) UpdateDelete(context.Context, *domain.Note, int64) error { panic("unused") }
func (m *memNoteRepo) UpdateMtime(context.Context, int64, int64, int64) error  { panic("unused") }
func (m *memNoteRepo) UpdateFID(context.Context, int64, int64, int64) error    { panic("unused") }
func (m *memNoteRepo) UpdateSnapshot(context.Context, string, string, int64, int64, int64) error {
	panic("unused")
}
func (m *memNoteRepo) Delete(context.Context, int64, int64, int64) error { panic("unused") }
func (m *memNoteRepo) DeletePhysicalByTime(context.Context, int64, int64) error {
	panic("unused")
}
func (m *memNoteRepo) DeletePhysicalByTimeAll(context.Context, int64) error { panic("unused") }
func (m *memNoteRepo) List(context.Context, int64, int, int, int64, string, bool, string, bool, string, string, []string) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListCount(context.Context, int64, int64, string, bool, string, bool, []string) (int64, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByUpdatedTimestamp(context.Context, int64, int64, int64) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByUpdatedTimestampPage(context.Context, int64, int64, int64, int, int) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByUpdatedTimestampMeta(context.Context, int64, int64, int64) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByUpdatedTimestampPageMeta(context.Context, int64, int64, int64, int, int) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListContentUnchanged(context.Context, int64) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByFID(context.Context, int64, int64, int64, int, int, string, string) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByFIDCount(context.Context, int64, int64, int64) (int64, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByFIDs(context.Context, []int64, int64, int64, int, int, string, string) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByFIDsCount(context.Context, []int64, int64, int64) (int64, error) {
	panic("unused")
}
func (m *memNoteRepo) CountByFIDs(context.Context, []int64, int64, int64) (map[int64]int64, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByIDs(context.Context, []int64, int64) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) ListByPathPrefix(context.Context, string, int64, int64) ([]*domain.Note, error) {
	panic("unused")
}
func (m *memNoteRepo) RecycleClear(context.Context, string, string, int64, int64) error {
	panic("unused")
}
func (m *memNoteRepo) DeleteByVaultID(context.Context, int64, int64) error { panic("unused") }
func (m *memNoteRepo) RebuildVaultIndex(context.Context, int64, int64) error {
	panic("unused")
}

type nopVault struct{}

func (nopVault) GetByName(context.Context, int64, string) (*domain.Vault, error) {
	panic("unused")
}
func (nopVault) GetOrCreate(context.Context, int64, string) (*domain.Vault, error) {
	panic("unused")
}
func (nopVault) MustGetID(context.Context, int64, string) (int64, error) { return 1, nil }
func (nopVault) Create(context.Context, int64, string) (*dto.VaultDTO, error) {
	panic("unused")
}
func (nopVault) Update(context.Context, int64, int64, string) (*dto.VaultDTO, error) {
	panic("unused")
}
func (nopVault) Get(context.Context, int64, int64) (*dto.VaultDTO, error) { panic("unused") }
func (nopVault) List(context.Context, int64) ([]*dto.VaultDTO, error)     { panic("unused") }
func (nopVault) Delete(context.Context, int64, int64) error               { panic("unused") }
func (nopVault) UpdateNoteStats(context.Context, int64, int64, int64, int64) error {
	return nil
}
func (nopVault) UpdateFileStats(context.Context, int64, int64, int64, int64) error {
	panic("unused")
}
func (nopVault) RebuildIndex(context.Context, int64, int64) error { panic("unused") }
func (nopVault) ForceDeleteDataItem(context.Context, int64, int64, string, int64, string, string, string) error {
	panic("unused")
}

type nopFolder struct{}

func (nopFolder) Get(context.Context, int64, *dto.FolderGetRequest) (*dto.FolderDTO, error) {
	panic("unused")
}
func (nopFolder) List(context.Context, int64, *dto.FolderListRequest) ([]*dto.FolderDTO, error) {
	panic("unused")
}
func (nopFolder) ListByUpdatedTimestamp(context.Context, int64, string, int64) ([]*dto.FolderDTO, error) {
	panic("unused")
}
func (nopFolder) UpdateOrCreate(context.Context, int64, *dto.FolderCreateRequest) (*dto.FolderDTO, error) {
	panic("unused")
}
func (nopFolder) Delete(context.Context, int64, *dto.FolderDeleteRequest) (*dto.FolderDTO, error) {
	panic("unused")
}
func (nopFolder) DeleteTree(context.Context, int64, *dto.FolderDeleteRequest) (*dto.FolderDTO, error) {
	panic("unused")
}
func (nopFolder) Rename(context.Context, int64, *dto.FolderRenameRequest) (*dto.FolderDTO, *dto.FolderDTO, error) {
	panic("unused")
}
func (nopFolder) ListNotes(context.Context, int64, *dto.FolderContentRequest, *pkgapp.Pager) ([]*dto.NoteNoContentDTO, int, error) {
	panic("unused")
}
func (nopFolder) ListFiles(context.Context, int64, *dto.FolderContentRequest, *pkgapp.Pager) ([]*dto.FileDTO, int, error) {
	panic("unused")
}
func (nopFolder) EnsurePathFID(context.Context, int64, int64, string) (int64, error) {
	panic("unused")
}
func (nopFolder) CleanupEmptyAncestors(context.Context, int64, int64, string) error {
	panic("unused")
}
func (nopFolder) SyncResourceFID(context.Context, int64, int64, []int64, []int64) error {
	return nil
}
func (nopFolder) GetTree(context.Context, int64, *dto.FolderTreeRequest) (*dto.FolderTreeResponse, error) {
	panic("unused")
}
func (nopFolder) CleanDuplicateFolders(context.Context, int64, int64) error { panic("unused") }
func (nopFolder) WithClient(string, string, string) service.FolderService   { return nopFolder{} }

func newAtomicSvc() (*memNoteRepo, service.NoteService) {
	repo := newMemNoteRepo()
	svc := service.NewNoteService(nil, repo, nil, nil, nil, nil, nopVault{}, nopFolder{}, nil, nil, nil, nil)
	return repo, svc.WithClient("rest", "workbench", "test")
}

func liveNote(path, pathHash, content string, version, mtime int64) *domain.Note {
	return &domain.Note{
		VaultID:     1,
		Action:      domain.NoteActionModify,
		Path:        path,
		PathHash:    pathHash,
		Content:     content,
		ContentHash: util.EncodeHash32(content),
		Version:     version,
		Ctime:       111,
		Mtime:       mtime,
		ClientName:  "obsidian",
	}
}

func atomicWrite(base *domain.Note, content string, mtime int64) *dto.NoteModifyOrCreateRequest {
	ver, mt := base.Version, base.Mtime
	return &dto.NoteModifyOrCreateRequest{
		Vault:       "vault",
		Path:        base.Path,
		PathHash:    base.PathHash,
		Content:     content,
		ContentHash: util.EncodeHash32(content),
		Ctime:       base.Ctime,
		Mtime:       mtime,
		AtomicPrecondition: dto.AtomicPrecondition{
			Atomic:              true,
			ExpectedContentHash: base.ContentHash,
			ExpectedVersion:     &ver,
			ExpectedMtime:       &mt,
		},
	}
}

func legacyWrite(base *domain.Note, content string, mtime int64) *dto.NoteModifyOrCreateRequest {
	return &dto.NoteModifyOrCreateRequest{
		Vault:       "vault",
		Path:        base.Path,
		PathHash:    base.PathHash,
		Content:     content,
		ContentHash: util.EncodeHash32(content),
		Ctime:       base.Ctime,
		Mtime:       mtime,
	}
}

func conflictReason(t *testing.T, err error) string {
	t.Helper()
	reason, ok := revisionReason(err)
	require.True(t, ok, "err=%v", err)
	return reason
}

func revisionReason(err error) (string, bool) {
	var c *code.Code
	if err == nil || !errors.As(err, &c) || !c.Is(code.ErrorNoteRevisionConflict) || c.Code() != 531 {
		return "", false
	}
	payload, ok := c.Data().(*dto.NoteRevisionConflict)
	if !ok || payload == nil || !payload.Conflict {
		return "", false
	}
	return payload.Reason, true
}
