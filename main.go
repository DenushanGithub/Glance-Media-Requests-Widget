package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"
)

// --- CONFIG ---

const (
	MetadataTTL    = 24 * time.Hour
	maxBodyBytes   = 1 << 20 // 1 MB
	rateLimitBurst = 30
	rateLimitWin   = time.Minute
)

var (
	SeerrURL          = getEnv("SEERR_URL", "http://192.168.2.50:5055/api/v1")
	APIKey            = os.Getenv("SEERR_API_KEY")
	WebhookAuthHeader = os.Getenv("WEBHOOK_AUTH_HEADER")
	RequestsAuthToken = os.Getenv("REQUESTS_AUTH_TOKEN")
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const banner = `
  GLANCE SEERR API
`

const version = "v1.0.0"

// --- TYPES ---

type SeerrResponse struct {
	Results []SeerrItem `json:"results"`
}

type SeerrItem struct {
	Type  string `json:"type"`
	Media struct {
		TMDBId int `json:"tmdbId"`
		Status int `json:"status"`
	} `json:"media"`
	RequestedBy struct {
		DisplayName string `json:"displayName"`
	} `json:"requestedBy"`
	Status int `json:"status"`
}

// Movies use Title/ReleaseDate; TV shows use Name/FirstAirDate.
type MetadataResponse struct {
	Title       string `json:"title"`
	Name        string `json:"name"`
	ReleaseDate string `json:"releaseDate"`
	FirstAir    string `json:"firstAirDate"`
	PosterPath  string `json:"posterPath"`
}

type CachedMetadata struct {
	Data      MetadataResponse
	Timestamp time.Time
}

type RefinedResult struct {
	Title         string `json:"title"`
	Year          string `json:"year"`
	Poster        string `json:"poster"`
	Type          string `json:"type"`
	TMDBId        int    `json:"tmdbId"`
	RequestedBy   string `json:"requestedBy"`
	RequestStatus int    `json:"requestStatus"`
	MediaStatus   int    `json:"mediaStatus"`
}

type WebhookPayload struct {
	NotificationType string `json:"notification_type"`
}

// --- RATE LIMITER ---

// Simple per-IP sliding-window rate limiter.
type rateLimiter struct {
	mu      sync.Mutex
	clients map[string][]time.Time
}

func newRateLimiter() *rateLimiter {
	rl := &rateLimiter{clients: make(map[string][]time.Time)}

	// Prune stale entries every minute so the map doesn't grow forever.
	go func() {
		t := time.NewTicker(rateLimitWin)
		defer t.Stop()
		for range t.C {
			rl.mu.Lock()
			cutoff := time.Now().Add(-rateLimitWin)
			for ip, ts := range rl.clients {
				filtered := ts[:0]
				for _, t := range ts {
					if t.After(cutoff) {
						filtered = append(filtered, t)
					}
				}
				if len(filtered) == 0 {
					delete(rl.clients, ip)
				} else {
					rl.clients[ip] = filtered
				}
			}
			rl.mu.Unlock()
		}
	}()

	return rl
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rateLimitWin)
	ts := rl.clients[ip]

	valid := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rateLimitBurst {
		rl.clients[ip] = valid
		return false
	}

	rl.clients[ip] = append(valid, now)
	return true
}

// --- GLOBALS ---

var (
	httpClient    = &http.Client{Timeout: 10 * time.Second}
	metadataCache sync.Map
	sfGroup       singleflight.Group // collapses concurrent fetches into one
	cacheMu       sync.RWMutex
	cachedResults []RefinedResult
	rl            = newRateLimiter()
)

// --- HELPERS ---

func logMsg(symbol, msg string) {
	fmt.Printf("%s %s\n", symbol, msg)
}

func printBanner() {
	fmt.Println(banner)
	fmt.Printf("  %-12s %s\n", "Version:", version)
	fmt.Printf("  %-12s %s\n", "Seerr URL:", SeerrURL)
	fmt.Printf("  %-12s %s\n", "Started:", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Println()
	fmt.Println("  ──────────────────────────────")
	fmt.Println()
}

func displayTitle(meta MetadataResponse) string {
	if meta.Title != "" {
		return meta.Title
	}
	return meta.Name
}

func extractYear(meta MetadataResponse) string {
	switch {
	case len(meta.ReleaseDate) >= 4:
		return meta.ReleaseDate[:4]
	case len(meta.FirstAir) >= 4:
		return meta.FirstAir[:4]
	default:
		return ""
	}
}

// secureCompare prevents timing-based token leakage during auth checks.
func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// clientIP checks X-Forwarded-For first to handle reverse proxy deployments.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}

// validateSeerrURL rejects non-HTTP(S) schemes to prevent SSRF via a crafted env var.
func validateSeerrURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("SEERR_URL parse error: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("SEERR_URL must use http or https, got: %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("SEERR_URL has no host")
	}
	return nil
}

// --- MIDDLEWARE ---

func withRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.allow(ip) {
			logMsg("[!]", fmt.Sprintf("Rate limit hit for %s on %s", ip, r.URL.Path))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func withBearerAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if len(header) <= len(prefix) || !secureCompare(header[len(prefix):], token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- METADATA FETCH WITH CACHE ---

func getMetadata(tmdbId int, mediaType string) (MetadataResponse, error) {
	if v, ok := metadataCache.Load(tmdbId); ok {
		entry := v.(CachedMetadata)
		if time.Since(entry.Timestamp) < MetadataTTL {
			fmt.Printf("⚡ Cache hit for TMDB ID %-7d - %s\n", tmdbId, displayTitle(entry.Data))
			return entry.Data, nil
		}
	}

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/%s/%d", SeerrURL, mediaType, tmdbId), nil)
	if err != nil {
		return MetadataResponse{}, err
	}
	req.Header.Set("X-Api-Key", APIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return MetadataResponse{}, err
	}
	defer resp.Body.Close()

	var meta MetadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return MetadataResponse{}, err
	}

	fmt.Printf("→ Fetched metadata for TMDB ID %-7d - %s\n", tmdbId, displayTitle(meta))
	metadataCache.Store(tmdbId, CachedMetadata{Data: meta, Timestamp: time.Now()})
	return meta, nil
}

// --- MAIN FETCH ---

func fetchAndStore() ([]RefinedResult, error) {
	v, err, _ := sfGroup.Do("fetch", func() (interface{}, error) {
		logMsg("[*]", "Starting Seerr sync...")

		req, err := http.NewRequest("GET", SeerrURL+"/request?take=15&sort=added", nil)
		if err != nil {
			logMsg("[!]", "Request creation failed")
			return nil, err
		}
		req.Header.Set("X-Api-Key", APIKey)

		resp, err := httpClient.Do(req)
		if err != nil {
			logMsg("[!]", "Seerr fetch failed")
			return nil, err
		}
		defer resp.Body.Close()

		var sResp SeerrResponse
		if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
			logMsg("[!]", "Decode failed")
			return nil, err
		}

		logMsg("[*]", fmt.Sprintf("Processing %d items...", len(sResp.Results)))

		// Pre-allocate at full length so each goroutine writes to its own index
		// without needing a mutex. valid[i] tracks whether the fetch succeeded.
		tempResults := make([]RefinedResult, len(sResp.Results))
		valid := make([]bool, len(sResp.Results))

		var wg sync.WaitGroup
		for i, item := range sResp.Results {
			if item.Media.TMDBId == 0 {
				continue
			}
			wg.Add(1)
			go func(idx int, it SeerrItem) {
				defer wg.Done()
				meta, err := getMetadata(it.Media.TMDBId, it.Type)
				if err != nil {
					logMsg("[!]", "Metadata fetch error")
					return
				}
				tempResults[idx] = RefinedResult{
					Title:         displayTitle(meta),
					Year:          extractYear(meta),
					Poster:        meta.PosterPath,
					Type:          it.Type,
					TMDBId:        it.Media.TMDBId,
					RequestedBy:   it.RequestedBy.DisplayName,
					RequestStatus: it.Status,
					MediaStatus:   it.Media.Status,
				}
				valid[idx] = true
			}(i, item)
		}
		wg.Wait()

		ordered := make([]RefinedResult, 0, len(sResp.Results))
		for i, r := range tempResults {
			if valid[i] {
				ordered = append(ordered, r)
			}
		}

		cacheMu.Lock()
		cachedResults = ordered
		cacheMu.Unlock()

		logMsg("[+]", fmt.Sprintf("SUCCESS: %d items cached.", len(ordered)))
		logMsg("──────────────────────────────", fmt.Sprintf("Sync complete: %s", time.Now().Format("15:04:05")))
		return ordered, nil
	})

	if err != nil {
		return nil, err
	}
	return v.([]RefinedResult), nil
}

// --- WEBHOOK HANDLER ---

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if WebhookAuthHeader != "" {
		if !secureCompare(r.Header.Get("Authorization"), WebhookAuthHeader) {
			logMsg("[!]", "Webhook rejected: bad auth")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var payload WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	logMsg("[W]", fmt.Sprintf("Webhook received: %s", payload.NotificationType))

	// Fire-and-forget; singleflight collapses any rapid duplicates.
	go fetchAndStore()

	w.WriteHeader(http.StatusOK)
}

// --- REQUESTS HANDLER ---

func handleRequests(w http.ResponseWriter, r *http.Request) {
	cacheMu.RLock()
	results := cachedResults
	cacheMu.RUnlock()

	if results == nil {
		var err error
		results, err = fetchAndStore()
		if err != nil || results == nil {
			results = []RefinedResult{}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"results": results})
}

// --- MAIN ---

func main() {
	if err := validateSeerrURL(SeerrURL); err != nil {
		logMsg("[!]", fmt.Sprintf("Invalid SEERR_URL: %v", err))
		os.Exit(1)
	}

	if RequestsAuthToken == "" {
		logMsg("[!]", "WARNING: REQUESTS_AUTH_TOKEN is not set — /requests is unprotected")
	}
	if WebhookAuthHeader == "" {
		logMsg("[!]", "WARNING: WEBHOOK_AUTH_HEADER is not set — /webhook auth is disabled")
	}

	printBanner()

	mux := http.NewServeMux()

	requestsHandler := withRateLimit(handleRequests)
	if RequestsAuthToken != "" {
		requestsHandler = withRateLimit(withBearerAuth(RequestsAuthToken, handleRequests))
	}
	mux.HandleFunc("/requests", requestsHandler)
	mux.HandleFunc("/webhook", withRateLimit(handleWebhook))

	srv := &http.Server{
		Addr:         ":5000",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go fetchAndStore()

	// Refresh cache daily to catch status changes missed by webhooks.
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			logMsg("[T]", "Daily refresh triggered")
			go fetchAndStore()
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM, up to 15 seconds.
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		logMsg("[*]", "Shutdown signal received — draining...")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logMsg("[!]", fmt.Sprintf("Shutdown error: %v", err))
		}
	}()

	logMsg("[*]", "Proxy service listening on :5000")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logMsg("[!]", fmt.Sprintf("Server failed: %v", err))
		os.Exit(1)
	}
	logMsg("[*]", "Server stopped cleanly.")
}