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
	"crypto/sha256"
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
	defaultAdminDivisions  = "data/admin-divisions.tsv"
	adminDivisionsSHA256   = "b7e7570618b24bfc542ada6d64c6550e8453626d640cc4236092bbea35048267"
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
	searchUpstream   = "https://api.tianditu.gov.cn/v2/search"
)

// Strictly numeric, length-bounded path components: z up to 2 digits,
// x/y up to 10 digits, so no "..", separators or anything else can pass.
var (
	tileURLRe     = regexp.MustCompile(`^/tile/(terrain|sat)/([0-9]{1,2})/([0-9]{1,10})/([0-9]{1,10})\.(png|jpg)$`)
	tiandituURLRe = regexp.MustCompile(`^/tianditu/(vec|cva|img|cia)/([0-9]{1,2})/([0-9]{1,10})/([0-9]{1,10})\.png$`)
	adminCodeRe   = regexp.MustCompile(`^[0-9]+$`)
)

// ---------------------------------------------------------------------------
// Configuration (environment only)
// ---------------------------------------------------------------------------

type config struct {
	ListenAddr           string
	CacheDir             string
	CacheMax             int64
	EvictWatermark       float64
	TileRate             int
	UpstreamTimeout      time.Duration
	MaxTileBytes         int64
	TiandituKey          string
	EnvFile              string
	AdminDivisionsFile   string
	AdminDivisionsSHA256 string
	// Admin dashboard (fixed credential).
	AdminUser string
	AdminPass string
	// Upstream base URLs, overridable mainly for tests / mirrors.
	TerrainUpstream  string
	SatUpstream      string
	TiandituUpstream string
	GeocoderUpstream string
	SearchUpstream   string
}

func defaultConfig() config {
	return config{
		ListenAddr:           defaultListenAddr,
		CacheDir:             "cache",
		CacheMax:             int64(defaultCacheMaxMB) << 20,
		EvictWatermark:       defaultEvictWatermark,
		TileRate:             defaultTileRate,
		UpstreamTimeout:      defaultUpstreamTimeout,
		MaxTileBytes:         defaultMaxTileBytes,
		EnvFile:              ".env",
		AdminDivisionsFile:   defaultAdminDivisions,
		AdminDivisionsSHA256: adminDivisionsSHA256,
		AdminUser:            defaultAdminUser,
		AdminPass:            defaultAdminPass,
		TerrainUpstream:      terrainUpstream,
		SatUpstream:          satUpstream,
		TiandituUpstream:     tiandituUpstream,
		GeocoderUpstream:     geocoderUpstream,
		SearchUpstream:       searchUpstream,
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
	cfg.AdminDivisionsFile = firstNonEmpty(get("MAP_ADMIN_DIVISIONS_FILE"), cfg.AdminDivisionsFile)
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
	cfg.SearchUpstream = firstNonEmpty(get("MAP_SEARCH_UPSTREAM"), cfg.SearchUpstream)
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
	cfg               config
	client            *http.Client
	adminDivisions    map[string]adminDivisionRecord
	adminDivisionsErr error

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
	adminDivisions, adminDivisionsErr := loadAdminDivisions(cfg.AdminDivisionsFile, cfg.AdminDivisionsSHA256)
	g := &gateway{
		cfg:               cfg,
		client:            &http.Client{Timeout: cfg.UpstreamTimeout},
		adminDivisions:    adminDivisions,
		adminDivisionsErr: adminDivisionsErr,
		rlCounts:          map[string]*rlRec{},
		fetchLocks:        map[string]*sync.Mutex{},
		statsStart:        time.Now(),
		byEndpoint:        map[string]*epStat{},
		byApp:             map[string]*epStat{},
		sessions:          map[string]adminSession{},
		fails:             map[string]failRec{},
	}
	if adminDivisionsErr != nil {
		log.Printf("administrative catalog unavailable: %v", adminDivisionsErr)
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
	case path == "/search/administrative":
		g.handleAdministrativeSearch(rec, r)
	case path == "/search/nearby":
		g.handleNearbySearch(rec, r)
	case path == "/resolve/candidates":
		g.handleCandidateResolution(rec, r)
	case strings.HasPrefix(path, "/tile/"):
		g.handleTile(rec, r)
	case strings.HasPrefix(path, "/tianditu/"):
		g.handleTianditu(rec, r)
	default:
		http.NotFound(rec, r)
	}
	// Admin routes are recorded inside their handlers; tiles are recorded here.
	if strings.HasPrefix(path, "/tile/") || strings.HasPrefix(path, "/tianditu/") || path == "/geocode/reverse" || path == "/search/administrative" || path == "/search/nearby" || path == "/resolve/candidates" {
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
	Level     string `json:"level"`
	Available bool   `json:"available"`
	Name      string `json:"name"`
	Code      string `json:"code"`
}

type adminDivisionRecord struct {
	Code   string
	Name   string
	Level  int
	Parent string
}

type reverseGeocodeResponse struct {
	SchemaVersion string `json:"schemaVersion"`
	Authority     struct {
		Provider   string `json:"provider"`
		CodeSystem string `json:"codeSystem"`
	} `json:"authority"`
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
	ResolvedLevel string `json:"resolvedLevel"`
	Municipality  bool   `json:"municipality"`
	Source        string `json:"source"`
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
	lon, lat, ok := requestCoordinates(w, r, map[string]bool{"lon": true, "lat": true})
	if !ok || !g.queryAllowed(w, r, true) {
		return
	}
	g.serveQueryJSON(w, r, "geocode/reverse", coordinateCacheKey(lon, lat), 30*24*time.Hour, func() (interface{}, int, error) {
		return g.reverseGeocode(lon, lat)
	})
}

func (g *gateway) queryAllowed(w http.ResponseWriter, r *http.Request, requiresCatalog bool) bool {
	if !g.tileRateAllow(r) {
		g.rateLimited.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return false
	}
	if g.cfg.TiandituKey == "" {
		http.Error(w, "tianditu key not configured", http.StatusServiceUnavailable)
		return false
	}
	if requiresCatalog && g.adminDivisionsErr != nil {
		http.Error(w, "administrative catalog unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func requestCoordinates(w http.ResponseWriter, r *http.Request, allowed map[string]bool) (float64, float64, bool) {
	q := r.URL.Query()
	for key, values := range q {
		if !allowed[key] || len(values) != 1 {
			http.Error(w, "invalid query parameters", http.StatusBadRequest)
			return 0, 0, false
		}
	}
	if len(q["lon"]) != 1 || len(q["lat"]) != 1 {
		http.Error(w, "lon and lat are required", http.StatusBadRequest)
		return 0, 0, false
	}
	lon, errLon := strconv.ParseFloat(q.Get("lon"), 64)
	lat, errLat := strconv.ParseFloat(q.Get("lat"), 64)
	if errLon != nil || errLat != nil || math.IsNaN(lon) || math.IsNaN(lat) || math.IsInf(lon, 0) || math.IsInf(lat, 0) || lon < -180 || lon > 180 || lat < -90 || lat > 90 {
		http.Error(w, "invalid lon or lat", http.StatusBadRequest)
		return 0, 0, false
	}
	return lon, lat, true
}

func (g *gateway) reverseGeocode(lon, lat float64) (interface{}, int, error) {
	postStr, _ := json.Marshal(map[string]interface{}{"lon": lon, "lat": lat, "ver": 1})
	upstream, err := url.Parse(g.cfg.GeocoderUpstream)
	if err != nil {
		return nil, http.StatusBadGateway, errors.New("geocoder unavailable")
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
		return nil, http.StatusBadGateway, errors.New("geocoder unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		g.upstreamErr.Add(1)
		log.Printf("geocoder upstream status %d", resp.StatusCode)
		return nil, http.StatusBadGateway, errors.New("geocoder unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		g.upstreamErr.Add(1)
		return nil, http.StatusBadGateway, errors.New("invalid geocoder response")
	}
	var raw tiandituGeocoderResponse
	if json.Unmarshal(body, &raw) != nil || raw.Status != "0" {
		g.upstreamErr.Add(1)
		return nil, http.StatusNotFound, errors.New("geocoder returned no result")
	}

	ac := raw.Result.AddressComponent
	country, err := makeAdministrativeDivision("country", ac.Nation, "156", 3)
	if err != nil {
		g.upstreamErr.Add(1)
		return nil, http.StatusBadGateway, errors.New("incomplete geocoder response")
	}
	province, err := g.catalogAdministrativeDivision("province", ac.Province, ac.ProvinceCode, 1)
	if err != nil {
		g.upstreamErr.Add(1)
		return nil, http.StatusBadGateway, errors.New("incomplete geocoder response")
	}
	city, err := g.catalogAdministrativeDivision("city", ac.City, ac.CityCode, 2)
	if err != nil {
		g.upstreamErr.Add(1)
		return nil, http.StatusBadGateway, errors.New("incomplete geocoder response")
	}
	county, err := g.catalogAdministrativeDivision("county", ac.County, ac.CountyCode, 3)
	if err != nil {
		g.upstreamErr.Add(1)
		return nil, http.StatusBadGateway, errors.New("incomplete geocoder response")
	}
	town, err := g.catalogAdministrativeDivision("town", ac.Town, ac.TownCode, 4)
	if err != nil || !province.Available || !county.Available || !g.validCatalogHierarchy(province, city, county, town) {
		g.upstreamErr.Add(1)
		return nil, http.StatusBadGateway, errors.New("incomplete geocoder response")
	}
	village, _ := makeAdministrativeDivision("village", "", "", 0)

	var out reverseGeocodeResponse
	out.SchemaVersion = "1.0"
	out.Authority.Provider = "map-gateway"
	out.Authority.CodeSystem = "china-national-geonames-level4+tianditu-prefix"
	out.Location.Lon, out.Location.Lat = lon, lat
	out.FormattedAddress = raw.Result.FormattedAddress
	out.Administrative.Country = country
	out.Administrative.Province = province
	out.Administrative.City = city
	out.Administrative.County = county
	out.Administrative.Town = town
	out.Administrative.Village = village
	out.ResolvedLevel = deepestAdministrativeLevel(province, city, county, town, village)
	out.Municipality = isMunicipality(ac.Province)
	out.Source = "tianditu"
	g.upstreamOK.Add(1)
	return out, http.StatusOK, nil
}

// queryCacheEntry stores completed, provider-normalized query responses on
// disk. It intentionally never stores request URLs, which could contain a key.
type queryCacheEntry struct {
	FetchedAt time.Time       `json:"fetched_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Payload   json.RawMessage `json:"payload"`
}

type queryCacheMetadata struct {
	State     string `json:"state"`
	FetchedAt string `json:"fetched_at"`
	ExpiresAt string `json:"expires_at"`
	Version   string `json:"version"`
}

func coordinateCacheKey(lon, lat float64) string {
	// Five decimal places is an approximately one-metre grid. It stops GPS
	// jitter from creating unbounded entries while retaining useful precision.
	return fmt.Sprintf("%.5f,%.5f", lon, lat)
}

func (g *gateway) queryCacheFile(endpoint, key string) string {
	sum := sha256.Sum256([]byte(endpoint + "\x00" + key))
	return filepath.Join(g.cfg.CacheDir, "queries", endpoint, hex.EncodeToString(sum[:])+".json")
}

func (g *gateway) readQueryCache(file string) (queryCacheEntry, bool) {
	body, err := os.ReadFile(file)
	if err != nil {
		return queryCacheEntry{}, false
	}
	var entry queryCacheEntry
	if json.Unmarshal(body, &entry) != nil || len(entry.Payload) == 0 || entry.FetchedAt.IsZero() || entry.ExpiresAt.IsZero() {
		return queryCacheEntry{}, false
	}
	return entry, true
}

func queryPayloadWithMetadata(payload []byte, state string, entry queryCacheEntry) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, err
	}
	meta, err := json.Marshal(queryCacheMetadata{
		State:     state,
		FetchedAt: entry.FetchedAt.UTC().Format(time.RFC3339),
		ExpiresAt: entry.ExpiresAt.UTC().Format(time.RFC3339),
		Version:   "1",
	})
	if err != nil {
		return nil, err
	}
	obj["cache"] = meta
	return json.Marshal(obj)
}

// serveQueryJSON is the shared cache policy for all Tianditu query endpoints.
// A fresh entry is served without an upstream call. An expired entry can be
// returned only after a refresh fails, and is explicitly marked stale.
func (g *gateway) serveQueryJSON(w http.ResponseWriter, r *http.Request, endpoint, key string, ttl time.Duration, build func() (interface{}, int, error)) {
	file := g.queryCacheFile(endpoint, key)
	entry, hasEntry := g.readQueryCache(file)
	now := time.Now()
	if hasEntry && now.Before(entry.ExpiresAt) {
		g.cacheHit.Add(1)
		g.endpointHit(endpoint)
		g.writeQueryResponse(w, entry, "hit")
		return
	}
	g.cacheMiss.Add(1)
	mu := g.lockFor("query:" + file)
	mu.Lock()
	defer mu.Unlock()
	// A concurrent request may have refreshed it while this request waited.
	entry, hasEntry = g.readQueryCache(file)
	now = time.Now()
	if hasEntry && now.Before(entry.ExpiresAt) {
		g.cacheHit.Add(1)
		g.endpointHit(endpoint)
		g.writeQueryResponse(w, entry, "hit")
		return
	}

	result, status, err := build()
	if err != nil {
		if hasEntry {
			g.cacheHit.Add(1)
			g.endpointHit(endpoint)
			g.writeQueryResponse(w, entry, "stale")
			return
		}
		http.Error(w, err.Error(), status)
		return
	}
	payload, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	entry = queryCacheEntry{FetchedAt: now.UTC(), ExpiresAt: now.UTC().Add(ttl), Payload: payload}
	encoded, err := json.Marshal(entry)
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	g.cacheWrite(file, encoded)
	g.writeQueryResponse(w, entry, "miss")
}

func (g *gateway) writeQueryResponse(w http.ResponseWriter, entry queryCacheEntry, state string) {
	body, err := queryPayloadWithMetadata(entry.Payload, state, entry)
	if err != nil {
		http.Error(w, "cached response invalid", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Map-Gateway-Cache", state)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

type queryLocation struct {
	Lon float64 `json:"lon"`
	Lat float64 `json:"lat"`
}

type administrativeCenterCandidate struct {
	Name      string `json:"name"`
	AdminCode string `json:"admin_code,omitempty"`
	Level     string `json:"level,omitempty"`
	Provider  string `json:"provider"`
	Source    string `json:"source,omitempty"`
	Center    *struct {
		Location   queryLocation `json:"location"`
		CenterType string        `json:"center_type"`
		DistanceM  *float64      `json:"distance_m,omitempty"`
	} `json:"center,omitempty"`
	Raw struct {
		Lonlat    string `json:"lonlat"`
		Bound     string `json:"bound"`
		AdminCode string `json:"adminCode"`
		Level     string `json:"level"`
	} `json:"raw"`
}

type administrativeSearchResponse struct {
	SchemaVersion string                          `json:"schema_version"`
	Provider      string                          `json:"provider"`
	Candidates    []administrativeCenterCandidate `json:"candidates"`
}

type nearbyPOICandidate struct {
	Name       string        `json:"name"`
	Location   queryLocation `json:"location"`
	DistanceM  float64       `json:"distance_m"`
	TypeCode   string        `json:"type_code,omitempty"`
	TypeName   string        `json:"type_name,omitempty"`
	Provider   string        `json:"provider"`
	Source     string        `json:"source,omitempty"`
	HotPointID string        `json:"hotPointID,omitempty"`
	SourceID   string        `json:"source_id,omitempty"`
	Province   struct {
		Name string `json:"name,omitempty"`
		Code string `json:"code,omitempty"`
	} `json:"province"`
	City struct {
		Name string `json:"name,omitempty"`
		Code string `json:"code,omitempty"`
	} `json:"city"`
	County struct {
		Name string `json:"name,omitempty"`
		Code string `json:"code,omitempty"`
	} `json:"county"`
}

type nearbySearchResponse struct {
	SchemaVersion string               `json:"schema_version"`
	Provider      string               `json:"provider"`
	Location      queryLocation        `json:"location"`
	QueryRadiusM  float64              `json:"query_radius_m"`
	Keyword       string               `json:"keyword"`
	DataTypes     string               `json:"data_types,omitempty"`
	Candidates    []nearbyPOICandidate `json:"candidates"`
}

type tiandituSearchResult struct {
	Status struct {
		InfoCode json.RawMessage `json:"infocode"`
		Info     string          `json:"info"`
	} `json:"status"`
	Pois   []map[string]json.RawMessage `json:"pois"`
	Area   json.RawMessage              `json:"area"`
	Prompt []map[string]json.RawMessage `json:"prompt"`
}

func (g *gateway) handleAdministrativeSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !queryKeysExactly(q, map[string]bool{"keyword": true, "specify": true, "origin_lon": true, "origin_lat": true, "limit": true}) || strings.TrimSpace(q.Get("keyword")) == "" || strings.TrimSpace(q.Get("specify")) == "" {
		http.Error(w, "keyword and specify are required", http.StatusBadRequest)
		return
	}
	limit, ok := queryLimit(w, q, 20)
	if !ok || !g.queryAllowed(w, r, false) {
		return
	}
	keyword, specify := strings.TrimSpace(q.Get("keyword")), strings.TrimSpace(q.Get("specify"))
	if len([]rune(keyword)) > 100 || len([]rune(specify)) > 100 {
		http.Error(w, "keyword or specify too long", http.StatusBadRequest)
		return
	}
	origin, originOK := optionalOrigin(w, q)
	if !originOK {
		return
	}
	originKey := ""
	if origin != nil {
		originKey = coordinateCacheKey(origin.Lon, origin.Lat)
	}
	key := "tianditu|adminCode:" + specify + "|level:all|" + keyword + "|" + originKey + "|" + strconv.Itoa(limit)
	g.serveQueryJSON(w, r, "search/administrative", key, 7*24*time.Hour, func() (interface{}, int, error) {
		return g.administrativeSearch(keyword, specify, origin, limit)
	})
}

func (g *gateway) handleNearbySearch(w http.ResponseWriter, r *http.Request) {
	lon, lat, ok := requestCoordinates(w, r, map[string]bool{"lon": true, "lat": true, "radius_m": true, "keyword": true, "data_types": true, "limit": true})
	if !ok {
		return
	}
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	if keyword == "" || len([]rune(keyword)) > 100 || len(q["radius_m"]) != 1 {
		http.Error(w, "keyword and radius_m are required", http.StatusBadRequest)
		return
	}
	radius, err := strconv.ParseFloat(q.Get("radius_m"), 64)
	if err != nil || math.IsNaN(radius) || math.IsInf(radius, 0) || radius <= 0 || radius > 10000 {
		http.Error(w, "radius_m must be within 0..10000", http.StatusBadRequest)
		return
	}
	dataTypes := strings.TrimSpace(q.Get("data_types"))
	if len([]rune(dataTypes)) > 200 {
		http.Error(w, "data_types too long", http.StatusBadRequest)
		return
	}
	limit, ok := queryLimit(w, q, 20)
	if !ok || !g.queryAllowed(w, r, false) {
		return
	}
	key := "tianditu|" + coordinateCacheKey(lon, lat) + "|" + strconv.FormatFloat(radius, 'f', -1, 64) + "|" + keyword + "|" + dataTypes + "|" + strconv.Itoa(limit)
	g.serveQueryJSON(w, r, "search/nearby", key, 10*time.Minute, func() (interface{}, int, error) {
		return g.nearbySearch(lon, lat, radius, keyword, dataTypes, limit)
	})
}

type candidateResolutionResponse struct {
	SchemaVersion            string                          `json:"schema_version"`
	Provider                 string                          `json:"provider"`
	ReverseGeocode           reverseGeocodeResponse          `json:"reverse_geocode"`
	AdministrativeCandidates []administrativeCenterCandidate `json:"administrative_candidates"`
	POICandidates            []nearbyPOICandidate            `json:"poi_candidates"`
}

// handleCandidateResolution combines the existing reverse result with the
// generic center and POI candidates. It remains domain-neutral: callers
// decide whether any candidate is suitable for a display or business action.
func (g *gateway) handleCandidateResolution(w http.ResponseWriter, r *http.Request) {
	lon, lat, ok := requestCoordinates(w, r, map[string]bool{"lon": true, "lat": true, "radius_m": true, "keyword": true, "data_types": true, "limit": true})
	if !ok {
		return
	}
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	if keyword == "" || len([]rune(keyword)) > 100 || len(q["radius_m"]) != 1 {
		http.Error(w, "keyword and radius_m are required", http.StatusBadRequest)
		return
	}
	radius, err := strconv.ParseFloat(q.Get("radius_m"), 64)
	if err != nil || math.IsNaN(radius) || math.IsInf(radius, 0) || radius <= 0 || radius > 10000 {
		http.Error(w, "radius_m must be within 0..10000", http.StatusBadRequest)
		return
	}
	dataTypes := strings.TrimSpace(q.Get("data_types"))
	if len([]rune(dataTypes)) > 200 {
		http.Error(w, "data_types too long", http.StatusBadRequest)
		return
	}
	limit, ok := queryLimit(w, q, 20)
	if !ok || !g.queryAllowed(w, r, true) {
		return
	}
	key := "tianditu|center-v2|" + coordinateCacheKey(lon, lat) + "|" + strconv.FormatFloat(radius, 'f', -1, 64) + "|" + keyword + "|" + dataTypes + "|" + strconv.Itoa(limit)
	g.serveQueryJSON(w, r, "resolve/candidates", key, 10*time.Minute, func() (interface{}, int, error) {
		reverse, status, err := g.reverseGeocode(lon, lat)
		if err != nil {
			return nil, status, err
		}
		reverseResult := reverse.(reverseGeocodeResponse)
		nearby, status, err := g.nearbySearch(lon, lat, radius, keyword, dataTypes, limit)
		if err != nil {
			return nil, status, err
		}
		out := candidateResolutionResponse{
			SchemaVersion:            "1.0",
			Provider:                 "map-gateway",
			ReverseGeocode:           reverseResult,
			AdministrativeCandidates: []administrativeCenterCandidate{},
			POICandidates:            nearby.(nearbySearchResponse).Candidates,
		}
		if name, specify := preferredAdministrativeSearch(reverseResult); name != "" {
			origin := queryLocation{Lon: lon, Lat: lat}
			if centers, _, centerErr := g.administrativeSearch(name, specify, &origin, limit); centerErr == nil {
				out.AdministrativeCandidates = centers.(administrativeSearchResponse).Candidates
			}
		}
		return out, http.StatusOK, nil
	})
}

func preferredAdministrativeSearch(reverse reverseGeocodeResponse) (string, string) {
	// TDT queryType=12 accepts a 9-digit national administrative code. The
	// bundled town code is 12 digits, so use the deepest TDT-compatible level.
	for _, division := range []administrativeDivision{reverse.Administrative.County, reverse.Administrative.City, reverse.Administrative.Province} {
		if division.Available {
			return division.Name, nationalAdministrativeCode(division.Code)
		}
	}
	return "", ""
}

func nationalAdministrativeCode(code string) string {
	code = strings.TrimSpace(code)
	if strings.HasPrefix(code, "156") {
		return code
	}
	return "156" + code
}

func queryKeysExactly(q url.Values, allowed map[string]bool) bool {
	for key, values := range q {
		if !allowed[key] || len(values) != 1 {
			return false
		}
	}
	return true
}

func queryLimit(w http.ResponseWriter, q url.Values, fallback int) (int, bool) {
	if len(q["limit"]) == 0 || q.Get("limit") == "" {
		return fallback, true
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 || limit > 50 {
		http.Error(w, "limit must be within 1..50", http.StatusBadRequest)
		return 0, false
	}
	return limit, true
}

func optionalOrigin(w http.ResponseWriter, q url.Values) (*queryLocation, bool) {
	_, hasLon := q["origin_lon"]
	_, hasLat := q["origin_lat"]
	if !hasLon && !hasLat {
		return nil, true
	}
	if !hasLon || !hasLat || q.Get("origin_lon") == "" || q.Get("origin_lat") == "" {
		http.Error(w, "origin_lon and origin_lat must be provided together", http.StatusBadRequest)
		return nil, false
	}
	lon, errLon := strconv.ParseFloat(q.Get("origin_lon"), 64)
	lat, errLat := strconv.ParseFloat(q.Get("origin_lat"), 64)
	if errLon != nil || errLat != nil || math.IsNaN(lon) || math.IsNaN(lat) || math.IsInf(lon, 0) || math.IsInf(lat, 0) || lon < -180 || lon > 180 || lat < -90 || lat > 90 {
		http.Error(w, "invalid origin_lon or origin_lat", http.StatusBadRequest)
		return nil, false
	}
	return &queryLocation{Lon: lon, Lat: lat}, true
}

func (g *gateway) administrativeSearch(keyword, specify string, origin *queryLocation, limit int) (interface{}, int, error) {
	raw, status, err := g.tiandituSearch("search/administrative", map[string]interface{}{"keyWord": keyword, "queryType": 12, "start": 0, "count": limit, "specify": specify})
	if err != nil {
		return nil, status, err
	}
	var out administrativeSearchResponse
	out.SchemaVersion, out.Provider = "1.0", "tianditu"
	for _, area := range objectsFromRaw(raw.Area) {
		candidate, ok := g.administrativeCandidate(area, origin)
		if ok {
			out.Candidates = append(out.Candidates, candidate)
		}
	}
	if len(out.Candidates) == 0 {
		adminName, adminCode := searchPromptAdmin(raw.Prompt, keyword, specify)
		for _, poi := range raw.Pois {
			candidate, ok := g.administrativePOICandidate(poi, adminName, adminCode, origin)
			if ok {
				out.Candidates = append(out.Candidates, candidate)
				break
			}
		}
	}
	if out.Candidates == nil {
		out.Candidates = []administrativeCenterCandidate{}
	}
	return out, http.StatusOK, nil
}

func (g *gateway) nearbySearch(lon, lat, radius float64, keyword, dataTypes string, limit int) (interface{}, int, error) {
	post := map[string]interface{}{"keyWord": keyword, "level": 18, "queryRadius": radius, "pointLonlat": fmt.Sprintf("%.8f,%.8f", lon, lat), "queryType": 3, "start": 0, "count": limit}
	if dataTypes != "" {
		post["dataTypes"] = dataTypes
	}
	raw, status, err := g.tiandituSearch("search/nearby", post)
	if err != nil {
		return nil, status, err
	}
	out := nearbySearchResponse{SchemaVersion: "1.0", Provider: "tianditu", Location: queryLocation{Lon: lon, Lat: lat}, QueryRadiusM: radius, Keyword: keyword, DataTypes: dataTypes, Candidates: []nearbyPOICandidate{}}
	for _, poi := range raw.Pois {
		candidate, ok := nearbyCandidate(poi)
		if ok {
			out.Candidates = append(out.Candidates, candidate)
		}
	}
	return out, http.StatusOK, nil
}

func (g *gateway) tiandituSearch(endpoint string, post map[string]interface{}) (tiandituSearchResult, int, error) {
	var zero tiandituSearchResult
	postStr, _ := json.Marshal(post)
	upstream, err := url.Parse(g.cfg.SearchUpstream)
	if err != nil {
		return zero, http.StatusBadGateway, errors.New("search unavailable")
	}
	params := upstream.Query()
	params.Set("postStr", string(postStr))
	params.Set("type", "query")
	params.Set("tk", g.cfg.TiandituKey)
	upstream.RawQuery = params.Encode()
	g.endpointUpstream(endpoint)
	resp, err := g.client.Get(upstream.String())
	if err != nil {
		g.upstreamErr.Add(1)
		log.Printf("search upstream: %s", g.redact(err.Error()))
		return zero, http.StatusBadGateway, errors.New("search unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		g.upstreamErr.Add(1)
		log.Printf("search upstream status %d", resp.StatusCode)
		return zero, http.StatusBadGateway, errors.New("search unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || json.Unmarshal(body, &zero) != nil {
		g.upstreamErr.Add(1)
		return tiandituSearchResult{}, http.StatusBadGateway, errors.New("invalid search response")
	}
	if rawText(zero.Status.InfoCode) != "1000" {
		g.upstreamErr.Add(1)
		return tiandituSearchResult{}, http.StatusBadGateway, errors.New("search returned no result")
	}
	g.upstreamOK.Add(1)
	return zero, http.StatusOK, nil
}

func rawText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

func objectsFromRaw(raw json.RawMessage) []map[string]json.RawMessage {
	var objects []map[string]json.RawMessage
	if json.Unmarshal(raw, &objects) == nil {
		return objects
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		return []map[string]json.RawMessage{object}
	}
	return nil
}

func parseLonlat(value string) (queryLocation, bool) {
	parts := strings.Split(strings.TrimSpace(value), ",")
	if len(parts) != 2 {
		return queryLocation{}, false
	}
	lon, errLon := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lat, errLat := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errLon != nil || errLat != nil || lon < -180 || lon > 180 || lat < -90 || lat > 90 {
		return queryLocation{}, false
	}
	return queryLocation{Lon: lon, Lat: lat}, true
}

func (g *gateway) administrativeCandidate(raw map[string]json.RawMessage, origin *queryLocation) (administrativeCenterCandidate, bool) {
	name := rawText(raw["name"])
	if name == "" {
		return administrativeCenterCandidate{}, false
	}
	candidate := administrativeCenterCandidate{Name: name, Provider: "tianditu", Source: "tianditu.search.v2.area"}
	candidate.Raw.Lonlat = rawText(raw["lonlat"])
	candidate.Raw.Bound = rawText(raw["bound"])
	candidate.Raw.AdminCode = rawText(raw["adminCode"])
	candidate.Raw.Level = rawText(raw["level"])
	candidate.AdminCode = candidate.Raw.AdminCode
	candidate.Level = g.administrativeLevel(candidate.AdminCode)
	if location, ok := parseLonlat(candidate.Raw.Lonlat); ok {
		var distance *float64
		if origin != nil {
			d := distanceMeters(*origin, location)
			distance = &d
		}
		candidate.Center = &struct {
			Location   queryLocation `json:"location"`
			CenterType string        `json:"center_type"`
			DistanceM  *float64      `json:"distance_m,omitempty"`
		}{Location: location, CenterType: "tianditu_area_center", DistanceM: distance}
	}
	return candidate, true
}

func searchPromptAdmin(prompts []map[string]json.RawMessage, keyword, specify string) (string, string) {
	for _, prompt := range prompts {
		var admins []map[string]json.RawMessage
		if json.Unmarshal(prompt["admins"], &admins) != nil {
			continue
		}
		for _, admin := range admins {
			name, code := rawText(admin["adminName"]), rawText(admin["adminCode"])
			if code == specify || name == keyword {
				return name, code
			}
		}
	}
	return keyword, specify
}

// administrativePOICandidate is deliberately a separate center type from an
// area result. TDT commonly returns a named POI for an administrative query
// (resultType=1) rather than area.lonlat; it is useful, but is not an area
// centroid or a government-seat assertion.
func (g *gateway) administrativePOICandidate(raw map[string]json.RawMessage, adminName, adminCode string, origin *queryLocation) (administrativeCenterCandidate, bool) {
	poiName := rawText(raw["name"])
	location, ok := parseLonlat(rawText(raw["lonlat"]))
	if !ok || poiName == "" || (poiName != adminName && poiName != strings.TrimPrefix(adminName, "中国")) {
		return administrativeCenterCandidate{}, false
	}
	var distance *float64
	if origin != nil {
		d := distanceMeters(*origin, location)
		distance = &d
	}
	candidate := administrativeCenterCandidate{
		Name:      adminName,
		AdminCode: adminCode,
		Level:     g.administrativeLevel(adminCode),
		Provider:  "tianditu",
		Source:    "tianditu.search.v2.poi",
	}
	candidate.Raw.Lonlat = rawText(raw["lonlat"])
	candidate.Raw.AdminCode = adminCode
	candidate.Center = &struct {
		Location   queryLocation `json:"location"`
		CenterType string        `json:"center_type"`
		DistanceM  *float64      `json:"distance_m,omitempty"`
	}{Location: location, CenterType: "tianditu_named_poi", DistanceM: distance}
	return candidate, true
}

func (g *gateway) administrativeLevel(code string) string {
	code = strings.TrimPrefix(strings.TrimSpace(code), "156")
	if record, ok := g.adminDivisions[code]; ok {
		switch record.Level {
		case 1:
			return "province"
		case 2:
			return "city"
		case 3:
			return "county"
		case 4:
			return "town"
		}
	}
	return ""
}

func distanceMeters(a, b queryLocation) float64 {
	const earthRadiusM = 6371008.8
	toRadians := math.Pi / 180
	dLat, dLon := (b.Lat-a.Lat)*toRadians, (b.Lon-a.Lon)*toRadians
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.Lat*toRadians)*math.Cos(b.Lat*toRadians)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Atan2(math.Sqrt(h), math.Sqrt(1-h))
}

func nearbyCandidate(raw map[string]json.RawMessage) (nearbyPOICandidate, bool) {
	name := rawText(raw["name"])
	location, locationOK := parseLonlat(rawText(raw["lonlat"]))
	distance, distanceOK := parseDistanceMeters(rawText(raw["distance"]))
	if name == "" || !locationOK || !distanceOK {
		return nearbyPOICandidate{}, false
	}
	candidate := nearbyPOICandidate{Name: name, Location: location, DistanceM: distance, Provider: "tianditu"}
	candidate.TypeCode = rawText(raw["typeCode"])
	candidate.TypeName = rawText(raw["typeName"])
	candidate.Source = rawText(raw["source"])
	candidate.HotPointID = rawText(raw["hotPointID"])
	candidate.SourceID = rawText(raw["source_id"])
	if candidate.SourceID == "" {
		candidate.SourceID = rawText(raw["id"])
	}
	candidate.Province.Name, candidate.Province.Code = rawText(raw["province"]), rawText(raw["provinceCode"])
	candidate.City.Name, candidate.City.Code = rawText(raw["city"]), rawText(raw["cityCode"])
	candidate.County.Name, candidate.County.Code = rawText(raw["county"]), rawText(raw["countyCode"])
	return candidate, true
}

func parseDistanceMeters(value string) (float64, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	multiplier := 1.0
	switch {
	case strings.HasSuffix(value, "km"):
		value, multiplier = strings.TrimSpace(strings.TrimSuffix(value, "km")), 1000
	case strings.HasSuffix(value, "m"):
		value = strings.TrimSpace(strings.TrimSuffix(value, "m"))
	case strings.HasSuffix(value, "公里"):
		value, multiplier = strings.TrimSpace(strings.TrimSuffix(value, "公里")), 1000
	case strings.HasSuffix(value, "米"):
		value = strings.TrimSpace(strings.TrimSuffix(value, "米"))
	}
	distance, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(distance) || math.IsInf(distance, 0) || distance < 0 {
		return 0, false
	}
	return distance * multiplier, true
}

func makeAdministrativeDivision(level, name, code string, codeLength int) (administrativeDivision, error) {
	name, code = strings.TrimSpace(name), strings.TrimSpace(code)
	d := administrativeDivision{Level: level, Name: name, Code: code}
	if name == "" && code == "" {
		return d, nil
	}
	if name == "" || code == "" || (codeLength > 0 && len(code) != codeLength) || !adminCodeRe.MatchString(code) {
		return d, errors.New("invalid administrative division")
	}
	d.Available = true
	return d, nil
}

func loadAdminDivisions(path, expectedSHA256 string) (map[string]adminDivisionRecord, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if expectedSHA256 != "" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != expectedSHA256 {
			return nil, errors.New("administrative catalog checksum mismatch")
		}
	}
	records := make(map[string]adminDivisionRecord, 43000)
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	line := 0
	for scanner.Scan() {
		line++
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("administrative catalog line %d: expected 4 fields", line)
		}
		level, err := strconv.Atoi(fields[2])
		if err != nil || level < 1 || level > 4 {
			return nil, fmt.Errorf("administrative catalog line %d: invalid level", line)
		}
		code, name, parent := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1]), strings.TrimSpace(fields[3])
		wantLength := 6
		if level == 4 {
			wantLength = 9
		}
		if name == "" || len(code) != wantLength || !adminCodeRe.MatchString(code) {
			return nil, fmt.Errorf("administrative catalog line %d: invalid code or name", line)
		}
		if _, exists := records[code]; exists {
			return nil, fmt.Errorf("administrative catalog line %d: duplicate code", line)
		}
		records[code] = adminDivisionRecord{Code: code, Name: name, Level: level, Parent: parent}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Level == 1 {
			if record.Parent != "" {
				return nil, fmt.Errorf("administrative catalog %s: province has parent", record.Code)
			}
			continue
		}
		// Some directly administered county-level divisions use their own
		// six-digit code as pid (for example 469027 -> 469027).
		if record.Level == 3 && record.Parent == record.Code {
			continue
		}
		parent, ok := records[record.Parent]
		if !ok || parent.Level != record.Level-1 {
			return nil, fmt.Errorf("administrative catalog %s: invalid parent", record.Code)
		}
	}
	return records, nil
}

func (g *gateway) catalogAdministrativeDivision(level, upstreamName, upstreamCode string, catalogLevel int) (administrativeDivision, error) {
	upstreamName, upstreamCode = strings.TrimSpace(upstreamName), strings.TrimSpace(upstreamCode)
	if upstreamName == "" && upstreamCode == "" {
		return administrativeDivision{Level: level}, nil
	}
	wantLength := 9
	if catalogLevel == 4 {
		wantLength = 12
	}
	if _, err := makeAdministrativeDivision(level, upstreamName, upstreamCode, wantLength); err != nil || !strings.HasPrefix(upstreamCode, "156") {
		return administrativeDivision{}, errors.New("invalid upstream administrative division")
	}
	record, ok := g.adminDivisions[strings.TrimPrefix(upstreamCode, "156")]
	if !ok || record.Level != catalogLevel {
		return administrativeDivision{}, errors.New("administrative code absent from catalog")
	}
	return administrativeDivision{Level: level, Available: true, Name: record.Name, Code: "156" + record.Code}, nil
}

func (g *gateway) validCatalogHierarchy(province, city, county, town administrativeDivision) bool {
	localCode := func(d administrativeDivision) string { return strings.TrimPrefix(d.Code, "156") }
	provinceRecord := g.adminDivisions[localCode(province)]
	countyRecord := g.adminDivisions[localCode(county)]
	if city.Available {
		cityRecord := g.adminDivisions[localCode(city)]
		if cityRecord.Parent != provinceRecord.Code || countyRecord.Parent != cityRecord.Code {
			return false
		}
	} else {
		countyParent, ok := g.adminDivisions[countyRecord.Parent]
		if countyRecord.Parent == countyRecord.Code && strings.HasPrefix(countyRecord.Code, provinceRecord.Code[:2]) {
			// Directly administered county-level division.
		} else if !ok || countyParent.Parent != provinceRecord.Code {
			return false
		}
	}
	if town.Available {
		townRecord := g.adminDivisions[localCode(town)]
		if townRecord.Parent != countyRecord.Code {
			return false
		}
	}
	return true
}

func deepestAdministrativeLevel(levels ...administrativeDivision) string {
	deepest := ""
	for _, level := range levels {
		if level.Available {
			deepest = level.Level
		}
	}
	return deepest
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
	oldSize := int64(0)
	if fi, err := os.Stat(file); err == nil {
		oldSize = fi.Size()
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
	g.cacheSize += int64(len(body)) - oldSize
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
