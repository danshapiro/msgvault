package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestScheduledGmailReportsUnresolvedMessagesAndStillRebuilds(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	// Only provider responses are simulated. Scheduled callback, retained MIME
	// replay and archive writes use their production implementations.
	savedTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = savedTransport })
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.Method + " " + req.URL.Path {
		case "POST /oauth2":
			body = `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`
		case "GET /gmail/v1/users/me/profile":
			body = `{"emailAddress":"reader@example.test","historyId":"1000"}`
		case "GET /gmail/v1/users/me/labels":
			body = `{"labels":[]}`
		default:
			return nil, fmt.Errorf("unexpected synthetic provider request: %s %s", req.Method, req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	home := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(err)
	keyJSON, err := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "service@example.test",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		"token_uri":   "https://token.example.test/oauth2",
	})
	require.NoError(err)
	keyPath := filepath.Join(home, "service-account.json")
	require.NoError(os.WriteFile(keyPath, keyJSON, 0600))
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home},
		OAuth: config.OAuthConfig{ServiceAccountKey: keyPath}}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	require.NoError(os.WriteFile(cfg.AttachmentsDir(), []byte("not a directory"), 0600))
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: "reader@example.test", HistoryID: 1000}
	mock.AddMessage("retained-file", testemail.NewMessage().WithAttachment("saved.txt", "text/plain", []byte("retained bytes")).Bytes(), nil)
	summary, err := msgsync.New(mock, st, &msgsync.Options{AttachmentsDir: cfg.AttachmentsDir()}).Full(t.Context(), mock.Profile.EmailAddress)
	require.NoError(err)
	require.Equal(int64(1), summary.UnresolvedMessages)
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	state := invocationFromContext(ctx)
	var logs bytes.Buffer
	state.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	originalRebuild := rebuildCacheAfterScheduledSourceRun
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = originalRebuild })
	rebuilds := 0
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { rebuilds++; return nil }

	err = runScheduledSync(ctx, mock.Profile.EmailAddress, st, nil, state)
	require.ErrorContains(err, "1 unresolved message")
	assert.Equal(1, rebuilds, "pending source work must not skip existing cache maintenance")
	assert.Contains(logs.String(), `"unresolved_messages":1`)
	require.NoError(os.Remove(cfg.AttachmentsDir()))
	require.NoError(runScheduledSync(ctx, mock.Profile.EmailAddress, st, nil, state), "successful retained-MIME replay clears source failure")
	assert.Equal(2, rebuilds)
}

func TestGmailSyncErrorSummarySeparatesAttemptsAndCurrentDebt(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		attempts, unresolved int64
		want                 string
	}{
		{"historical attempts settled", 18, 0, "Unresolved:    0"},
		{"current work remains", 18, 5, "Unresolved:    5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			var out bytes.Buffer
			writeGmailSyncErrors(&out, &gmail.SyncSummary{Errors: tc.attempts, UnresolvedMessages: tc.unresolved})
			assert.Contains(out.String(), "Error attempts: 18")
			assert.Contains(out.String(), tc.want)
		})
	}
}
