package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// The served worker's cache name carries a digest of the embedded static tree,
// so a build with different shell assets installs a new worker and cache.
func TestServiceWorkerCacheNameTracksEmbeddedAssets(t *testing.T) {
	srv := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !regexp.MustCompile(`const CACHE_VERSION = "agentdeck-shell-v\d+-[0-9a-f]{12}"`).MatchString(rr.Body.String()) {
		t.Fatalf("cache name is not keyed to the embedded assets:\n%.200s", rr.Body.String())
	}
}
