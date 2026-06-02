package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAddSynctechSMSDriveWritesConfigWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cmd := newTestRootCmd()
	cmd.AddCommand(newAddSynctechSMSDriveCmd())
	cmd.SetArgs([]string{
		"add-synctech-sms-drive", "pixel",
		"--owner-phone", "+15550000001",
		"--folder-id", "drive-folder-id",
		"--google-account", "user@example.com",
		"--schedule", "30 4 * * *",
		"--oauth-app", "personal",
		"--skip-auth-for-test",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(data)
	for _, want := range []string{`[[synctech_sms.sources]]`, `name = "pixel"`, `backend = "drive"`, `folder_id = "drive-folder-id"`, `google_account = "user@example.com"`, `owner_phone = "+15550000001"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	lower := strings.ToLower(text)
	refreshTokenKey := "refresh" + "_token"
	clientSecretKey := "client" + "_secret\""
	if strings.Contains(lower, refreshTokenKey) || strings.Contains(lower, clientSecretKey) {
		t.Fatalf("config contains secret material:\n%s", text)
	}
}

func TestSynctechSMSDriveNoNewFilesCompletesSyncAndTouchesSource(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	client := &testDriveClient{}

	if err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client, fixedNow); err != nil {
		t.Fatalf("runSynctechSMSDriveSourceWithClient: %v", err)
	}

	source := mustSynctechSource(t, f.Store, src.OwnerPhone)
	assertSyncRunCounts(t, f.Store, source.ID, 1, 1, 0)
	if !source.LastSyncAt.Valid {
		t.Fatal("source last_sync_at is NULL, want touched after no-new poll")
	}
	if client.downloads != 0 {
		t.Fatalf("downloads = %d, want 0", client.downloads)
	}
}

func TestRunConfiguredSynctechSMSSourceWithStoreUsesProvidedStore(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	client := &testDriveClient{}
	oldClientFactory := newSynctechSMSDriveClient
	oldRebuildCacheAfterSync := rebuildCacheAfterSync
	defer func() {
		newSynctechSMSDriveClient = oldClientFactory
		rebuildCacheAfterSync = oldRebuildCacheAfterSync
	}()
	newSynctechSMSDriveClient = func(context.Context, config.SynctechSMSSource) (synctechsms.DriveClient, error) {
		return client, nil
	}
	var rebuilds int
	rebuildCacheAfterSync = func(string, string) {
		rebuilds++
	}

	if err := runConfiguredSynctechSMSSourceWithStore(context.Background(), f.Store, src); err != nil {
		t.Fatalf("runConfiguredSynctechSMSSourceWithStore: %v", err)
	}

	source := mustSynctechSource(t, f.Store, src.OwnerPhone)
	assertSyncRunCounts(t, f.Store, source.ID, 1, 1, 0)
	if !source.LastSyncAt.Valid {
		t.Fatal("source last_sync_at is NULL, want touched after configured poll")
	}
	if _, err := f.Store.ListSources(""); err != nil {
		t.Fatalf("provided store is not usable after configured sync: %v", err)
	}
	if rebuilds != 1 {
		t.Fatalf("cache rebuild hook calls = %d, want 1", rebuilds)
	}
}

func TestRunConfiguredSynctechSMSLocalPropagatesImportFailure(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	src.Backend = "local"
	src.Path = filepath.Join(t.TempDir(), "missing.xml")
	oldRebuildCacheAfterSync := rebuildCacheAfterSync
	defer func() {
		rebuildCacheAfterSync = oldRebuildCacheAfterSync
	}()
	var rebuilds int
	rebuildCacheAfterSync = func(string, string) {
		rebuilds++
	}

	err := runConfiguredSynctechSMSSourceWithStore(context.Background(), f.Store, src)
	if err == nil {
		t.Fatal("runConfiguredSynctechSMSSourceWithStore error = nil, want local import failure")
	}
	if rebuilds != 0 {
		t.Fatalf("cache rebuild hook calls = %d, want 0 after failed local import", rebuilds)
	}
	source := mustSynctechSource(t, f.Store, src.OwnerPhone)
	if source.LastSyncAt.Valid {
		t.Fatalf("source last_sync_at = %v, want NULL after failed local import", source.LastSyncAt.Time)
	}
}

func TestSynctechSMSDriveImportsOneFileWithSingleOuterSync(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	client := &testDriveClient{
		files: []synctechsms.DriveFile{stableDriveFile("drive-file-1", "backup.xml", "sum-1")},
		contents: map[string]string{
			"drive-file-1": `<smses count="1"><sms address="+15551234567" date="1717214400000" type="1" body="hello" read="1" status="-1"/></smses>`,
		},
	}

	if err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client, fixedNow); err != nil {
		t.Fatalf("runSynctechSMSDriveSourceWithClient: %v", err)
	}

	source := mustSynctechSource(t, f.Store, src.OwnerPhone)
	assertSyncRunCounts(t, f.Store, source.ID, 1, 1, 0)
	assertMessageCount(t, f.Store, 1)
	item := mustSourceImportItem(t, f.Store, source.ID, "drive-file-1")
	if item.Status != "imported" || item.RecordsImported != 1 || !item.ImportedAt.Valid {
		t.Fatalf("import item = %#v, want imported with one record", item)
	}
}

func TestSynctechSMSDriveSkipsImportedChecksum(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	source, err := ensureConfiguredSynctechSMSSource(f.Store, src, synctechImportOptions(src))
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := f.Store.UpsertSourceImportItem(store.SourceImportItem{
		SourceID:   source.ID,
		Provider:   "drive",
		ProviderID: "drive-file-1",
		Name:       "backup.xml",
		Checksum:   "sum-1",
		Status:     "imported",
	}); err != nil {
		t.Fatalf("seed import item: %v", err)
	}
	client := &testDriveClient{
		files: []synctechsms.DriveFile{stableDriveFile("drive-file-1", "backup.xml", "sum-1")},
	}

	if err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client, fixedNow); err != nil {
		t.Fatalf("runSynctechSMSDriveSourceWithClient: %v", err)
	}

	source = mustSynctechSource(t, f.Store, src.OwnerPhone)
	assertSyncRunCounts(t, f.Store, source.ID, 1, 1, 0)
	if client.downloads != 0 {
		t.Fatalf("downloads = %d, want 0 for unchanged checksum", client.downloads)
	}
}

func TestSynctechSMSDriveDownloadFailureMarksItemAndSyncFailed(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	downloadErr := errors.New("download unavailable")
	client := &testDriveClient{
		files:       []synctechsms.DriveFile{stableDriveFile("drive-file-1", "backup.xml", "sum-1")},
		downloadErr: downloadErr,
	}

	err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client, fixedNow)
	if !errors.Is(err, downloadErr) {
		t.Fatalf("error = %v, want downloadErr", err)
	}

	source := mustSynctechSource(t, f.Store, src.OwnerPhone)
	assertSyncRunCounts(t, f.Store, source.ID, 1, 0, 1)
	item := mustSourceImportItem(t, f.Store, source.ID, "drive-file-1")
	if item.Status != "failed" || !item.ErrorMessage.Valid {
		t.Fatalf("import item = %#v, want failed with error", item)
	}
}

func TestSynctechSMSDriveImportFailureMarksItemAndSyncFailed(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	client := &testDriveClient{
		files: []synctechsms.DriveFile{stableDriveFile("drive-file-1", "backup.xml", "sum-1")},
		contents: map[string]string{
			"drive-file-1": `<smses count="1"><sms address="+15551234567"`,
		},
	}

	err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client, fixedNow)
	if err == nil {
		t.Fatal("runSynctechSMSDriveSourceWithClient error = nil, want import failure")
	}

	source := mustSynctechSource(t, f.Store, src.OwnerPhone)
	assertSyncRunCounts(t, f.Store, source.ID, 1, 0, 1)
	item := mustSourceImportItem(t, f.Store, source.ID, "drive-file-1")
	if item.Status != "failed" || !item.ErrorMessage.Valid {
		t.Fatalf("import item = %#v, want failed with error", item)
	}
}

func TestSynctechSMSDriveReimportsChangedChecksum(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	source, err := ensureConfiguredSynctechSMSSource(f.Store, src, synctechImportOptions(src))
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := f.Store.UpsertSourceImportItem(store.SourceImportItem{
		SourceID:   source.ID,
		Provider:   "drive",
		ProviderID: "drive-file-1",
		Name:       "backup.xml",
		Checksum:   "old-sum",
		Status:     "imported",
	}); err != nil {
		t.Fatalf("seed import item: %v", err)
	}
	client := &testDriveClient{
		files: []synctechsms.DriveFile{stableDriveFile("drive-file-1", "backup.xml", "new-sum")},
		contents: map[string]string{
			"drive-file-1": `<smses count="1"><sms address="+15551234567" date="1717214400000" type="1" body="hello" read="1" status="-1"/></smses>`,
		},
	}

	if err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client, fixedNow); err != nil {
		t.Fatalf("runSynctechSMSDriveSourceWithClient: %v", err)
	}

	if client.downloads != 1 {
		t.Fatalf("downloads = %d, want 1 for changed checksum", client.downloads)
	}
	item := mustSourceImportItem(t, f.Store, source.ID, "drive-file-1")
	if item.Checksum != "new-sum" || item.Status != "imported" {
		t.Fatalf("import item = %#v, want updated checksum and imported", item)
	}
}

func TestSynctechSMSDriveListImportedChecksumsAllowsNullChecksum(t *testing.T) {
	f := newSynctechDriveTestFixture(t)
	src := testSynctechDriveSource()
	source, err := ensureConfiguredSynctechSMSSource(f.Store, src, synctechImportOptions(src))
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	_, err = f.Store.DB().Exec(`
		INSERT INTO source_import_items (source_id, provider, provider_id, name, checksum, status)
		VALUES (?, 'drive', 'drive-file-1', 'backup.xml', NULL, 'imported')
	`, source.ID)
	if err != nil {
		t.Fatalf("seed NULL checksum import item: %v", err)
	}

	checksums, err := f.Store.ListImportedSourceItemChecksums(source.ID, "drive")
	if err != nil {
		t.Fatalf("ListImportedSourceItemChecksums: %v", err)
	}
	if got, ok := checksums["drive-file-1"]; !ok || got != "" {
		t.Fatalf("checksums = %#v, want empty checksum entry", checksums)
	}
	item := mustSourceImportItem(t, f.Store, source.ID, "drive-file-1")
	if item.Checksum != "" {
		t.Fatalf("GetSourceImportItem checksum = %q, want empty string for NULL", item.Checksum)
	}
}

type synctechDriveTestFixture struct {
	*storetest.Fixture
}

func newSynctechDriveTestFixture(t *testing.T) synctechDriveTestFixture {
	t.Helper()
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	return synctechDriveTestFixture{Fixture: storetest.New(t)}
}

func testSynctechDriveSource() config.SynctechSMSSource {
	return config.SynctechSMSSource{
		Name:               "pixel",
		Enabled:            true,
		Backend:            "drive",
		FolderID:           "drive-folder-id",
		GoogleAccount:      "user@example.com",
		OwnerPhone:         "+15550000001",
		StableAfter:        "10m",
		IncludeSMS:         true,
		IncludeMMS:         true,
		IncludeCalls:       true,
		IncludeAttachments: true,
	}
}

func stableDriveFile(id, name, checksum string) synctechsms.DriveFile {
	return synctechsms.DriveFile{
		ID:           id,
		Name:         name,
		Size:         100,
		Checksum:     checksum,
		ModifiedTime: fixedNow().Add(-time.Hour),
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 5, 25, 4, 30, 0, 0, time.UTC)
}

type testDriveClient struct {
	files       []synctechsms.DriveFile
	listErr     error
	downloadErr error
	contents    map[string]string
	downloads   int
}

func (c *testDriveClient) ListBackupFiles(context.Context, string) ([]synctechsms.DriveFile, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.files, nil
}

func (c *testDriveClient) DownloadToFile(_ context.Context, fileID, path string) error {
	c.downloads++
	if c.downloadErr != nil {
		return c.downloadErr
	}
	content := c.contents[fileID]
	if content == "" {
		content = `<smses count="0"></smses>`
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func mustSynctechSource(t *testing.T, st *store.Store, ownerPhone string) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSource(synctechsms.SourceType, ownerPhone)
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
	return source
}

func mustSourceImportItem(t *testing.T, st *store.Store, sourceID int64, providerID string) *store.SourceImportItem {
	t.Helper()
	item, err := st.GetSourceImportItem(sourceID, "drive", providerID)
	if err != nil {
		t.Fatalf("GetSourceImportItem: %v", err)
	}
	if item == nil {
		t.Fatalf("source import item %q not found", providerID)
	}
	return item
}

func assertSyncRunCounts(t *testing.T, st *store.Store, sourceID int64, total, completed, failed int) {
	t.Helper()
	var gotTotal, gotCompleted, gotFailed int
	err := st.DB().QueryRow(`
		SELECT COUNT(*),
		       SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END)
		FROM sync_runs
		WHERE source_id = ?
	`, sourceID).Scan(&gotTotal, &gotCompleted, &gotFailed)
	if err != nil {
		t.Fatalf("count sync runs: %v", err)
	}
	if gotTotal != total || gotCompleted != completed || gotFailed != failed {
		t.Fatalf("sync counts total/completed/failed = %d/%d/%d, want %d/%d/%d",
			gotTotal, gotCompleted, gotFailed, total, completed, failed)
	}
}

func assertMessageCount(t *testing.T, st *store.Store, want int) {
	t.Helper()
	var got int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&got); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
}
