package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func newAddSynctechSMSDriveCmd() *cobra.Command {
	var opts struct {
		OwnerPhone      string
		FolderID        string
		GoogleAccount   string
		Schedule        string
		OAuthApp        string
		SkipAuthForTest bool
	}
	cmd := &cobra.Command{
		Use:   "add-synctech-sms-drive <name>",
		Short: "Configure a Google Drive SMS Backup & Restore source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.OwnerPhone == "" {
				return fmt.Errorf("--owner-phone is required")
			}
			if opts.FolderID == "" {
				return fmt.Errorf("--folder-id is required")
			}
			if opts.GoogleAccount == "" {
				return fmt.Errorf("--google-account is required")
			}
			name := args[0]
			if cfg.GetSynctechSMSSource(name) != nil {
				return fmt.Errorf("synctech-sms source %q already exists", name)
			}
			cfg.SynctechSMS.Sources = append(cfg.SynctechSMS.Sources, config.SynctechSMSSource{
				Name:               name,
				Enabled:            true,
				Backend:            "drive",
				FolderID:           opts.FolderID,
				GoogleAccount:      opts.GoogleAccount,
				OwnerPhone:         opts.OwnerPhone,
				Schedule:           opts.Schedule,
				IncludeSMS:         true,
				IncludeMMS:         true,
				IncludeCalls:       true,
				IncludeAttachments: true,
				StableAfter:        "10m",
				OAuthApp:           opts.OAuthApp,
			})
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			if !opts.SkipAuthForTest {
				if err := ensureSynctechSMSDriveToken(cmd.Context(), opts.GoogleAccount, opts.OAuthApp); err != nil {
					return err
				}
				cmd.Println("Drive source configured.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.OwnerPhone, "owner-phone", "", "Owner phone number in E.164 format")
	cmd.Flags().StringVar(&opts.FolderID, "folder-id", "", "Google Drive folder ID")
	cmd.Flags().StringVar(&opts.GoogleAccount, "google-account", "", "Google account email used for Drive OAuth token lookup")
	cmd.Flags().StringVar(&opts.Schedule, "schedule", "30 4 * * *", "Cron schedule for Drive imports")
	cmd.Flags().StringVar(&opts.OAuthApp, "oauth-app", "", "Named OAuth app from config.toml")
	cmd.Flags().BoolVar(&opts.SkipAuthForTest, "skip-auth-for-test", false, "Skip OAuth setup in tests")
	_ = cmd.Flags().MarkHidden("skip-auth-for-test")
	return cmd
}

func newSyncSynctechSMSCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync-synctech-sms <name>",
		Short: "Run one configured synctech-sms source now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := cfg.GetSynctechSMSSource(args[0])
			if src == nil {
				return fmt.Errorf("synctech-sms source %q not found", args[0])
			}
			return runConfiguredSynctechSMSSource(cmd.Context(), *src)
		},
	}
}

func runConfiguredSynctechSMSSource(ctx context.Context, src config.SynctechSMSSource) error {
	st, err := openStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return runConfiguredSynctechSMSSourceWithStore(ctx, st, src)
}

func runConfiguredSynctechSMSSourceWithStore(ctx context.Context, st *store.Store, src config.SynctechSMSSource) error {
	opts := synctechImportOptions(src)
	if opts.OwnerPhone == "" {
		return fmt.Errorf("synctech-sms source %q owner_phone is required", src.Name)
	}

	switch src.Backend {
	case "", "local":
		if src.Path == "" {
			return fmt.Errorf("synctech-sms source %q path is required for local backend", src.Name)
		}
		source, err := ensureConfiguredSynctechSMSSource(st, src, opts)
		if err != nil {
			return err
		}
		if _, err := synctechsms.NewImporter(st, opts).ImportPath(src.Path); err != nil {
			return err
		}
		if err := st.TouchSourceLastSyncAt(source.ID); err != nil {
			return err
		}
	case "drive":
		if err := runSynctechSMSDriveSource(ctx, st, src, opts); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported synctech-sms backend %q", src.Backend)
	}
	rebuildCacheAfterSync("synctech-sms:"+src.Name, src.Name)
	return nil
}

func ensureConfiguredSynctechSMSSource(st *store.Store, src config.SynctechSMSSource, opts synctechsms.ImportOptions) (*store.Source, error) {
	source, err := st.GetOrCreateSource(synctechsms.SourceType, opts.OwnerPhone)
	if err != nil {
		return nil, fmt.Errorf("get source: %w", err)
	}
	confirmDefaultIdentity(io.Discard, st, source.ID, src.Name, opts.OwnerPhone, "account-identifier")
	if err := runPostSourceCreateMigrations(st); err != nil {
		return nil, fmt.Errorf("post-source-create migrations: %w", err)
	}
	return source, nil
}

func runSynctechSMSDriveSource(ctx context.Context, st *store.Store, src config.SynctechSMSSource, opts synctechsms.ImportOptions) error {
	if src.GoogleAccount == "" {
		return fmt.Errorf("synctech-sms source %q google_account is required", src.Name)
	}
	if src.FolderID == "" {
		return fmt.Errorf("synctech-sms source %q folder_id is required", src.Name)
	}
	client, err := newSynctechSMSDriveClient(ctx, src)
	if err != nil {
		return err
	}
	return runSynctechSMSDriveSourceWithClient(ctx, st, src, opts, client, time.Now)
}

func runSynctechSMSDriveSourceWithClient(ctx context.Context, st *store.Store, src config.SynctechSMSSource, opts synctechsms.ImportOptions, client synctechsms.DriveClient, now func() time.Time) error {
	source, err := ensureConfiguredSynctechSMSSource(st, src, opts)
	if err != nil {
		return err
	}
	syncID, err := st.StartSync(source.ID, synctechsms.AdapterName)
	if err != nil {
		return fmt.Errorf("start sync: %w", err)
	}
	files, err := client.ListBackupFiles(ctx, src.FolderID)
	if err != nil {
		return failSynctechSMSDriveSync(st, syncID, err)
	}
	imported, err := st.ListImportedSourceItemChecksums(source.ID, "drive")
	if err != nil {
		return failSynctechSMSDriveSync(st, syncID, err)
	}
	stableAfter, err := time.ParseDuration(src.StableAfter)
	if err != nil {
		return failSynctechSMSDriveSync(st, syncID, fmt.Errorf("parse stable_after: %w", err))
	}
	selected := synctechsms.SelectStableDriveFiles(files, now(), stableAfter, imported)
	imp := synctechsms.NewImporter(st, opts)
	var totalSummary synctechsms.ImportSummary
	for _, file := range selected {
		stagingDir := filepath.Join(cfg.Data.DataDir, "imports", "synctech-sms", src.Name)
		if err := os.MkdirAll(stagingDir, 0o700); err != nil {
			return failSynctechSMSDriveSync(st, syncID, fmt.Errorf("create staging directory: %w", err))
		}
		staged := filepath.Join(stagingDir, file.ID+"-"+filepath.Base(file.Name))
		item := store.SourceImportItem{
			SourceID:   source.ID,
			Provider:   "drive",
			ProviderID: file.ID,
			Name:       file.Name,
			Checksum:   file.Checksum,
			Size:       file.Size,
			ModifiedAt: sql.NullTime{Time: file.ModifiedTime, Valid: !file.ModifiedTime.IsZero()},
			Status:     "pending",
		}
		if err := st.UpsertSourceImportItem(item); err != nil {
			return failSynctechSMSDriveSync(st, syncID, err)
		}
		if err := client.DownloadToFile(ctx, file.ID, staged); err != nil {
			item.Status = "failed"
			item.ErrorMessage = sql.NullString{String: err.Error(), Valid: true}
			_ = st.UpsertSourceImportItem(item)
			return failSynctechSMSDriveSync(st, syncID, err)
		}
		summary, err := imp.ImportPathForSource(source.ID, staged)
		if err != nil {
			item.Status = "failed"
			item.ErrorMessage = sql.NullString{String: err.Error(), Valid: true}
			item.RecordsImported = summary.SMSImported + summary.MMSImported + summary.CallsImported
			_ = st.UpsertSourceImportItem(item)
			return failSynctechSMSDriveSync(st, syncID, err)
		}
		item.Status = "imported"
		item.ImportedAt = sql.NullTime{Time: now(), Valid: true}
		item.RecordsImported = summary.SMSImported + summary.MMSImported + summary.CallsImported
		item.ErrorMessage = sql.NullString{}
		if err := st.UpsertSourceImportItem(item); err != nil {
			return failSynctechSMSDriveSync(st, syncID, err)
		}
		totalSummary.FilesSeen += summary.FilesSeen
		totalSummary.FilesImported += summary.FilesImported
		totalSummary.SMSImported += summary.SMSImported
		totalSummary.MMSImported += summary.MMSImported
		totalSummary.CallsImported += summary.CallsImported
		totalSummary.AttachmentsImported += summary.AttachmentsImported
	}
	total := int64(totalSummary.SMSImported + totalSummary.MMSImported + totalSummary.CallsImported)
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: total,
		MessagesAdded:     total,
	}); err != nil {
		return failSynctechSMSDriveSync(st, syncID, fmt.Errorf("update sync checkpoint: %w", err))
	}
	if err := st.CompleteSync(syncID, ""); err != nil {
		_ = st.FailSync(syncID, err.Error())
		return fmt.Errorf("complete sync: %w", err)
	}
	if err := st.TouchSourceLastSyncAt(source.ID); err != nil {
		return fmt.Errorf("touch source last sync: %w", err)
	}
	if len(selected) > 0 {
		if err := st.RecomputeConversationStats(source.ID); err != nil {
			return fmt.Errorf("recompute conversation stats: %w", err)
		}
	}
	return nil
}

func failSynctechSMSDriveSync(st *store.Store, syncID int64, err error) error {
	if err == nil {
		return nil
	}
	if failErr := st.FailSync(syncID, err.Error()); failErr != nil {
		return fmt.Errorf("%w (also failed to mark sync failed: %v)", err, failErr)
	}
	return err
}

var newSynctechSMSDriveClient = newGoogleSynctechSMSDriveClient

func newGoogleSynctechSMSDriveClient(ctx context.Context, src config.SynctechSMSSource) (synctechsms.DriveClient, error) {
	clientSecrets, err := cfg.OAuth.ClientSecretsFor(src.OAuthApp)
	if err != nil {
		return nil, err
	}
	mgr, err := newSynctechSMSDriveOAuthManager(clientSecrets)
	if err != nil {
		return nil, err
	}
	if !mgr.HasToken(src.GoogleAccount) {
		return nil, fmt.Errorf("no Drive OAuth token for %s; run add-synctech-sms-drive on a machine with browser auth first", src.GoogleAccount)
	}
	ts, err := mgr.TokenSource(ctx, src.GoogleAccount)
	if err != nil {
		return nil, err
	}
	service, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("create Drive service: %w", err)
	}
	return synctechsms.NewGoogleDriveClient(service), nil
}

func ensureSynctechSMSDriveToken(ctx context.Context, googleAccount, oauthApp string) error {
	clientSecrets, err := cfg.OAuth.ClientSecretsFor(oauthApp)
	if err != nil {
		return err
	}
	mgr, err := newSynctechSMSDriveOAuthManager(clientSecrets)
	if err != nil {
		return err
	}
	if mgr.HasToken(googleAccount) {
		return nil
	}
	return mgr.Authorize(ctx, googleAccount)
}

func newSynctechSMSDriveOAuthManager(clientSecrets string) (*oauth.Manager, error) {
	// The current OAuth manager validates account identity through Gmail's
	// profile endpoint, so request a read-only Gmail scope alongside Drive.
	return oauth.NewManagerWithScopes(clientSecrets, cfg.TokensDir(), logger, []string{
		drive.DriveReadonlyScope,
		"https://www.googleapis.com/auth/gmail.readonly",
	})
}

func synctechImportOptions(src config.SynctechSMSSource) synctechsms.ImportOptions {
	return synctechsms.ImportOptions{
		OwnerPhone:         src.OwnerPhone,
		AttachmentsDir:     cfg.AttachmentsDir(),
		IncludeSMS:         src.IncludeSMS,
		IncludeMMS:         src.IncludeMMS,
		IncludeCalls:       src.IncludeCalls,
		IncludeAttachments: src.IncludeAttachments,
	}
}

func init() {
	rootCmd.AddCommand(newAddSynctechSMSDriveCmd())
	rootCmd.AddCommand(newSyncSynctechSMSCmd())
}
