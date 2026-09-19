package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func geocoderStub(t *testing.T, response string, status int, inspect func(*http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inspect != nil {
			inspect(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestGateway builds a gateway with an isolated cache dir and a fake key.
func newTestGateway(t *testing.T, mutate func(*config)) (*gateway, string) {
	t.Helper()
	cacheDir := t.TempDir()
	adminFile := filepath.Join(t.TempDir(), "admin-divisions.tsv")
	adminData := strings.Join([]string{
		"510000\t四川省\t1\t",
		"511400\t眉山市\t2\t510000",
		"511402\t东坡区\t3\t511400",
		"511402003\t苏祠街道\t4\t511402",
		"110000\t北京市\t1\t",
		"110100\t北京市\t2\t110000",
		"110101\t东城区\t3\t110100",
		"110101001\t东华门街道\t4\t110101",
	}, "\n") + "\n"
	if err := os.WriteFile(adminFile, []byte(adminData), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.CacheDir = cacheDir
	cfg.AdminDivisionsFile = adminFile
	cfg.AdminDivisionsSHA256 = ""
	cfg.TiandituKey = "test-key"
	cfg.TileRate = 1000 // off by default for most tests
	if mutate != nil {
		mutate(&cfg)
	}
	return newGateway(cfg), cacheDir
}

// stubUpstream returns an httptest server serving body with contentType,
// counting requests.
func stubUpstream(t *testing.T, contentType string, body []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", contentType)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func doGET(t *testing.T, g *gateway, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Health, routing, security surface
// ---------------------------------------------------------------------------

func TestHealthEndpoints(t *testing.T) {
	g, _ := newTestGateway(t, nil)
	for _, p := range []string{"/health", "/healthz"} {
		rec := doGET(t, g, p, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
			t.Fatalf("%s: got %d %q, want 200 ok", p, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("%s: content-type %q", p, ct)
		}
	}
}

func TestNoKeyEndpointAndUnknownRoutes(t *testing.T) {
	g, _ := newTestGateway(t, nil)
	for _, p := range []string{
		"/", "/api/tianditu-key", "/api/tianditu-key/", "/tianditu-key",
		"/favicon.ico", "/status", "/robots.txt",
	} {
		if rec := doGET(t, g, p, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", p, rec.Code)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	g, _ := newTestGateway(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/tile/terrain/3/6/2.png", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: got %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET" {
		t.Fatalf("Allow header: %q", rec.Header().Get("Allow"))
	}
}

func TestReverseGeocodeReturnsAllLevels(t *testing.T) {
	up := geocoderStub(t, `{"status":"0","msg":"ok","result":{"formatted_address":"四川省眉山市东坡区苏祠街道","addressComponent":{"nation":"中国","province":"四川省","province_code":"156510000","city":"眉山市","city_code":"156511400","county":"东坡区","county_code":"156511402","town":"苏祠街道","town_code":"156511402003"}}}`, http.StatusOK, func(r *http.Request) {
		if r.URL.Query().Get("tk") != "test-key" || r.URL.Query().Get("type") != "geocode" {
			t.Errorf("unexpected upstream query: %s", r.URL.RawQuery)
		}
		if !strings.Contains(r.URL.Query().Get("postStr"), `"lon":103.8343`) {
			t.Errorf("unexpected postStr: %s", r.URL.Query().Get("postStr"))
		}
	})
	g, _ := newTestGateway(t, func(c *config) { c.GeocoderUpstream = up.URL })
	rec := doGET(t, g, "/geocode/reverse?lon=103.8343&lat=30.0508", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got reverseGeocodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Administrative.Province.Name != "四川省" || got.Administrative.City.Name != "眉山市" || got.Administrative.County.Name != "东坡区" || got.Administrative.Town.Name != "苏祠街道" {
		t.Fatalf("unexpected hierarchy: %+v", got.Administrative)
	}
	if got.SchemaVersion != "1.0" || got.Authority.Provider != "map-gateway" || got.Authority.CodeSystem != "china-national-geonames-level4+tianditu-prefix" {
		t.Fatalf("unexpected contract metadata: %+v", got)
	}
	if got.ResolvedLevel != "town" || !got.Administrative.Town.Available || got.Administrative.Town.Level != "town" {
		t.Fatalf("unexpected resolution: %+v", got)
	}
	if got.Administrative.Village.Available || got.Administrative.Village.Level != "village" || got.Administrative.Village.Name != "" || got.Administrative.Village.Code != "" || got.Municipality {
		t.Fatalf("unexpected village/municipality: %+v", got)
	}
}

func TestReverseGeocodeMunicipality(t *testing.T) {
	up := geocoderStub(t, `{"status":"0","result":{"formatted_address":"北京市东城区东华门街道","addressComponent":{"nation":"中国","province":"北京市","province_code":"156110000","city":"","city_code":"","county":"东城区","county_code":"156110101","town":"东华门街道","town_code":"156110101001"}}}`, http.StatusOK, nil)
	g, _ := newTestGateway(t, func(c *config) { c.GeocoderUpstream = up.URL })
	rec := doGET(t, g, "/geocode/reverse?lon=116.3974&lat=39.9093", nil)
	var got reverseGeocodeResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if !got.Municipality || got.Administrative.City.Available || got.Administrative.City.Level != "city" || got.Administrative.City.Name != "" || got.Administrative.City.Code != "" || got.Administrative.County.Name != "东城区" {
		t.Fatalf("unexpected municipality result: %+v", got)
	}
}

func TestReverseGeocodeUsesNormalizedCoordinateCache(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"status":"0","result":{"formatted_address":"四川省眉山市东坡区苏祠街道","addressComponent":{"nation":"中国","province":"四川省","province_code":"156510000","city":"眉山市","city_code":"156511400","county":"东坡区","county_code":"156511402","town":"苏祠街道","town_code":"156511402003"}}}`)
	}))
	defer up.Close()
	g, _ := newTestGateway(t, func(c *config) { c.GeocoderUpstream = up.URL })
	first := doGET(t, g, "/geocode/reverse?lon=103.834301&lat=30.050801", nil)
	second := doGET(t, g, "/geocode/reverse?lon=103.834304&lat=30.050804", nil)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("responses %d/%d, upstream hits %d", first.Code, second.Code, hits)
	}
	if second.Header().Get("X-Map-Gateway-Cache") != "hit" {
		t.Fatalf("cache state: %q", second.Header().Get("X-Map-Gateway-Cache"))
	}
}

func TestMunicipalityIsNotInferredFromMissingCity(t *testing.T) {
	if !isMunicipality("重庆市") || isMunicipality("海南省") {
		t.Fatal("municipality must be based on the four municipality names")
	}
}

func TestReverseGeocodeValidationAndFailures(t *testing.T) {
	g, _ := newTestGateway(t, nil)
	for _, path := range []string{
		"/geocode/reverse", "/geocode/reverse?lon=181&lat=30", "/geocode/reverse?lon=1&lat=NaN",
		"/geocode/reverse?lon=1&lat=2&level=town", "/geocode/reverse?lon=1&lon=2&lat=3",
	} {
		if rec := doGET(t, g, path, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, rec.Code)
		}
	}

	noKey, _ := newTestGateway(t, func(c *config) { c.TiandituKey = "" })
	if rec := doGET(t, noKey, "/geocode/reverse?lon=1&lat=2", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing key: got %d", rec.Code)
	}
	up := geocoderStub(t, `{"status":"1","msg":"no result"}`, http.StatusOK, nil)
	bad, _ := newTestGateway(t, func(c *config) { c.GeocoderUpstream = up.URL })
	if rec := doGET(t, bad, "/geocode/reverse?lon=1&lat=2", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no result: got %d", rec.Code)
	}
}

func TestReverseGeocodeRejectsUntrustworthyAdministrativeData(t *testing.T) {
	tests := []string{
		`{"status":"0","result":{"addressComponent":{"nation":"中国","province":"四川省","province_code":"156510000","county":"东坡区","county_code":"","town":"苏祠街道","town_code":"156511402003"}}}`,
		`{"status":"0","result":{"addressComponent":{"nation":"中国","province":"四川省","province_code":"156510000","county":"东坡区","county_code":"156330105"}}}`,
		`{"status":"0","result":{"addressComponent":{"nation":"中国","province":"四川省","province_code":"156510000","county":"东坡区","county_code":"156511402","town":"苏祠街道","town_code":"156330105012"}}}`,
	}
	for i, body := range tests {
		up := geocoderStub(t, body, http.StatusOK, nil)
		g, _ := newTestGateway(t, func(c *config) { c.GeocoderUpstream = up.URL })
		if rec := doGET(t, g, "/geocode/reverse?lon=103.8343&lat=30.0508", nil); rec.Code != http.StatusBadGateway {
			t.Errorf("case %d: got %d: %s", i, rec.Code, rec.Body.String())
		}
	}
}

func TestAdministrativeDivisionContract(t *testing.T) {
	empty, err := makeAdministrativeDivision("town", "", "", 12)
	if err != nil || empty.Available || empty.Level != "town" || empty.Name != "" || empty.Code != "" {
		t.Fatalf("unexpected empty level: %+v, %v", empty, err)
	}
	if _, err := makeAdministrativeDivision("town", "苏祠街道", "", 12); err == nil {
		t.Fatal("name without code must be rejected")
	}
	if _, err := makeAdministrativeDivision("county", "东坡区", "511402", 9); err == nil {
		t.Fatal("non-Tianditu code length must be rejected")
	}
}

func TestAdministrativeCatalogIsCanonicalAuthority(t *testing.T) {
	up := geocoderStub(t, `{"status":"0","result":{"addressComponent":{"nation":"中国","province":"四川","province_code":"156510000","city":"眉山","city_code":"156511400","county":"旧东坡名称","county_code":"156511402","town":"旧苏祠名称","town_code":"156511402003"}}}`, http.StatusOK, nil)
	g, _ := newTestGateway(t, func(c *config) { c.GeocoderUpstream = up.URL })
	rec := doGET(t, g, "/geocode/reverse?lon=103.8343&lat=30.0508", nil)
	var got reverseGeocodeResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if got.Administrative.Province.Name != "四川省" || got.Administrative.County.Name != "东坡区" || got.Administrative.Town.Name != "苏祠街道" {
		t.Fatalf("catalog names were not canonical: %+v", got.Administrative)
	}
}

func TestReverseGeocodeConsistentEmptyTown(t *testing.T) {
	up := geocoderStub(t, `{"status":"0","result":{"addressComponent":{"nation":"中国","province":"四川省","province_code":"156510000","city":"眉山市","city_code":"156511400","county":"东坡区","county_code":"156511402","town":"","town_code":""}}}`, http.StatusOK, nil)
	g, _ := newTestGateway(t, func(c *config) { c.GeocoderUpstream = up.URL })
	rec := doGET(t, g, "/geocode/reverse?lon=103.8343&lat=30.0508", nil)
	var got reverseGeocodeResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if got.ResolvedLevel != "county" || got.Administrative.Town.Available || got.Administrative.Town.Level != "town" || got.Administrative.Town.Name != "" || got.Administrative.Town.Code != "" {
		t.Fatalf("unexpected empty town contract: %+v", got)
	}
}

func TestAdministrativeSearchKeepsProviderAreaCenterAndCaches(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Query().Get("type") != "query" {
			t.Errorf("type: %q", r.URL.Query().Get("type"))
		}
		var post map[string]interface{}
		if err := json.Unmarshal([]byte(r.URL.Query().Get("postStr")), &post); err != nil || post["queryType"] != float64(12) || post["specify"] != "156511402" {
			t.Errorf("unexpected postStr: %s (%v)", r.URL.Query().Get("postStr"), err)
		}
		fmt.Fprint(w, `{"status":{"infocode":1000},"area":[{"name":"东坡区","lonlat":"103.8300,30.0500","bound":"103.7,29.9,104.0,30.2","adminCode":156511402,"level":3}]}`)
	}))
	defer up.Close()
	g, _ := newTestGateway(t, func(c *config) { c.SearchUpstream = up.URL })
	path := "/search/administrative?keyword=%E4%B8%9C%E5%9D%A1%E5%8C%BA&specify=156511402"
	first := doGET(t, g, path, nil)
	second := doGET(t, g, path, nil)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("responses %d/%d, upstream hits %d", first.Code, second.Code, hits)
	}
	var got administrativeSearchResponse
	if err := json.Unmarshal(first.Body.Bytes(), &got); err != nil || len(got.Candidates) != 1 {
		t.Fatalf("bad response: %v %s", err, first.Body.String())
	}
	candidate := got.Candidates[0]
	if candidate.Center == nil || candidate.Center.CenterType != "tianditu_area_center" || candidate.Raw.AdminCode != "156511402" || candidate.Raw.Bound == "" || candidate.Raw.Level != "3" {
		t.Fatalf("provider area was not retained: %+v", candidate)
	}
	if second.Header().Get("X-Map-Gateway-Cache") != "hit" {
		t.Fatalf("second call cache state: %q", second.Header().Get("X-Map-Gateway-Cache"))
	}
}

func TestNearbySearchReturnsNumericDistanceAndCaches100Meters(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var post map[string]interface{}
		if err := json.Unmarshal([]byte(r.URL.Query().Get("postStr")), &post); err != nil || post["queryType"] != float64(3) || post["queryRadius"] != float64(100) || post["keyWord"] != "公园" {
			t.Errorf("unexpected postStr: %s (%v)", r.URL.Query().Get("postStr"), err)
		}
		fmt.Fprint(w, `{"status":{"infocode":"1000"},"pois":[{"name":"东坡湖公园","lonlat":"103.8349,30.0509","distance":"0.08km","typeCode":"110101","typeName":"公园","source":"天地图","hotPointID":"abc","source_id":"source-1","province":"四川省","provinceCode":"156510000","city":"眉山市","cityCode":"156511400","county":"东坡区","countyCode":"156511402"}]}`)
	}))
	defer up.Close()
	g, _ := newTestGateway(t, func(c *config) { c.SearchUpstream = up.URL })
	path := "/search/nearby?lon=103.8343&lat=30.0508&radius_m=100&keyword=%E5%85%AC%E5%9B%AD"
	first := doGET(t, g, path, nil)
	second := doGET(t, g, path, nil)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("responses %d/%d, upstream hits %d", first.Code, second.Code, hits)
	}
	var got nearbySearchResponse
	if err := json.Unmarshal(first.Body.Bytes(), &got); err != nil || len(got.Candidates) != 1 {
		t.Fatalf("bad response: %v %s", err, first.Body.String())
	}
	candidate := got.Candidates[0]
	if candidate.DistanceM != 80 || candidate.TypeCode != "110101" || candidate.HotPointID != "abc" || candidate.SourceID != "source-1" || candidate.County.Code != "156511402" {
		t.Fatalf("unexpected candidate: %+v", candidate)
	}
	var raw map[string]interface{}
	json.Unmarshal(first.Body.Bytes(), &raw)
	if _, exists := raw["confidence"]; exists {
		t.Fatal("gateway must not invent a provider confidence")
	}
}

func TestExpiredQueryCacheIsReturnedAsStaleOnUpstreamFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer up.Close()
	g, _ := newTestGateway(t, func(c *config) { c.SearchUpstream = up.URL })
	key := "tianditu|" + coordinateCacheKey(103.8343, 30.0508) + "|100|公园||20"
	entry := queryCacheEntry{
		FetchedAt: time.Now().Add(-20 * time.Minute).UTC(),
		ExpiresAt: time.Now().Add(-10 * time.Minute).UTC(),
		Payload:   json.RawMessage(`{"schema_version":"1.0","provider":"tianditu","candidates":[]}`),
	}
	body, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	g.cacheWrite(g.queryCacheFile("search/nearby", key), body)
	rec := doGET(t, g, "/search/nearby?lon=103.8343&lat=30.0508&radius_m=100&keyword=%E5%85%AC%E5%9B%AD", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Map-Gateway-Cache") != "stale" {
		t.Fatalf("got %d, cache=%q: %s", rec.Code, rec.Header().Get("X-Map-Gateway-Cache"), rec.Body.String())
	}
}

func TestAdministrativeCatalogUnavailable(t *testing.T) {
	g, _ := newTestGateway(t, func(c *config) { c.AdminDivisionsFile = filepath.Join(t.TempDir(), "missing.tsv") })
	rec := doGET(t, g, "/geocode/reverse?lon=103.8343&lat=30.0508", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

func TestLoadAdminDivisionsValidatesChecksumAndParents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "catalog.tsv")
	if err := os.WriteFile(p, []byte("510000\t四川省\t1\t\n511400\t眉山市\t2\t999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdminDivisions(p, "bad-checksum"); err == nil {
		t.Fatal("checksum mismatch must fail")
	}
	if _, err := loadAdminDivisions(p, ""); err == nil {
		t.Fatal("orphan parent must fail")
	}
}

// ---------------------------------------------------------------------------
// Path safety (only safe numeric components may pass)
// ---------------------------------------------------------------------------

func TestBadPathsRejected(t *testing.T) {
	// Stub upstream so the valid-path check below never touches the real
	// network (and fails loudly instead of fetching a live tile).
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3}
	up, hits := stubUpstream(t, "image/png", png)
	g, _ := newTestGateway(t, func(c *config) {
		c.TerrainUpstream = up.URL + "/%s/%s/%s.png"
	})
	bad := []string{
		"/tile/terrain/a/1/1.png",                  // letters
		"/tile/terrain/3/6/1",                      // no extension
		"/tile/terrain/3/6/1.png/extra",            // extra segment
		"/tile/terrain/3/6/1.PNG",                  // wrong case
		"/tile/terrain/3/6/1.gif",                  // wrong extension
		"/tile/terrain/31/6/1.png",                 // zoom above maxZoom
		"/tile/terrain/-1/6/1.png",                 // negative
		"/tile/terrain/3/6/1.png/../../etc/passwd", // traversal attempt
		"/tile/terrain/3/6/%2e%2e.png",             // encoded dots
		"/tile/terrain/3/6/12345678901.png",        // x too long (11 digits)
		"/tile/sat/3/6/1.gif",                      // sat only png|jpg
		"/tianditu/vec/3/6/1.jpg",                  // tianditu only .png
		"/tianditu/foo/3/6/1.png",                  // unknown layer
		"/tianditu/vec/3/6",                        // missing y
		// tile URLs are path-only: any query string is rejected before
		// rate limiting, cache or upstream handling
		"/tile/terrain/3/6/1.png?x=1",
		"/tile/terrain/3/6/1.png?foo=bar",
		"/tianditu/vec/3/6/1.png?tk=test-key",
	}
	for _, p := range bad {
		if rec := doGET(t, g, p, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", p, rec.Code)
		}
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("upstream hits = %d, want 0 (rejected requests must not reach upstream)", atomic.LoadInt32(hits))
	}

	// valid URL behavior preserved: the same path without a query string is served
	rec := doGET(t, g, "/tile/terrain/3/6/1.png", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != string(png) {
		t.Fatalf("valid path without query: got %d %q, want 200 with tile body", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("upstream hits = %d after valid request, want 1", atomic.LoadInt32(hits))
	}
}

// ---------------------------------------------------------------------------
// Terrain end-to-end (miss -> upstream -> cache -> hit)
// ---------------------------------------------------------------------------

func TestTileTerrain(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3}
	up, hits := stubUpstream(t, "image/png", png)

	g, cacheDir := newTestGateway(t, func(c *config) {
		c.TerrainUpstream = up.URL + "/%s/%s/%s.png"
	})

	// miss: fetches upstream, caches, returns the body with proper headers
	rec := doGET(t, g, "/tile/terrain/3/6/2.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("miss: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != string(png) {
		t.Fatal("miss: body mismatch")
	}
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("miss: content-type %q", rec.Header().Get("Content-Type"))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != cacheControlHeader {
		t.Fatalf("miss: cache-control %q", cc)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("upstream hits = %d, want 1", atomic.LoadInt32(hits))
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "terrain", "3", "6", "2.png")); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	// hit: served from cache, upstream not called again
	rec = doGET(t, g, "/tile/terrain/3/6/2.png", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != string(png) {
		t.Fatalf("hit: %d %q", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("upstream hits = %d after hit, want still 1", atomic.LoadInt32(hits))
	}
}

func TestTileSatOrderAndJpg(t *testing.T) {
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	_, hits := stubUpstream(t, "image/jpeg", jpg)
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpg)
	}))
	defer ts.Close()

	g, cacheDir := newTestGateway(t, func(c *config) {
		c.SatUpstream = ts.URL + "/%s/%s/%s"
	})

	rec := doGET(t, g, "/tile/sat/3/6/2.jpg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sat jpg: got %d, want 200", rec.Code)
	}
	// ArcGIS order: /tile/{z}/{y}/{x} — browser URL is /tile/sat/{z}/{x}/{y}.
	// Request /tile/sat/3/6/2.jpg means z=3, x=6, y=2 -> upstream /3/2/6.
	if gotPath != "/3/2/6" {
		t.Fatalf("upstream path = %q, want /3/2/6 (z/y/x order)", gotPath)
	}
	if rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("content-type %q", rec.Header().Get("Content-Type"))
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "sat", "3", "6", "2.jpg")); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	// .png variant must also work (parity with webapp regex). It is a distinct
	// cache entry (sat/…/2.png vs sat/…/2.jpg), so it fetches upstream once more;
	// the served bytes are sniffed as JPEG even though the cached name ends in .png.
	rec = doGET(t, g, "/tile/sat/3/6/2.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sat png variant: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("sniffed content-type %q, want image/jpeg", ct)
	}
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("upstream hits = %d, want 2 (one per cache variant)", atomic.LoadInt32(hits))
	}
}

// ---------------------------------------------------------------------------
// Tianditu
// ---------------------------------------------------------------------------

func TestTiandituEndToEnd(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 9, 8, 7}
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	}))
	defer ts.Close()

	g, cacheDir := newTestGateway(t, func(c *config) {
		c.TiandituUpstream = ts.URL + "/%d/%s?LAYER=%s&TILEMATRIX=%s&TILEROW=%s&TILECOL=%s&tk=%s"
	})

	rec := doGET(t, g, "/tianditu/vec/3/6/2.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("content-type %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(gotQuery, "tk=test-key") {
		t.Fatalf("query missing key: %q", gotQuery)
	}
	// /tianditu/vec/3/6/2.png -> z=3, x=6, y=2; WMTS wants TILEROW=y, TILECOL=x.
	if !strings.Contains(gotQuery, "TILEMATRIX=3") || !strings.Contains(gotQuery, "TILEROW=2") || !strings.Contains(gotQuery, "TILECOL=6") {
		t.Fatalf("query params wrong: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "LAYER=vec") {
		t.Fatalf("query layer wrong: %q", gotQuery)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "tianditu", "vec", "3", "6", "2.png")); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	// all four layers accepted
	for _, layer := range []string{"cva", "img", "cia"} {
		if rec := doGET(t, g, "/tianditu/"+layer+"/1/2/3.png", nil); rec.Code != http.StatusOK {
			t.Fatalf("layer %s: got %d", layer, rec.Code)
		}
	}
}

func TestTiandituNoKey(t *testing.T) {
	g, _ := newTestGateway(t, func(c *config) { c.TiandituKey = "" })
	rec := doGET(t, g, "/tianditu/vec/3/6/2.png", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
	// terrain/sat still work without any key
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	up, _ := stubUpstream(t, "image/png", png)
	g2, _ := newTestGateway(t, func(c *config) {
		c.TiandituKey = ""
		c.TerrainUpstream = up.URL + "/%s/%s/%s.png"
	})
	if rec := doGET(t, g2, "/tile/terrain/3/6/2.png", nil); rec.Code != http.StatusOK {
		t.Fatalf("terrain without key: got %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Upstream error mapping
// ---------------------------------------------------------------------------

func TestUpstreamErrors(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()

	g, _ := newTestGateway(t, func(c *config) {
		c.TerrainUpstream = bad.URL + "/%s/%s/%s.png"
	})
	if rec := doGET(t, g, "/tile/terrain/3/6/2.png", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream 500: got %d, want 502", rec.Code)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	g2, _ := newTestGateway(t, func(c *config) {
		c.TerrainUpstream = dead.URL + "/%s/%s/%s.png"
	})
	if rec := doGET(t, g2, "/tile/terrain/3/6/2.png", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream conn reset: got %d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Cache eviction (oldest-first, down to watermark)
// ---------------------------------------------------------------------------

func TestCacheEvictionOldestFirst(t *testing.T) {
	g, cacheDir := newTestGateway(t, func(c *config) {
		c.CacheMax = 3 * 1024 // 3 KiB
		c.TileRate = 10000
	})
	base := time.Now().Add(-4 * time.Hour)
	oneK := make([]byte, 1024)

	// three files, oldest -> newest, none exceeding the cap
	for i := 0; i < 3; i++ {
		p := filepath.Join(cacheDir, "terrain", "0", "0", fmt.Sprintf("%d.png", i))
		g.cacheWrite(p, oneK)
		mt := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	// writing a 4th file pushes total to 4 KiB > 3 KiB cap; eviction must
	// remove the two oldest files to drop to the 2.4 KiB watermark.
	p4 := filepath.Join(cacheDir, "terrain", "0", "0", "3.png")
	g.cacheWrite(p4, oneK)

	for _, gone := range []string{"0.png", "1.png"} {
		if _, err := os.Stat(filepath.Join(cacheDir, "terrain", "0", "0", gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should have been evicted (oldest)", gone)
		}
	}
	for _, kept := range []string{"2.png", "3.png"} {
		if _, err := os.Stat(filepath.Join(cacheDir, "terrain", "0", "0", kept)); err != nil {
			t.Fatalf("%s should have been kept: %v", kept, err)
		}
	}
	g.cacheMu.Lock()
	size := g.cacheSize
	g.cacheMu.Unlock()
	if size != 2*1024 {
		t.Fatalf("cache size after eviction = %d, want 2048", size)
	}
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

func TestRateLimitPerIP(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	up, _ := stubUpstream(t, "image/png", png)
	g, _ := newTestGateway(t, func(c *config) {
		c.TileRate = 2
		c.TerrainUpstream = up.URL + "/%s/%s/%s.png"
	})
	ip := "203.0.113.7"
	for i := 1; i <= 2; i++ {
		rec := doGET(t, g, "/tile/terrain/3/6/2.png", map[string]string{"X-Real-IP": ip})
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i, rec.Code)
		}
	}
	if rec := doGET(t, g, "/tile/terrain/3/6/2.png", map[string]string{"X-Real-IP": ip}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: got %d, want 429", rec.Code)
	}
	// a different IP is not limited
	if rec := doGET(t, g, "/tile/terrain/3/6/3.png", map[string]string{"X-Real-IP": "198.51.100.9"}); rec.Code != http.StatusOK {
		t.Fatalf("other IP: got %d, want 200", rec.Code)
	}
	// X-Forwarded-For fallback
	if rec := doGET(t, g, "/tile/terrain/3/6/4.png", map[string]string{"X-Forwarded-For": "198.51.100.10, 10.0.0.1"}); rec.Code != http.StatusOK {
		t.Fatalf("XFF fallback: got %d, want 200", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:52341"
	if got := clientIP(r); got != "127.0.0.1" {
		t.Fatalf("remoteaddr: got %q", got)
	}
	r.Header.Set("X-Real-IP", "1.2.3.4")
	if got := clientIP(r); got != "1.2.3.4" {
		t.Fatalf("x-real-ip: got %q", got)
	}
	r.Header.Del("X-Real-IP")
	r.Header.Set("X-Forwarded-For", " 5.6.7.8 , 9.9.9.9 ")
	if got := clientIP(r); got != "5.6.7.8" {
		t.Fatalf("xff: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Config / env
// ---------------------------------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	t.Setenv("MAP_ENV_FILE", "/nonexistent/env-file")
	cfg := configFromEnv()
	if cfg.ListenAddr != "127.0.0.1:8082" {
		t.Fatalf("listen addr %q", cfg.ListenAddr)
	}
	if cfg.CacheMax != int64(200)<<20 {
		t.Fatalf("cache max %d", cfg.CacheMax)
	}
	if cfg.TileRate != 600 || cfg.UpstreamTimeout != 45*time.Second {
		t.Fatalf("rate %d timeout %v", cfg.TileRate, cfg.UpstreamTimeout)
	}
}

func TestConfigFromEnvAndFile(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(envFile, []byte("# comment\nTIANDITU_KEY=file-key\nMAP_CACHE_MAX_MB=77\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAP_ENV_FILE", envFile)
	t.Setenv("MAP_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("TIANDITU_KEY", "") // empty env -> file value must win

	cfg := configFromEnv()
	if cfg.ListenAddr != "127.0.0.1:9999" {
		t.Fatalf("listen addr %q", cfg.ListenAddr)
	}
	if cfg.TiandituKey != "file-key" {
		t.Fatalf("key %q, want file-key", cfg.TiandituKey)
	}
	if cfg.CacheMax != int64(77)<<20 {
		t.Fatalf("cache max %d", cfg.CacheMax)
	}

	// real env var beats the file
	t.Setenv("TIANDITU_KEY", "env-key")
	cfg = configFromEnv()
	if cfg.TiandituKey != "env-key" {
		t.Fatalf("key %q, want env-key", cfg.TiandituKey)
	}
}

func TestRedact(t *testing.T) {
	g, _ := newTestGateway(t, nil)
	if got := g.redact("dial tcp: https://x/?tk=test-key boom test-key"); strings.Contains(got, "test-key") {
		t.Fatalf("key leaked in: %q", got)
	}
	if !strings.Contains(g.redact("boom test-key"), "REDACTED") {
		t.Fatal("redaction marker missing")
	}
}

func TestReadAllLimited(t *testing.T) {
	// oversized upstream body must be rejected, not cached
	huge := make([]byte, 10)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(huge)
	}))
	defer up.Close()
	g, cacheDir := newTestGateway(t, func(c *config) {
		c.MaxTileBytes = 4
		c.TerrainUpstream = up.URL + "/%s/%s/%s.png"
	})

	rec := doGET(t, g, "/tile/terrain/3/6/2.png", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oversized: got %d, want 502", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "terrain", "3", "6", "2.png")); !os.IsNotExist(err) {
		t.Fatal("oversized tile must not be cached")
	}
}

func TestServeFileSniffsContentType(t *testing.T) {
	g, cacheDir := newTestGateway(t, nil)
	p := filepath.Join(cacheDir, "sat", "3", "6", "2.png") // JPEG bytes under a .png name
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doGET(t, g, "/tile/sat/3/6/2.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type %q, want image/jpeg (sniffed)", ct)
	}
	if rec.Body.Len() != 8 {
		t.Fatalf("body length %d", rec.Body.Len())
	}
}

func TestServeFileNotWritableToCache(t *testing.T) {
	// a cache hit must not require writing anything
	g, cacheDir := newTestGateway(t, nil)
	p := filepath.Join(cacheDir, "terrain", "3", "6", "2.png")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("tile-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doGET(t, g, "/tile/terrain/3/6/2.png", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "tile-bytes" {
		t.Fatalf("hit: %d %q", rec.Code, rec.Body.String())
	}
}
