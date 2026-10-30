package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

// catalogSnapshot is the cached upstream media catalogue. The catalogue endpoint
// is anonymous, so the plugin can warm it before any credential exists; that is
// what lets CPA show the 42 Miora models on a fresh install.
type catalogSnapshot struct {
	Models    []catalogModel
	Tier      string
	FetchedAt time.Time
	Err       string
}

var catalogCache struct {
	mu         sync.RWMutex
	snapshot   catalogSnapshot
	inflight   chan struct{}
	generation uint64
}

// loadCatalog returns the cached catalogue, refreshing it when the TTL expired.
// Concurrent callers share one upstream request.
func loadCatalog(ctx context.Context) catalogSnapshot {
	cache := &catalogCache
	cache.mu.Lock()
	snapshot := cache.snapshot

	ttl := config().catalogTTL()
	if snapshot.Err != "" || len(snapshot.Models) == 0 {
		ttl = min(ttl, 30*time.Second)
	}
	if !snapshot.FetchedAt.IsZero() && time.Since(snapshot.FetchedAt) < ttl {
		cache.mu.Unlock()
		return snapshot
	}

	if cache.inflight != nil {
		wait := cache.inflight
		cache.mu.Unlock()
		select {
		case <-wait:
			return loadCatalog(ctx)
		case <-ctx.Done():
			return peekCatalog()
		}
	}
	done := make(chan struct{})
	cache.inflight = done
	generation := cache.generation
	cache.mu.Unlock()

	client := newAPIClient(config(), nil)
	models, tier, errFetch := client.fetchCatalog(ctx)

	cache.mu.Lock()
	// Discard results from a configuration invalidated during the fetch.
	if generation == cache.generation {
		if errFetch == nil && len(models) > 0 {
			cache.snapshot = catalogSnapshot{Models: models, Tier: tier, FetchedAt: time.Now()}
		} else {
			cache.snapshot.Err = ""
			if errFetch != nil {
				cache.snapshot.Err = errFetch.Error()
			}
			cache.snapshot.FetchedAt = time.Now()
		}
	}
	snapshot = cache.snapshot
	cache.inflight = nil
	cache.mu.Unlock()
	close(done)
	return snapshot
}

// peekCatalog returns the cached catalogue without any upstream call.
func peekCatalog() catalogSnapshot {
	catalogCache.mu.RLock()
	defer catalogCache.mu.RUnlock()
	return catalogCache.snapshot
}

// invalidateCatalog forces the next load to hit the upstream.
func invalidateCatalog() {
	catalogCache.mu.Lock()
	catalogCache.generation++
	catalogCache.snapshot.FetchedAt = time.Time{}
	catalogCache.mu.Unlock()
}

// warmCatalog fetches the catalogue in the background right after registration so
// the first model listing is already populated.
func warmCatalog() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		snapshot := loadCatalog(ctx)
		if len(snapshot.Models) > 0 {
			logInfo("miora catalogue loaded", map[string]any{
				"models": len(snapshot.Models),
				"tier":   snapshot.Tier,
			})
		} else if snapshot.Err != "" {
			logWarn("miora catalogue unavailable", map[string]any{"error": snapshot.Err})
		}
	}()
}

// loadCatalogCached is the synchronous variant used where a short block is fine
// (model registration, panel refresh): it returns the cached catalogue and only
// hits the upstream when the cache is cold or stale.
func loadCatalogCached() catalogSnapshot {
	ctx, cancel := contextWithTimeout(25 * time.Second)
	defer cancel()
	return loadCatalog(ctx)
}

// contextWithTimeout is a tiny helper so call sites stay one line.
func contextWithTimeout(duration time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), duration)
}

// findCatalogModel looks up one upstream model ID.
func findCatalogModel(upstream string) (catalogModel, bool) {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return catalogModel{}, false
	}
	snapshot := peekCatalog()
	for _, model := range snapshot.Models {
		if strings.EqualFold(model.ModelID, upstream) {
			return model, true
		}
	}
	return catalogModel{}, false
}

// mediaKindForModel resolves the media kind for a model ID, falling back to the
// operator list and finally to name heuristics so unknown IDs still work.
func mediaKindForModel(cfg *Config, upstream string) string {
	if model, ok := findCatalogModel(upstream); ok {
		return model.mediaKind()
	}
	if cfg != nil {
		for _, override := range cfg.Models {
			if strings.EqualFold(override.Upstream, upstream) || strings.EqualFold(override.ID, upstream) {
				return override.Media
			}
		}
	}
	lower := strings.ToLower(upstream)
	switch {
	case strings.Contains(lower, "video"), strings.Contains(lower, "kling"), strings.Contains(lower, "vidu"),
		strings.Contains(lower, "hailuo"), strings.Contains(lower, "seedance"), strings.Contains(lower, "minimax"):
		return mediaVideo
	case strings.Contains(lower, "3d"), strings.Contains(lower, "tripo"), strings.Contains(lower, "hunyuan3d"):
		return mediaThreeD
	default:
		return mediaImage
	}
}

// defaultModelFor returns the configured fallback model for one media kind.
func defaultModelFor(cfg *Config, media string) string {
	if cfg == nil {
		return ""
	}
	switch media {
	case mediaVideo:
		return cfg.DefaultVideoModel
	case mediaThreeD:
		return cfg.Default3DModel
	default:
		return cfg.DefaultImageModel
	}
}

// catalogueModels merges the upstream catalogue with operator-defined models,
// applying include/exclude filters and de-duplicating by upstream ID.
func catalogueModels(cfg *Config) []catalogModel {
	snapshot := peekCatalog()
	out := make([]catalogModel, 0, len(snapshot.Models)+len(cfg.Models))
	seen := map[string]struct{}{}
	for _, model := range snapshot.Models {
		upstream := strings.TrimSpace(model.ModelID)
		if upstream == "" || !cfg.modelAllowed(upstream) {
			continue
		}
		if _, ok := seen[strings.ToLower(upstream)]; ok {
			continue
		}
		seen[strings.ToLower(upstream)] = struct{}{}
		out = append(out, model)
	}
	for _, override := range cfg.Models {
		upstream := strings.TrimSpace(override.Upstream)
		if upstream == "" || !cfg.modelAllowed(upstream) {
			continue
		}
		if _, ok := seen[strings.ToLower(upstream)]; ok {
			continue
		}
		seen[strings.ToLower(upstream)] = struct{}{}
		out = append(out, catalogModel{
			ModelID:     upstream,
			ContentType: override.Media,
			Label:       firstNonEmpty(override.Label, override.ID, upstream),
			Available:   true,
		})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
