package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const whatsappIdentityTestPhone = "+15555550100"

func TestImportWhatsAppReportsPartialWithoutConfirmingIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, true, false)
	output, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.ErrorContains(err, "WhatsApp import incomplete")
	assert.NotContains(output, "Confirmed identity")
	assert.Contains(output, "Messages:       3 processed, 1 added, 0 updated, 2 skipped")
	assert.Contains(output, "Errors:         1")

	identities := whatsappIdentityTestIdentities(t, home)
	assert.Empty(identities)
	receipt := decodeWhatsAppImportReceipt(t, output)
	assert.Equal("partial", receipt.Status)
	assert.Equal(int64(1), receipt.Errors)
	assert.Equal(int64(3), receipt.Processed)
	assert.Equal(int64(1), receipt.Upserted)
	assert.Equal(int64(2), receipt.Skipped)
	assert.Equal(map[string]int64{"duplicate_stanza": 2}, receipt.SkippedCounts)
	assert.Equal(map[string]int64{"duplicate_stanza": 1}, receipt.ErrorCounts)
	run := whatsappIdentityTestLatestSync(t, home)
	assert.Equal("failed", run.Status)
	assert.Equal(receipt.Errors, run.ErrorsCount)
	assert.Equal(receipt.Processed, run.MessagesProcessed)
	assert.Equal(receipt.Added, run.MessagesAdded)
}

func TestImportWhatsAppConfirmsDefaultIdentityOnCleanImport(t *testing.T) {
	assert := assert.New(t)
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, false, false)
	output, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.NoError(t, err)
	assert.Len(whatsappIdentityTestIdentities(t, home), 1)
	receipt := decodeWhatsAppImportReceipt(t, output)
	assert.Equal("complete", receipt.Status)
	assert.Zero(receipt.Errors)
	assert.EqualValues(1, receipt.Added)
}

func TestImportWhatsAppDoesNotConfirmIdentityWithNoDefaultIdentity(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, true, false)
	_, err := runWhatsAppIdentityCommand(t, home, fixture, true)

	require.ErrorContains(t, err, "WhatsApp import incomplete")
	assert.Empty(t, whatsappIdentityTestIdentities(t, home))
	assert.Len(t, whatsappIdentityTestSources(t, home), 1)
}

func TestImportWhatsAppDoesNotConfirmIdentityOnHardError(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, false, true)
	_, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.ErrorContains(t, err, "import failed")
	assert.Empty(t, whatsappIdentityTestIdentities(t, home))
	assert.Len(t, whatsappIdentityTestSources(t, home), 1)
}

func TestImportWhatsAppDoesNotCreateSourceForUnknownDatabase(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppUnknownDatabaseFixture(t)
	_, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.ErrorContains(t, err, "import failed")
	assert.Empty(t, whatsappIdentityTestSources(t, home))
	assert.Empty(t, whatsappIdentityTestIdentities(t, home))
}

func TestImportWhatsAppIdentityIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, false, false)

	_, err := runWhatsAppIdentityCommand(t, home, fixture, false)
	require.NoError(err)
	output, err := runWhatsAppIdentityCommand(t, home, fixture, false)
	require.NoError(err)

	identities := whatsappIdentityTestIdentities(t, home)
	require.Len(identities, 1)
	assert.Equal(whatsappIdentityTestPhone, identities[0].Address)
	receipt := decodeWhatsAppImportReceipt(t, output)
	assert.Equal("complete", receipt.Status)
	assert.Zero(receipt.Added)
	assert.EqualValues(1, receipt.Skipped)
	assert.Equal(map[string]int64{"unchanged": 1}, receipt.SkippedCounts)
}

func TestImportWhatsAppAppleNormalSkipsRemainComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, false, false)
	db, err := sql.Open("sqlite3", fixture)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO ZWAMESSAGE VALUES
		(2, 1, NULL, 'image-message', 0, 700000001, 'caption', 1, '15555550101@s.whatsapp.net'),
		(3, 1, NULL, 'unsupported-message', 0, 700000002, 'text', 8, '15555550101@s.whatsapp.net'),
		(4, 1, NULL, 'empty-message', 0, 700000003, '   ', 0, '15555550101@s.whatsapp.net'),
		(5, 1, NULL, '   ', 0, 700000004, 'text', 7, '15555550101@s.whatsapp.net')`)
	require.NoError(err)
	require.NoError(db.Close())
	for attempt := range 2 {
		output, err := runWhatsAppIdentityCommand(t, home, fixture, false)
		require.NoError(err)
		receipt := decodeWhatsAppImportReceipt(t, output)
		assert.Equal("complete", receipt.Status)
		assert.Zero(receipt.Errors)
		assert.Equal(int64(5), receipt.Processed)
		assert.Equal(int64(4+attempt), receipt.Skipped)
		expected := map[string]int64{"type_1": 1, "type_8": 1, "empty_text": 1, "missing_key": 1}
		if attempt > 0 {
			expected["unchanged"] = 1
		}
		assert.Equal(expected, receipt.SkippedCounts)
		run := whatsappIdentityTestLatestSync(t, home)
		assert.Equal("completed", run.Status)
		assert.Equal(receipt.Errors, run.ErrorsCount)
		assert.Equal(receipt.Processed, run.MessagesProcessed)
		assert.Equal(receipt.Added, run.MessagesAdded)
		assert.Equal(receipt.Updated, run.MessagesUpdated)
	}
}

type whatsAppImportReceipt struct {
	Version       int              `json:"version"`
	Status        string           `json:"status"`
	Errors        int64            `json:"errors"`
	Processed     int64            `json:"messages_processed"`
	Upserted      int64            `json:"messages_upserted"`
	Added         int64            `json:"messages_added"`
	Updated       int64            `json:"messages_updated"`
	Skipped       int64            `json:"messages_skipped"`
	SkippedCounts map[string]int64 `json:"skipped_counts"`
	ErrorCounts   map[string]int64 `json:"error_counts"`
}

func decodeWhatsAppImportReceipt(t *testing.T, output string) whatsAppImportReceipt {
	t.Helper()
	require.Equal(t, 1, strings.Count(output, "WHATSAPP_IMPORT_RESULT="))
	_, encoded, found := strings.Cut(output, "WHATSAPP_IMPORT_RESULT=")
	require.True(t, found)
	encoded, _, _ = strings.Cut(encoded, "\n")
	var result whatsAppImportReceipt
	require.NoError(t, json.Unmarshal([]byte(encoded), &result))
	assert.Equal(t, 1, result.Version)
	assert.Equal(t, result.Added+result.Updated, result.Upserted)
	var skipped int64
	for _, count := range result.SkippedCounts {
		skipped += count
	}
	assert.Equal(t, result.Skipped, skipped, "receipt skip categories must account for every skipped message")
	return result
}

func whatsappIdentityTestLatestSync(t *testing.T, home string) *store.SyncRun {
	t.Helper()
	require := require.New(t)
	db, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = db.Close() }()
	sources, err := db.GetSourcesByIdentifier(whatsappIdentityTestPhone)
	require.NoError(err)
	require.Len(sources, 1)
	run, err := db.GetLatestSync(sources[0].ID)
	require.NoError(err)
	return run
}

func runWhatsAppIdentityCommand(
	t *testing.T,
	home, fixture string,
	noDefaultIdentity bool,
) (string, error) {
	t.Helper()
	return runWhatsAppIdentityCommandContext(t, func(ctx context.Context) context.Context { return ctx }, home, fixture, noDefaultIdentity)
}

// runWhatsAppIdentityCommandContext runs the import with the context that
// wrap derives from the test's invocation context.
func runWhatsAppIdentityCommandContext(
	t *testing.T,
	wrap func(context.Context) context.Context,
	home, fixture string,
	noDefaultIdentity bool,
) (string, error) {
	t.Helper()
	cfg := testConfigValue()

	oldCfg := cfg
	oldNoDefaultIdentity := noDefaultIdentityImportWhatsApp
	oldImportPhone := importPhone
	oldImportMediaDir := importMediaDir
	oldImportContacts := importContacts
	oldImportLimit := importLimit
	oldImportDisplayName := importDisplayName
	t.Cleanup(func() {
		cfg = oldCfg
		noDefaultIdentityImportWhatsApp = oldNoDefaultIdentity
		importPhone = oldImportPhone
		importMediaDir = oldImportMediaDir
		importContacts = oldImportContacts
		importLimit = oldImportLimit
		importDisplayName = oldImportDisplayName
	})

	testCtx := withStoreResolverConfig(t, &config.Config{
		HomeDir: home,
		Data: config.DataConfig{
			DataDir: home,
		},
		Analytics: config.AnalyticsConfig{
			Engine:         config.AnalyticsEngineAuto,
			AutoBuildCache: true,
		},
	})
	noDefaultIdentityImportWhatsApp = noDefaultIdentity
	importPhone = whatsappIdentityTestPhone
	importMediaDir = ""
	importContacts = ""
	importLimit = 0
	importDisplayName = ""

	var output strings.Builder
	cmd := &cobra.Command{}
	cmd.SetContext(wrap(testCtx))
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	oldStdout := os.Stdout
	stdoutReader, stdoutWriter, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = stdoutWriter
	defer func() {
		os.Stdout = oldStdout
		_ = stdoutWriter.Close()
		_ = stdoutReader.Close()
	}()
	runErr := runWhatsAppImport(cmd, fixture)
	require.NoError(t, stdoutWriter.Close())
	os.Stdout = oldStdout
	var printed bytes.Buffer
	_, copyErr := io.Copy(&printed, stdoutReader)
	require.NoError(t, copyErr)
	require.NoError(t, stdoutReader.Close())
	return output.String() + printed.String(), runErr
}

func createWhatsAppIdentityFixture(t *testing.T, recoverable, hardError bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE ZWACHATSESSION (
			Z_PK INTEGER PRIMARY KEY,
			ZCONTACTJID TEXT,
			ZPARTNERNAME TEXT,
			ZSESSIONTYPE INTEGER,
			ZLASTMESSAGEDATE TIMESTAMP
		);
		CREATE TABLE ZWAMESSAGE (
			Z_PK INTEGER PRIMARY KEY,
			ZCHATSESSION INTEGER,
			ZGROUPMEMBER INTEGER,
			ZSTANZAID TEXT,
			ZISFROMME INTEGER,
			ZMESSAGEDATE TIMESTAMP,
			ZTEXT TEXT,
			ZMESSAGETYPE INTEGER,
			ZFROMJID TEXT
		);
		INSERT INTO ZWACHATSESSION VALUES
			(1, '15555550101@s.whatsapp.net', 'Test Contact', 0, 700000003);
		INSERT INTO ZWAMESSAGE VALUES
			(1, 1, NULL, 'unique-message', 0, 700000000, 'imported text', 0,
			 '15555550101@s.whatsapp.net');
	`)
	require.NoError(t, err)
	if !hardError {
		_, err = db.ExecContext(t.Context(), `
			CREATE TABLE ZWAGROUPMEMBER (
				Z_PK INTEGER PRIMARY KEY,
				ZCHATSESSION INTEGER,
				ZMEMBERJID TEXT,
				ZCONTACTNAME TEXT,
				ZFIRSTNAME TEXT,
				ZISADMIN INTEGER
			);
		`)
		require.NoError(t, err)
		if recoverable {
			_, err = db.ExecContext(t.Context(), `
				INSERT INTO ZWAMESSAGE VALUES
					(2, 1, NULL, 'duplicate-message', 0, 700000001, 'duplicate one', 0,
					 '15555550101@s.whatsapp.net'),
					(3, 1, NULL, 'duplicate-message', 0, 700000002, 'duplicate two', 0,
					 '15555550101@s.whatsapp.net');
			`)
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Close())
	return path
}

func createWhatsAppUnknownDatabaseFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unknown.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "CREATE TABLE unrelated (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

func whatsappIdentityTestSources(t *testing.T, home string) []*store.Source {
	t.Helper()
	db, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	sources, err := db.GetSourcesByIdentifier(whatsappIdentityTestPhone)
	require.NoError(t, err)
	return sources
}

func whatsappIdentityTestIdentities(t *testing.T, home string) []store.AccountIdentity {
	t.Helper()
	db, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	sources, err := db.GetSourcesByIdentifier(whatsappIdentityTestPhone)
	require.NoError(t, err)
	if len(sources) == 0 {
		return nil
	}
	identities, err := db.ListAccountIdentities(sources[0].ID)
	require.NoError(t, err)
	return identities
}
