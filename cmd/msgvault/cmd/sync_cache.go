package cmd

var rebuildCacheAfterSync = rebuildCacheAfterSyncImpl

func rebuildCacheAfterSyncImpl(syncKind, identifier string) {
	dbPath := cfg.DatabaseDSN()
	analyticsDir := cfg.AnalyticsDir()
	if staleness := cacheNeedsBuild(dbPath, analyticsDir); staleness.NeedsBuild {
		logger.Info("rebuilding cache after sync",
			"sync_kind", syncKind,
			"identifier", identifier,
			"reason", staleness.Reason,
			"full_rebuild", staleness.FullRebuild)
		result, err := buildCache(dbPath, analyticsDir, staleness.FullRebuild)
		if err != nil {
			logger.Error("cache build failed",
				"sync_kind", syncKind,
				"identifier", identifier,
				"error", err)
			return
		}
		if !result.Skipped {
			logger.Info("cache build completed",
				"sync_kind", syncKind,
				"identifier", identifier,
				"exported", result.ExportedCount)
		}
	}
}
