// map-gateway: standalone tile proxy + disk cache for terrain / satellite /
// Tianditu (天地图) tiles. Extracted from the webapp tile handlers so the
// browser-facing URLs stay exactly the same; nginx will later route
// /tile/ and /tianditu/ here and keep the rest on webapp:8080.
//
// Routes (GET only, browser-compatible):
//
//	/health, /healthz                          -> "ok" (systemd / probes)
//	/geocode/reverse?lon={lon}&lat={lat}        -> Tianditu reverse geocoding
//	/tile/terrain/{z}/{x}/{y}.png              -> AWS Terrarium elevation tiles
//	/tile/sat/{z}/{x}/{y}.jpg|.png             -> ArcGIS World_Imagery
//	/tianditu/{vec|cva|img|cia}/{z}/{x}/{y}.png -> Tianditu WMTS (needs TIANDITU_KEY)
//
// All tile path components are strictly numeric and length-bounded, so no
// path traversal or odd input can reach the filesystem or upstream. Tile
// URLs are path-only: any query string (cache-buster, injected parameter)
// is rejected with 404 before rate limiting, cache or upstream handling.
//
// Responses are disk-cached under MAP_CACHE_DIR (bounded size, oldest-first
// eviction to a watermark) and served with
// `Cache-Control: public, max-age=86400`, mirroring the webapp semantics.
//
// Configuration comes exclusively from the environment (optionally via an
// env file, MAP_ENV_FILE). Provider credentials for keyless upstreams
// (Terrarium / ArcGIS) are never exposed and are redacted from logs.
// TIANDITU_KEY is a *client-side* key (the browser sends tk= with every
// Tianditu request); it is exposed via GET /cfg/maps so the frontend can
// fail over to direct upstream access if this gateway is unreachable.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Defaults / constants
// ---------------------------------------------------------------------------

const (
	defaultListenAddr      = "127.0.0.1:8082"
	defaultCacheMaxMB      = 200
	defaultEvictWatermark  = 0.8 // evict down to 80% of the cap
	defaultTileRate        = 600 // per IP per minute, shared across tile endpoints
	defaultUpstreamTimeout = 45 * time.Second
	defaultMaxTileBytes    = 8 << 20 // 8 MiB per tile, guards against runaway upstreams
	maxZoom                = 30

	// Admin dashboard login (fixed credential, password-only check).
	defaultAdminUser   = "admin"
	defaultAdminPass   = "boygo"
	adminSessionTTL    = 24 * time.Hour
	adminLockThreshold = 5                // failed attempts before lockout
	adminLockDuration  = 60 * time.Second // lockout duration
	adminCookieName    = "mapgw_admin"
	adminMinuteHistory = 60 // keep 60 minutes of per-minute request history

	// HTTP-level TTL, same as the webapp tile handlers.
	cacheControlHeader = "public, max-age=86400"

	// Upstream providers (public, no credentials needed).
	terrainUpstream = "https://s3.amazonaws.com/elevation-tiles-prod/terrarium/%s/%s/%s.png"
	// ArcGIS tile order is /tile/{z}/{row=y}/{col=x}.
	satUpstream = "https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/%s/%s/%s"
	// Tianditu WMTS; tk=<TIANDITU_KEY> is appended server-side only.
	tiandituUpstream = "https://t%d.tianditu.gov.cn/%s_w/wmts?SERVICE=WMTS&REQUEST=GetTile&VERSION=1.0.0&LAYER=%s&STYLE=default&TILEMATRIXSET=w&FORMAT=tiles&TILEMATRIX=%s&TILEROW=%s&TILECOL=%s&tk=%s"
	geocoderUpstream = "https://api.tianditu.gov.cn/geocoder"
)

// Strictly numeric, length-bounded path components: z up to 2 digits,
// x/y up to 10 digits, so no "..", separators or anything else can pass.
var (
	tileURLRe     = regexp.MustCompile(`^/tile/(terrain|sat)/([0-9]{1,2})/([0-9]{1,10})/([0-9]{1,10})\.(png|jpg)$`)
	tiandituURLRe = regexp.MustCompile(`^/tianditu/(vec|cva|img|cia)/([0-9]{1,2})/([0-9]{1,10})/([0-9]{1,10})\.png$`)
)

// ---------------------------------------------------------------------------
// Configuration (environment only)
// ---------------------------------------------------------------------------

type config struct {
	ListenAddr      string
	CacheDir        string
	CacheMax        int64
	EvictWatermark  float64
	TileRate        int
	UpstreamTimeout time.Duration
	MaxTileBytes    int64
	TiandituKey     string
	EnvFile         string
	// Admin dashboard (fixed credential).
	AdminUser string
	AdminPass string
	// Upstream base URLs, overridable mainly for tests / mirrors.
	TerrainUpstream  string
	SatUpstream      string
	TiandituUpstream string
	GeocoderUpstream string
}

func defaultConfig() config {
	return config{
		ListenAddr:       defaultListenAddr,
		CacheDir:         "cache",
		CacheMax:         int64(defaultCacheMaxMB) << 20,
		EvictWatermark:   defaultEvictWatermark,
		TileRate:         defaultTileRate,
		UpstreamTimeout:  defaultUpstreamTimeout,
		MaxTileBytes:     defaultMaxTileBytes,
		EnvFile:          ".env",
		AdminUser:        defaultAdminUser,
		AdminPass:        defaultAdminPass,
		TerrainUpstream:  terrainUpstream,
		SatUpstream:      satUpstream,
		TiandituUpstream: tiandituUpstream,
		GeocoderUpstream: geocoderUpstream,
	}
}

// configFromEnv builds the config from the process environment, falling back
// to an env file (MAP_ENV_FILE, default ./.env) for values not set in the
// environment — the same precedence systemd's EnvironmentFile provides.
func configFromEnv() config {
	cfg := defaultConfig()
	// MAP_ENV_FILE itself must be resolved before the env file is parsed.
	cfg.EnvFile = firstNonEmpty(os.Getenv("MAP_ENV_FILE"), cfg.EnvFile)
	fileVars := map[string]string{}
	if cfg.EnvFile != "" {
		if kv, err := parseEnvFile(cfg.EnvFile); err == nil {
			fileVars = kv
		}
	}
	// env beats file beats default
	get := func(name string) string {
		return firstNonEmpty(os.Getenv(name), fileVars[name])
	}

	cfg.ListenAddr = firstNonEmpty(get("MAP_LISTEN_ADDR"), cfg.ListenAddr)
	cfg.CacheDir = firstNonEmpty(get("MAP_CACHE_DIR"), cfg.CacheDir)
	cfg.TiandituKey = get("TIANDITU_KEY")
	cfg.AdminUser = firstNonEmpty(get("MAP_ADMIN_USER"), cfg.AdminUser)
	cfg.AdminPass = firstNonEmpty(get("MAP_ADMIN_PASS"), cfg.AdminPass)
	if v := get("MAP_CACHE_MAX_MB"); v != "" {
		if mb, err := strconv.Atoi(v); err == nil && mb > 0 {
			cfg.CacheMax = int64(mb) << 20
		}
	}
	if v := get("MAP_CACHE_EVICT_WATERMARK"); v != "" {
		if w, err := strconv.ParseFloat(v, 64); err == nil && w > 0 && w <= 1 {
			cfg.EvictWatermark = w
		}
	}
	if v := get("MAP_TILE_RATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TileRate = n
		}
	}
	if v := get("MAP_UPSTREAM_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.UpstreamTimeout = d
		}
	}
	if v := get("MAP_MAX_TILE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxTileBytes = n
		}
	}
	// Overrides for tests / mirror deployments; defaults match production.
	cfg.TerrainUpstream = firstNonEmpty(get("MAP_TERRAIN_UPSTREAM"), cfg.TerrainUpstream)
	cfg.SatUpstream = firstNonEmpty(get("MAP_SAT_UPSTREAM"), cfg.SatUpstream)
	cfg.TiandituUpstream = firstNonEmpty(get("MAP_TIANDITU_UPSTREAM"), cfg.TiandituUpstream)
	cfg.GeocoderUpstream = firstNonEmpty(get("MAP_GEOCODER_UPSTREAM"), cfg.GeocoderUpstream)
	return cfg
}

// parseEnvFile reads KEY=VALUE lines; # comments and blank lines are skipped.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key != "" {
			vars[key] = val
		}
	}
	return vars, sc.Err()
}

// ---------------------------------------------------------------------------
// Gateway
// ---------------------------------------------------------------------------

type rlRec struct {
	window time.Time
	count  int
}

// ---------------------------------------------------------------------------
// Runtime statistics + admin sessions
// ---------------------------------------------------------------------------

// statsSnapshot holds a copy of the live counters for the admin dashboard.
type statsSnapshot struct {
	Start       time.Time          `json:"start"`
	Total       int64              `json:"totalRequests"`
	CacheHit    int64              `json:"cacheHits"`
	CacheMiss   int64              `json:"cacheMisses"`
	UpstreamOK  int64              `json:"upstreamOK"`
	UpstreamErr int64              `json:"upstreamErrors"`
	RateLimited int64              `json:"rateLimited"`
	BytesOut    int64              `json:"bytesOut"`
	ByEndpoint  map[string]*epStat `json:"byEndpoint"`
	ByApp       map[string]*epStat `json:"byApp"`
	RecentErr   []errRec           `json:"recentErrors"`
	MinuteHist  []minuteRec        `json:"minuteHistory"`
}

type epStat struct {
	Count    int64 `json:"count"`
	CacheHit int64 `json:"cacheHits"`
	Upstream int64 `json:"upstreamFetches"`
	Errors   int64 `json:"errors"`
	Bytes    int64 `json:"bytes"`
}

type errRec struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	DurMS  int64  `json:"durMs"`
}

type minuteRec struct {
	Time   string `json:"time"`
	Count  int64  `json:"count"`
	Errors int64  `json:"errors"`
}

type adminSession struct {
	Token  string
	Expiry time.Time
}

type failRec struct {
	Count   int
	Expires time.Time
}

type gateway struct {
	cfg    config
	client *http.Client

	cacheMu   sync.Mutex
	cacheSize int64

	rlMu     sync.Mutex
	rlCounts map[string]*rlRec

	// Per-cache-key locks deduplicate concurrent upstream fetches of the same tile.
	fetchMu    sync.Mutex
	fetchLocks map[string]*sync.Mutex

	// Runtime statistics (backend of /admin/api/stats).
	statsMu       sync.Mutex
	statsStart    time.Time
	reqTotal      atomic.Int64
	cacheHit      atomic.Int64
	cacheMiss     atomic.Int64
	upstreamOK    atomic.Int64
	upstreamErr   atomic.Int64
	rateLimited   atomic.Int64
	bytesOut      atomic.Int64
	byEndpoint    map[string]*epStat // guarded by statsMu
	byApp         map[string]*epStat // guarded by statsMu
	recentErr     []errRec           // guarded by statsMu
	minuteHistory []minuteRec        // guarded by statsMu
	lastMinute    string             // guarded by statsMu

	// Admin login sessions + failed-attempt lockout.
	sessMu   sync.Mutex
	sessions map[string]adminSession
	fails    map[string]failRec
}

func newGateway(cfg config) *gateway {
	g := &gateway{
		cfg:        cfg,
		client:     &http.Client{Timeout: cfg.UpstreamTimeout},
		rlCounts:   map[string]*rlRec{},
		fetchLocks: map[string]*sync.Mutex{},
		statsStart: time.Now(),
		byEndpoint: map[string]*epStat{},
		byApp:      map[string]*epStat{},
		sessions:   map[string]adminSession{},
		fails:      map[string]failRec{},
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		log.Printf("cache dir %s: %v", cfg.CacheDir, err)
	}
	g.initCacheSize()
	return g
}

func (g *gateway) initCacheSize() {
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	filepath.Walk(g.cfg.CacheDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			g.cacheSize += info.Size()
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// HTTP routing
// ---------------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	path := r.URL.Path
	switch {
	case path == "/admin/login" && r.Method == http.MethodPost:
		g.handleAdminLogin(w, r)
		goto done
	case path == "/admin/logout" && r.Method == http.MethodPost:
		g.handleAdminLogout(w, r)
		goto done
	case path == "/admin" && r.Method == http.MethodGet:
		g.handleAdminPage(w, r)
		goto done
	case path == "/admin/" && r.Method == http.MethodGet:
		g.handleAdminPage(w, r)
		goto done
	case path == "/admin/api/stats" && r.Method == http.MethodGet:
		g.handleAdminStats(w, r)
		goto done
	case strings.HasPrefix(path, "/admin"):
		http.NotFound(w, r)
		goto done
	case r.Method != http.MethodGet:
		rec.Header().Set("Allow", "GET")
		http.Error(rec, "method not allowed", http.StatusMethodNotAllowed)
	case path == "/health" || path == "/healthz":
		rec.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rec.Header().Set("Cache-Control", "no-store")
		rec.WriteHeader(http.StatusOK)
		fmt.Fprintln(rec, "ok")
	case path == "/cfg/maps":
		g.handleMapConfig(rec, r)
	case path == "/geocode/reverse":
		g.handleReverseGeocode(rec, r)
	case strings.HasPrefix(path, "/tile/"):
		g.handleTile(rec, r)
	case strings.HasPrefix(path, "/tianditu/"):
		g.handleTianditu(rec, r)
	default:
		http.NotFound(rec, r)
	}
	// Admin routes are recorded inside their handlers; tiles are recorded here.
	if strings.HasPrefix(path, "/tile/") || strings.HasPrefix(path, "/tianditu/") || path == "/geocode/reverse" {
		g.recordRequest(path, rec.status, rec.written, time.Since(start), "", r.Header.Get("Referer"))
	}
done:
	// Request paths never contain secrets; upstream URLs are never logged.
	if !strings.HasPrefix(path, "/admin") {
		log.Printf("%s %s -> %d (%dB) %s", r.Method, path, rec.status, rec.written,
			time.Since(start).Round(time.Millisecond))
	}
}

type administrativeDivision struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

type reverseGeocodeResponse struct {
	Location struct {
		Lon float64 `json:"lon"`
		Lat float64 `json:"lat"`
	} `json:"location"`
	FormattedAddress string `json:"formattedAddress"`
	Administrative   struct {
		Country  administrativeDivision `json:"country"`
		Province administrativeDivision `json:"province"`
		City     administrativeDivision `json:"city"`
		County   administrativeDivision `json:"county"`
		Town     administrativeDivision `json:"town"`
		Village  administrativeDivision `json:"village"`
	} `json:"administrative"`
	Municipality bool   `json:"municipality"`
	Source       string `json:"source"`
}

type tiandituGeocoderResponse struct {
	Status string `json:"status"`
	Msg    string `json:"msg"`
	Result struct {
		FormattedAddress string `json:"formatted_address"`
		AddressComponent struct {
			Nation       string `json:"nation"`
			Province     string `json:"province"`
			ProvinceCode string `json:"province_code"`
			City         string `json:"city"`
			CityCode     string `json:"city_code"`
			County       string `json:"county"`
			CountyCode   string `json:"county_code"`
			Town         string `json:"town"`
			TownCode     string `json:"town_code"`
		} `json:"addressComponent"`
	} `json:"result"`
}

// handleReverseGeocode returns every administrative level available from one
// lookup. Callers decide which level they need; there is deliberately no level
// request parameter. Tianditu currently has no stable structured village field,
// so village is retained in our contract and returned empty until such a source
// is available.
func (g *gateway) handleReverseGeocode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.tileRateAllow(r) {
		g.rateLimited.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	if g.cfg.TiandituKey == "" {
		http.Error(w, "tianditu key not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	if len(q) != 2 || len(q["lon"]) != 1 || len(q["lat"]) != 1 {
		http.Error(w, "exactly lon and lat are required", http.StatusBadRequest)
		return
	}
	lon, errLon := strconv.ParseFloat(q.Get("lon"), 64)
	lat, errLat := strconv.ParseFloat(q.Get("lat"), 64)
	if errLon != nil || errLat != nil || math.IsNaN(lon) || math.IsNaN(lat) || math.IsInf(lon, 0) || math.IsInf(lat, 0) || lon < -180 || lon > 180 || lat < -90 || lat > 90 {
		http.Error(w, "invalid lon or lat", http.StatusBadRequest)
		return
	}

	postStr, _ := json.Marshal(map[string]interface{}{"lon": lon, "lat": lat, "ver": 1})
	upstream, err := url.Parse(g.cfg.GeocoderUpstream)
	if err != nil {
		http.Error(w, "geocoder unavailable", http.StatusBadGateway)
		return
	}
	params := upstream.Query()
	params.Set("postStr", string(postStr))
	params.Set("type", "geocode")
	params.Set("tk", g.cfg.TiandituKey)
	upstream.RawQuery = params.Encode()

	g.endpointUpstream("geocode/reverse")
	resp, err := g.client.Get(upstream.String())
	if err != nil {
		g.upstreamErr.Add(1)
		log.Printf("geocoder upstream: %s", g.redact(err.Error()))
		http.Error(w, "geocoder unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		g.upstreamErr.Add(1)
		log.Printf("geocoder upstream status %d", resp.StatusCode)
		http.Error(w, "geocoder unavailable", http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		g.upstreamErr.Add(1)
		http.Error(w, "invalid geocoder response", http.StatusBadGateway)
		return
	}
	var raw tiandituGeocoderResponse
	if json.Unmarshal(body, &raw) != nil || raw.Status != "0" {
		g.upstreamErr.Add(1)
		http.Error(w, "geocoder returned no result", http.StatusNotFound)
		return
	}

	ac := raw.Result.AddressComponent
	var out reverseGeocodeResponse
	out.Location.Lon, out.Location.Lat = lon, lat
	out.FormattedAddress = raw.Result.FormattedAddress
	out.Administrative.Country = administrativeDivision{Name: ac.Nation}
	out.Administrative.Province = administrativeDivision{Name: ac.Province, Code: ac.ProvinceCode}
	out.Administrative.City = administrativeDivision{Name: ac.City, Code: ac.CityCode}
	out.Administrative.County = administrativeDivision{Name: ac.County, Code: ac.CountyCode}
	out.Administrative.Town = administrativeDivision{Name: ac.Town, Code: ac.TownCode}
	out.Municipality = isMunicipality(ac.Province)
	out.Source = "tianditu"
	g.upstreamOK.Add(1)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	json.NewEncoder(w).Encode(out)
}

func isMunicipality(province string) bool {
	switch province {
	case "北京市", "上海市", "天津市", "重庆市":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Tile handlers (parity with webapp: rate limit -> path -> cache -> upstream)
// ---------------------------------------------------------------------------

func (g *gateway) handleTile(w http.ResponseWriter, r *http.Request) {
	if !tilePathOnly(r) {
		http.NotFound(w, r)
		return
	}
	if !g.tileRateAllow(r) {
		g.rateLimited.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	m := tileURLRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	kind, z, x, y, ext := m[1], m[2], m[3], m[4], m[5]
	if !validTile(z, x, y) {
		http.NotFound(w, r)
		return
	}
	// Cache key mirrors the webapp layout: {kind}/{z}/{x}/{y}.{ext}
	// so an existing ~/webapp/cache tree can be copied over for pre-warming.
	file := filepath.Join(g.cfg.CacheDir, kind, z, x, y+"."+ext)
	if g.serveCached(w, r, file, ext) {
		g.cacheHit.Add(1)
		g.endpointHit("tile/" + kind)
		return
	}
	g.cacheMiss.Add(1)

	var remote string
	switch kind {
	case "terrain":
		remote = fmt.Sprintf(g.cfg.TerrainUpstream, z, x, y)
	case "sat":
		// ArcGIS expects /tile/{z}/{row=y}/{col=x}.
		remote = fmt.Sprintf(g.cfg.SatUpstream, z, y, x)
	default:
		http.NotFound(w, r)
		return
	}
	g.fetchAndCache(w, r, file, ext, remote)
}

// handleMapConfig serves the frontend fallback config: which direct upstream
// templates exist and whether a client-side Tianditu key is configured.
// TIANDITU_KEY is a browser-side key (tk= appears in every direct request),
// so returning it here only mirrors what the browser would send anyway.
func (g *gateway) handleMapConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	maps := map[string]interface{}{
		"tiandituKey": g.cfg.TiandituKey,
		"upstreams": map[string]string{
			"terrain":  g.cfg.TerrainUpstream,
			"sat":      g.cfg.SatUpstream,
			"tianditu": g.cfg.TiandituUpstream,
		},
	}
	if err := json.NewEncoder(w).Encode(maps); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
}

func (g *gateway) handleTianditu(w http.ResponseWriter, r *http.Request) {
	// Query strings are rejected before the key check too: the route itself
	// is invalid, so even a key-less or key-ful request must 404.
	if !tilePathOnly(r) {
		http.NotFound(w, r)
		return
	}
	// Key check first, same order as webapp: no key -> 503 before anything else.
	if g.cfg.TiandituKey == "" {
		http.Error(w, "tianditu key not configured", http.StatusServiceUnavailable)
		return
	}
	if !g.tileRateAllow(r) {
		g.rateLimited.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	m := tiandituURLRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	layer, z, x, y := m[1], m[2], m[3], m[4]
	if !validTile(z, x, y) {
		http.NotFound(w, r)
		return
	}
	file := filepath.Join(g.cfg.CacheDir, "tianditu", layer, z, x, y+".png")
	if g.serveCached(w, r, file, "png") {
		g.cacheHit.Add(1)
		g.endpointHit("tianditu/" + layer)
		return
	}
	g.cacheMiss.Add(1)
	sub := tiandituSubdomain()
	remote := fmt.Sprintf(g.cfg.TiandituUpstream, sub, layer, layer, z, y, x, g.cfg.TiandituKey)
	g.fetchAndCache(w, r, file, "png", remote)
}

// tilePathOnly reports whether the request carries no query string. Tile
// URLs are path-only, so any query string is rejected before rate limiting,
// cache or upstream handling (ForceQuery covers a bare trailing '?').
func tilePathOnly(r *http.Request) bool {
	return r.URL.RawQuery == "" && !r.URL.ForceQuery
}

// validTile verifies z/x/y parsed from the numeric regex are sane.
func validTile(z, x, y string) bool {
	zi, e1 := strconv.Atoi(z)
	xi, e2 := strconv.Atoi(x)
	yi, e3 := strconv.Atoi(y)
	return e1 == nil && e2 == nil && e3 == nil && zi >= 0 && zi <= maxZoom && xi >= 0 && yi >= 0
}

// tiandituSubdomain spreads requests across t1..t7.tianditu.gov.cn.
func tiandituSubdomain() int {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return int(time.Now().UnixNano()%7) + 1
	}
	return int(b[0]%7) + 1
}

// ---------------------------------------------------------------------------
// Upstream fetch + cache
// ---------------------------------------------------------------------------

// fetchAndCache downloads a missing tile under a per-key lock (so concurrent
// requests for the same tile only hit the upstream once), writes it to the
// disk cache and streams it back.
func (g *gateway) fetchAndCache(w http.ResponseWriter, r *http.Request, file, ext, remote string) {
	mu := g.lockFor(file)
	mu.Lock()
	defer mu.Unlock()

	// Another request may have filled the cache while we waited; the hit was
	// already counted by the caller's serveCached, so do not double-count.
	if g.serveCached(w, r, file, ext) {
		return
	}

	resp, err := g.client.Get(remote)
	g.endpointUpstream(endpointFor(r.URL.Path))
	if err != nil {
		g.upstreamErr.Add(1)
		// Never log `remote`: it embeds the Tianditu key. Error text is redacted.
		log.Printf("upstream fetch failed: %s", g.redact(err.Error()))
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		g.upstreamErr.Add(1)
		log.Printf("upstream status %d", resp.StatusCode)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, g.cfg.MaxTileBytes+1))
	if err != nil {
		g.upstreamErr.Add(1)
		log.Printf("upstream read failed: %s", g.redact(err.Error()))
		http.Error(w, "read error", http.StatusBadGateway)
		return
	}
	if int64(len(body)) > g.cfg.MaxTileBytes {
		g.upstreamErr.Add(1)
		http.Error(w, "tile too large", http.StatusBadGateway)
		return
	}
	g.upstreamOK.Add(1)

	g.cacheWrite(file, body)
	w.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), contentTypeFor(ext)))
	w.Header().Set("Cache-Control", cacheControlHeader)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (g *gateway) lockFor(key string) *sync.Mutex {
	g.fetchMu.Lock()
	defer g.fetchMu.Unlock()
	m, ok := g.fetchLocks[key]
	if !ok {
		m = &sync.Mutex{}
		g.fetchLocks[key] = m
	}
	return m
}

// serveCached serves the tile from disk if present; returns false on a miss.
func (g *gateway) serveCached(w http.ResponseWriter, r *http.Request, file, ext string) bool {
	fi, err := os.Stat(file)
	if err != nil || fi.Size() == 0 {
		return false
	}
	g.serveFile(w, r, file, ext, fi.ModTime())
	return true
}

// serveFile streams a cached tile with correct headers; Content-Type is
// sniffed from the bytes (a sat tile cached as .png may still hold JPEG data).
func (g *gateway) serveFile(w http.ResponseWriter, r *http.Request, file, ext string, modTime time.Time) {
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "cache read error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	head := make([]byte, 512)
	n, _ := f.Read(head)
	ct := http.DetectContentType(head[:n])
	if ct == "application/octet-stream" || ct == "text/plain; charset=utf-8" {
		ct = contentTypeFor(ext)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "cache read error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", cacheControlHeader)
	http.ServeContent(w, r, filepath.Base(file), modTime, f)
}

func contentTypeFor(ext string) string {
	switch ext {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// cacheWrite stores a tile atomically (tmp + rename) and enforces the size cap.
func (g *gateway) cacheWrite(file string, body []byte) {
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		log.Printf("cache mkdir failed: %v", err)
		return
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		log.Printf("cache write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		log.Printf("cache rename failed: %v", err)
		return
	}
	g.cacheSize += int64(len(body))
	if g.cacheSize > g.cfg.CacheMax {
		g.evictLocked()
	}
}

// evictLocked removes the oldest files (by mtime) until the cache shrinks to
// EvictWatermark * CacheMax. Same oldest-first semantics as the webapp.
func (g *gateway) evictLocked() {
	type fi struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fi
	filepath.Walk(g.cfg.CacheDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, fi{p, info.Size(), info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	target := int64(float64(g.cfg.CacheMax) * g.cfg.EvictWatermark)
	for _, f := range files {
		if g.cacheSize <= target {
			break
		}
		if os.Remove(f.path) == nil {
			g.cacheSize -= f.size
		}
	}
}

// ---------------------------------------------------------------------------
// Rate limiting (shared counter across all tile endpoints, like webapp)
// ---------------------------------------------------------------------------

// tileRateAllow enforces TileRate requests/minute per client IP.
// IP resolution: X-Real-IP (set by nginx from real_ip-resolved $remote_addr),
// then the first X-Forwarded-For entry, then the peer address.
func (g *gateway) tileRateAllow(r *http.Request) bool {
	ip := clientIP(r)
	now := time.Now()
	g.rlMu.Lock()
	defer g.rlMu.Unlock()
	if len(g.rlCounts) > 2000 {
		g.rlCounts = map[string]*rlRec{} // simple anti-growth reset
	}
	rec, ok := g.rlCounts[ip]
	if !ok || now.Sub(rec.window) > time.Minute {
		g.rlCounts[ip] = &rlRec{window: now, count: 1}
		return true
	}
	rec.count++
	return rec.count <= g.cfg.TileRate
}

func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------------------
// Admin dashboard (login + stats)
// ---------------------------------------------------------------------------

// recordRequest updates live counters for a tile request (or any request
// fed through the tile path). Called exactly once per request.
func (g *gateway) recordRequest(path string, status int, bytes int64, dur time.Duration, ep, referer string) {
	g.reqTotal.Add(1)
	g.bytesOut.Add(bytes)

	if ep == "" {
		ep = endpointFor(path)
	}
	app := appFor(referer)
	err := status >= 400

	now := time.Now()
	g.statsMu.Lock()
	st := g.byEndpoint[ep]
	if st == nil {
		st = &epStat{}
		g.byEndpoint[ep] = st
	}
	st.Count++
	st.Bytes += bytes
	if err {
		st.Errors++
	}

	as := g.byApp[app]
	if as == nil {
		as = &epStat{}
		g.byApp[app] = as
	}
	as.Count++
	as.Bytes += bytes
	if err {
		as.Errors++
	}
	if err && dur > 0 {
		g.recentErr = append(g.recentErr, errRec{
			Time:   now.Format("15:04:05"),
			Method: "GET",
			Path:   path,
			Status: status,
			DurMS:  dur.Milliseconds(),
		})
		if len(g.recentErr) > 50 {
			g.recentErr = g.recentErr[len(g.recentErr)-50:]
		}
	}
	g.appendMinute(now, err)
	g.statsMu.Unlock()
}

// endpointHit / endpointUpstream update the per-endpoint detail counters
// (cacheHits / upstreamFetches columns in the dashboard). They are called
// from the tile handlers at the moment a cache hit or an upstream fetch
// actually happens.
func (g *gateway) endpointHit(ep string) {
	g.statsMu.Lock()
	st := g.byEndpoint[ep]
	if st == nil {
		st = &epStat{}
		g.byEndpoint[ep] = st
	}
	st.CacheHit++
	g.statsMu.Unlock()
}

func (g *gateway) endpointUpstream(ep string) {
	g.statsMu.Lock()
	st := g.byEndpoint[ep]
	if st == nil {
		st = &epStat{}
		g.byEndpoint[ep] = st
	}
	st.Upstream++
	g.statsMu.Unlock()
}

// appFor classifies a request by its Referer into a stable application name.
// Used by the dashboard to show which app the tile traffic comes from.
func appFor(referer string) string {
	if referer == "" {
		return "直连/无来源"
	}
	r := strings.ToLower(referer)
	switch {
	case strings.Contains(r, "kmlguru"):
		return "KMLGuru"
	case strings.Contains(r, "3dkml") || strings.Contains(r, "kml3d"):
		return "KML3D"
	case strings.Contains(r, "x.zaitu.cn"):
		return "本站页面"
	default:
		return "其他"
	}
}

// endpointFor buckets a tile path into a stable endpoint name.
func endpointFor(path string) string {
	switch {
	case strings.HasPrefix(path, "/tile/terrain/"):
		return "tile/terrain"
	case strings.HasPrefix(path, "/tile/sat/"):
		return "tile/sat"
	case strings.HasPrefix(path, "/tianditu/vec/"):
		return "tianditu/vec"
	case strings.HasPrefix(path, "/tianditu/cva/"):
		return "tianditu/cva"
	case strings.HasPrefix(path, "/tianditu/img/"):
		return "tianditu/img"
	case strings.HasPrefix(path, "/tianditu/cia/"):
		return "tianditu/cia"
	case path == "/geocode/reverse":
		return "geocode/reverse"
	default:
		return "other"
	}
}

// appendMinute appends/tallies per-minute buckets, keeping at most
// adminMinuteHistory buckets (rolls over, oldest dropped).
func (g *gateway) appendMinute(now time.Time, isErr bool) {
	key := now.Format("15:04")
	if g.lastMinute != key {
		g.lastMinute = key
		g.minuteHistory = append(g.minuteHistory, minuteRec{Time: key})
		if len(g.minuteHistory) > adminMinuteHistory {
			g.minuteHistory = g.minuteHistory[len(g.minuteHistory)-adminMinuteHistory:]
		}
	}
	last := &g.minuteHistory[len(g.minuteHistory)-1]
	last.Count++
	if isErr {
		last.Errors++
	}
}

// cacheBreakdown walks the cache tree and returns per-directory file/size stats.
type cacheStat struct {
	Label string  `json:"label"`
	Count int     `json:"count"`
	MB    float64 `json:"mb"`
}

func (g *gateway) cacheBreakdown() []cacheStat {
	var out []cacheStat
	accum := map[string]*cacheStat{}
	order := []string{}

	filepath.Walk(g.cfg.CacheDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(g.cfg.CacheDir, p)
		if rerr != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		var label string
		switch len(parts) {
		case 5: // tianditu/{layer}/{z}/{x}/{y}.png
			label = "tianditu/" + parts[1]
		case 4: // {kind}/{z}/{x}/{y}.png
			label = parts[0]
		default:
			return nil
		}
		st, ok := accum[label]
		if !ok {
			st = &cacheStat{Label: label}
			accum[label] = st
			order = append(order, label)
		}
		st.Count++
		st.MB += float64(info.Size()) / 1e6
		return nil
	})

	for _, l := range order {
		st := accum[l]
		st.MB = mathRound2(st.MB)
		out = append(out, *st)
	}
	return out
}

func mathRound2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// snapshot builds a point-in-time stats payload.
func (g *gateway) snapshot() statsSnapshot {
	g.statsMu.Lock()
	ep := make(map[string]*epStat, len(g.byEndpoint))
	for k, v := range g.byEndpoint {
		cp := *v
		ep[k] = &cp
	}
	ap := make(map[string]*epStat, len(g.byApp))
	for k, v := range g.byApp {
		cp := *v
		ap[k] = &cp
	}
	rerr := make([]errRec, len(g.recentErr))
	copy(rerr, g.recentErr)
	hist := make([]minuteRec, len(g.minuteHistory))
	copy(hist, g.minuteHistory)
	g.statsMu.Unlock()

	return statsSnapshot{
		Start:       g.statsStart,
		Total:       g.reqTotal.Load(),
		CacheHit:    g.cacheHit.Load(),
		CacheMiss:   g.cacheMiss.Load(),
		UpstreamOK:  g.upstreamOK.Load(),
		UpstreamErr: g.upstreamErr.Load(),
		RateLimited: g.rateLimited.Load(),
		BytesOut:    g.bytesOut.Load(),
		ByEndpoint:  ep,
		ByApp:       ap,
		RecentErr:   rerr,
		MinuteHist:  hist,
	}
}

// ---------------------------------------------------------------------------
// Admin login / sessions
// ---------------------------------------------------------------------------

func (g *gateway) sessionValid(token string) bool {
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	s, ok := g.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(s.Expiry) {
		delete(g.sessions, token)
		return false
	}
	return true
}

func (g *gateway) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)

	// Accept both urlencoded (curl/tests) and multipart/form-data (browser
	// FormData). ParseMultipartForm fills r.PostForm for multipart bodies and
	// is a no-op/error for others (ignored); ParseForm then merges urlencoded.
	_ = r.ParseMultipartForm(1 << 20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user := r.PostFormValue("user")
	pass := r.PostFormValue("pass")

	// Lockout: only after adminLockThreshold consecutive failures does this
	// IP get blocked briefly. The fail record is reset on a successful login.
	g.sessMu.Lock()
	fr := g.fails[ip]
	if fr.Count >= adminLockThreshold && time.Now().Before(fr.Expires) {
		g.sessMu.Unlock()
		log.Printf("admin login locked out from %s", ip)
		http.Error(w, "LOCKED", http.StatusTooManyRequests)
		return
	}
	g.sessMu.Unlock()

	if pass == "" {
		log.Printf("admin login empty password from %s", ip)
		http.Error(w, "CR_EMPTY", http.StatusUnauthorized)
		return
	}

	// Password-only check (username is decorative, like the removed webapp).
	expected := []byte(g.cfg.AdminPass)
	provided := []byte(pass)
	ok := subtle.ConstantTimeCompare(expected, provided) == 1 && len(expected) == len(provided)

	if !ok {
		g.sessMu.Lock()
		fr = g.fails[ip]
		fr.Count++
		if fr.Count >= adminLockThreshold {
			fr.Expires = time.Now().Add(adminLockDuration)
		}
		g.fails[ip] = fr
		g.sessMu.Unlock()
		log.Printf("admin login failed for user=%q from %s", user, ip)
		http.Error(w, "CR_PASSWORD", http.StatusUnauthorized)
		return
	}

	g.sessMu.Lock()
	delete(g.fails, ip)
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err == nil {
		token := hex.EncodeToString(tok)
		g.sessions[token] = adminSession{Token: token, Expiry: time.Now().Add(adminSessionTTL)}
		http.SetCookie(w, &http.Cookie{
			Name:     adminCookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(adminSessionTTL.Seconds()),
		})
		g.sessMu.Unlock()
		log.Printf("admin login ok from %s", ip)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
		return
	}
	g.sessMu.Unlock()
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (g *gateway) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(adminCookieName)
	if err == nil {
		g.sessMu.Lock()
		delete(g.sessions, c.Value)
		g.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func (g *gateway) authorized(r *http.Request) bool {
	c, err := r.Cookie(adminCookieName)
	if err != nil {
		return false
	}
	return g.sessionValid(c.Value)
}

func (g *gateway) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var page string
	if g.authorized(r) {
		page = adminDashboardPage
	} else {
		page = adminLoginPage
	}
	fmt.Fprint(w, page)
}

func (g *gateway) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	type payload struct {
		Stats   statsSnapshot `json:"stats"`
		Cache   []cacheStat   `json:"cacheBreakdown"`
		CacheMB float64       `json:"cacheSizeMB"`
		Version string        `json:"version"`
	}
	g.cacheMu.Lock()
	cacheSizeMB := mathRound2(float64(g.cacheSize) / 1e6)
	g.cacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(payload{
		Stats:   g.snapshot(),
		Cache:   g.cacheBreakdown(),
		CacheMB: cacheSizeMB,
		Version: "map-gateway v1",
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// redact strips the configured Tianditu key from any string before logging.
func (g *gateway) redact(s string) string {
	if k := g.cfg.TiandituKey; k != "" {
		s = strings.ReplaceAll(s, k, "REDACTED")
	}
	return s
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := configFromEnv()
	g := newGateway(cfg)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("map-gateway listening on %s (cache=%s, max=%dMB, rate=%d/min/IP, tianditu-key=%t)",
		cfg.ListenAddr, cfg.CacheDir, cfg.CacheMax>>20, cfg.TileRate, cfg.TiandituKey != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Admin pages (self-contained, no external dependencies, zh-CN)
// ---------------------------------------------------------------------------

const adminLoginPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>地图网关 - 登录</title>
<style>
  :root { --bg:#0f1420; --card:#1a2233; --line:#2a3550; --txt:#e6ecf5; --dim:#8b96ab; --acc:#4f8cff; }
  * { box-sizing:border-box; margin:0; padding:0; }
  body { background:var(--bg); color:var(--txt); font:16px/1.6 system-ui,sans-serif; min-height:100vh; display:flex; align-items:center; justify-content:center; }
  .card { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:40px 36px; width:340px; box-shadow:0 10px 40px rgba(0,0,0,.4); }
  h1 { font-size:22px; margin-bottom:6px; }
  .sub { color:var(--dim); font-size:13px; margin-bottom:28px; }
  label { display:block; font-size:13px; color:var(--dim); margin:14px 0 6px; }
  input { width:100%; padding:10px 12px; border-radius:8px; border:1px solid var(--line); background:#0f1420; color:var(--txt); font-size:15px; }
  input:focus { outline:none; border-color:var(--acc); }
  button { width:100%; margin-top:24px; padding:11px; border:0; border-radius:8px; background:var(--acc); color:#fff; font-size:15px; font-weight:600; cursor:pointer; }
  button:hover { filter:brightness(1.1); }
  .err { margin-top:14px; color:#ff6b6b; font-size:13px; display:none; }
</style>
</head>
<body>
  <div class="card">
    <h1>地图网关统计</h1>
    <div class="sub">登录后查看瓦片代理运行统计</div>
    <form id="f" method="post">
      <label for="user">用户名</label>
      <input id="user" name="user" autocomplete="username" placeholder="admin">
      <label for="pass">密码</label>
      <input id="pass" name="pass" type="password" autocomplete="current-password" placeholder="请输入密码">
      <button type="submit">登 录</button>
      <div class="err" id="err"></div>
    </form>
  </div>
<script>
var base = location.pathname;
if (base.charAt(base.length-1) !== '/') { base += '/'; }
document.getElementById('f').addEventListener('submit', function (e) {
  e.preventDefault();
  var fd = new FormData(this);
  fetch(base + 'login', { method: 'POST', body: fd, redirect: 'follow' })
    .then(function (r) {
      if (r.ok) { location.reload(); }
      else { return r.text(); }
    })
    .then(function (t) {
      if (!t) return;
      var el = document.getElementById('err');
      var msg;
      if (t === 'LOCKED') {
        msg = '尝试次数过多，请 60 秒后重试';
      } else if (t === 'CR_EMPTY') {
        msg = '密码为空，请重新输入';
      } else if (t === 'CR_PASSWORD') {
        msg = '密码错误';
      } else {
        msg = '登录失败（' + t + '）';
      }
      el.style.display = 'block';
      el.textContent = msg;
    })
    .catch(function () { location.reload(); });
});
</script>
</body>
</html>`

const adminDashboardPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>地图网关 - 统计</title>
<style>
  :root { --bg:#0f1420; --card:#1a2233; --line:#2a3550; --txt:#e6ecf5; --dim:#8b96ab; --acc:#4f8cff; --ok:#3fb96f; --warn:#e8a33d; --bad:#ff6b6b; }
  * { box-sizing:border-box; margin:0; padding:0; }
  body { background:var(--bg); color:var(--txt); font:14px/1.6 system-ui,sans-serif; padding:24px; }
  h1 { font-size:20px; }
  .top { display:flex; justify-content:space-between; align-items:center; margin-bottom:20px; flex-wrap:wrap; gap:10px; }
  .meta { color:var(--dim); font-size:13px; }
  .cards { display:grid; grid-template-columns:repeat(auto-fit,minmax(180px,1fr)); gap:14px; margin-bottom:20px; }
  .card { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:16px; }
  .card .n { font-size:26px; font-weight:700; }
  .card .l { color:var(--dim); font-size:12px; margin-top:2px; }
  .card.ok .n{color:var(--ok)} .card.warn .n{color:var(--warn)} .card.bad .n{color:var(--bad)}
  .panel { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:16px; margin-bottom:20px; }
  .panel h2 { font-size:15px; margin-bottom:12px; color:var(--dim); }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th,td { text-align:left; padding:7px 10px; border-bottom:1px solid var(--line); }
  th { color:var(--dim); font-weight:500; }
  .btn { float:right; padding:6px 14px; border-radius:6px; border:1px solid var(--line); background:transparent; color:var(--dim); cursor:pointer; font-size:13px; }
  .btn:hover { color:var(--txt); }
  svg { width:100%; height:auto; background:#0f1420; border-radius:8px; }
</style>
</head>
<body>
<div class="top">
  <h1>地图网关 · 运行统计</h1>
  <form id="logoutf" method="post" action="logout"><button class="btn" type="submit">退出登录</button></form>
</div>
<div class="meta">版本 <span id="ver">-</span> · 启动时间 <span id="start">-</span> · 运行时长 <span id="uptime">-</span> · 缓存上限 <span id="cap">-</span> · 刷新间隔 5s</div>

<div class="cards">
  <div class="card"><div class="n" id="c_total">0</div><div class="l">总请求数</div></div>
  <div class="card ok"><div class="n" id="c_hitrate">0%</div><div class="l">缓存命中率</div></div>
  <div class="card"><div class="n" id="c_cachemb">0</div><div class="l">缓存大小 (MB)</div></div>
  <div class="card warn"><div class="n" id="c_rate">0</div><div class="l">被限流请求</div></div>
  <div class="card bad"><div class="n" id="c_err">0</div><div class="l">上游错误</div></div>
</div>

<div class="panel">
  <h2>最近 60 分钟请求趋势（蓝色=请求数，红色=错误数）</h2>
  <svg id="chart" viewBox="0 0 600 180" preserveAspectRatio="none"></svg>
</div>

<div class="panel">
  <h2>按应用来源</h2>
  <table>
    <thead><tr><th>应用</th><th>请求数</th><th>出流量</th><th>错误</th></tr></thead>
    <tbody id="app_body"></tbody>
  </table>
</div>

<div class="panel">
  <h2>按端点统计</h2>
  <table>
    <thead><tr><th>端点</th><th>请求数</th><th>缓存命中</th><th>上游拉取</th><th>错误</th><th>出流量</th></tr></thead>
    <tbody id="ep_body"></tbody>
  </table>
</div>

<div class="panel">
  <h2>缓存分层明细</h2>
  <table>
    <thead><tr><th>层级</th><th>文件数</th><th>大小 (MB)</th></tr></thead>
    <tbody id="cache_body"></tbody>
  </table>
</div>

<div class="panel">
  <h2>最近错误（50 条）</h2>
  <table>
    <thead><tr><th>时间</th><th>状态</th><th>耗时</th><th>路径</th></tr></thead>
    <tbody id="err_body"></tbody>
  </table>
</div>

<script>
function esc(s){return String(s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];});}
function fmtMB(b){return b>=1e6?(b/1e6).toFixed(1):b+'B';}
function fmtDur(ms){if(ms>=1000)return (ms/1000).toFixed(1)+'s';return ms+'ms';}
function draw(data){
  var hist = data.stats.minuteHistory || [];
  var max = 1;
  hist.forEach(function(m){ if(m.count>max) max=m.count; });
  var W=600,H=180,P=8,n=hist.length;
  var s='<line x1="'+P+'" y1="'+(H-P)+'" x2="'+(W-P)+'" y2="'+(H-P)+'" stroke="#2a3550"/>';
  var step=n>1?(W-2*P)/(n-1):0;
  var pts=[],ptsE=[];
  hist.forEach(function(m,i){
    var x=(i===0?P:(P+i*step)), y=(H-P)-(m.count/max)*(H-2*P);
    pts.push(x.toFixed(1)+','+y.toFixed(1));
    var ye=(H-P)-(m.errors/max)*(H-2*P);
    ptsE.push(x.toFixed(1)+','+ye.toFixed(1));
  });
  if(pts.length){ s+='<polyline points="'+pts.join(' ')+'" fill="none" stroke="#4f8cff" stroke-width="2"/>';
    s+='<polyline points="'+ptsE.join(' ')+'" fill="none" stroke="#ff6b6b" stroke-width="2"/>'; }
  if(hist.length){ s+='<text x="'+P+'" y="14" fill="#8b96ab" font-size="10">min='+hist[0].time+' · max='+max+'</text>'; }
  document.getElementById('chart').innerHTML=s;
}
document.getElementById('logoutf').addEventListener('submit', function (e) {
  e.preventDefault();
  var base = location.pathname;
  if (base.charAt(base.length-1) !== '/') { base += '/'; }
  fetch(base + 'logout', { method: 'POST' }).then(function(){ location.reload(); });
});
function refresh(){
  var base = location.pathname;
  if (base.charAt(base.length-1) !== '/') { base += '/'; }
  fetch(base + 'api/stats').then(function(r){ return r.json(); }).then(function(d){
    var s=d.stats||{};
    document.getElementById('ver').textContent=d.version||'-';
    document.getElementById('start').textContent=(s.start||'').replace('T',' ').replace('Z','');
    var up=Math.max(0,(Date.now()-new Date(s.start).getTime())/1000);
    var h=Math.floor(up/3600),m=Math.floor(up%3600/60),sec=Math.floor(up%60);
    document.getElementById('uptime').textContent=h+'小时'+m+'分'+sec+'秒';
    document.getElementById('cap').textContent=(d.cacheSizeMB? '动态':'') ;
    document.getElementById('c_total').textContent=s.totalRequests||0;
    var tot=(s.cacheHits||0)+(s.cacheMisses||0);
    document.getElementById('c_hitrate').textContent= tot? Math.round(100*(s.cacheHits||0)/tot)+'%' : '0%';
    document.getElementById('c_cachemb').textContent=d.cacheSizeMB||0;
    document.getElementById('c_rate').textContent=s.rateLimited||0;
    document.getElementById('c_err').textContent=s.upstreamErrors||0;
    draw(d);
    var ab=document.getElementById('app_body'); ab.innerHTML='';
    var apps=s.byApp||{};
    Object.keys(apps).sort().forEach(function(k){
      var v=apps[k];
      var tr=document.createElement('tr');
      [k, v.count, fmtMB(v.bytes), v.errors].forEach(function(t){
        var td=document.createElement('td'); td.textContent=t; tr.appendChild(td);
      });
      ab.appendChild(tr);
    });
    var ep=document.getElementById('ep_body'); ep.innerHTML='';
    var eps=s.byEndpoint||{};
    Object.keys(eps).sort().forEach(function(k){
      var v=eps[k];
      var tr=document.createElement('tr');
      [k, v.count, v.cacheHits, v.upstreamFetches, v.errors, fmtMB(v.bytes)].forEach(function(t){
        var td=document.createElement('td'); td.textContent=t; tr.appendChild(td);
      });
      ep.appendChild(tr);
    });
    var cb=document.getElementById('cache_body'); cb.innerHTML='';
    (d.cacheBreakdown||[]).forEach(function(c){
      var tr=document.createElement('tr');
      [c.label, c.count, c.mb].forEach(function(t){ var td=document.createElement('td'); td.textContent=t; tr.appendChild(td); });
      cb.appendChild(tr);
    });
    var eb=document.getElementById('err_body'); eb.innerHTML='';
    (s.recentErrors||[]).slice().reverse().forEach(function(e){
      var tr=document.createElement('tr');
      [e.time, e.status, fmtDur(e.durMs), e.path].forEach(function(t){ var td=document.createElement('td'); td.textContent=t; tr.appendChild(td); });
      eb.appendChild(tr);
    });
  }).catch(function(){});
}
refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`
