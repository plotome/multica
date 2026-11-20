package forkupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestPrivateCatalogAndArtifactHTTPPreparation(t *testing.T) {
	var envelope []byte
	var gets atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog":
			w.Write(envelope)
		case "/package":
			gets.Add(1)
			w.Write([]byte("package"))
		case "/redirect":
			http.Redirect(w, r, "/package", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := catalogFixture()
	for i := range c.Artifacts {
		c.Artifacts[i].URL = server.URL + "/package"
	}
	var policy Policy
	envelope, policy = signFixture(t, c)
	policy.Origin = server.URL
	candidate, _, err := FetchCatalog(context.Background(), server.Client(), policy, server.URL+"/catalog", int64(len(envelope)))
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	path, err := DownloadArtifact(context.Background(), server.Client(), policy, candidate.Artifacts[0], cache)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != candidate.Artifacts[0].SHA256 {
		t.Fatal("mutable artifact cache name")
	}
	if _, err := DownloadArtifact(context.Background(), server.Client(), policy, candidate.Artifacts[0], cache); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 1 {
		t.Fatal("verified cache was downloaded again")
	}
	if _, _, err := FetchCatalog(context.Background(), server.Client(), policy, server.URL+"/catalog", int64(len(envelope)-1)); err == nil {
		t.Fatal("catalog byte limit ignored")
	}
	redirect := candidate.Artifacts[0]
	redirect.URL = server.URL + "/redirect"
	if _, err := DownloadArtifact(context.Background(), server.Client(), policy, redirect, t.TempDir()); err == nil {
		t.Fatal("redirect accepted")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DownloadArtifact(context.Background(), server.Client(), policy, candidate.Artifacts[0], cache); err == nil {
		t.Fatal("corrupt cache silently repaired")
	}
}

func TestFailedDownloadDoesNotPublishPackage(t *testing.T) {
	for _, body := range []string{"packagE", "packag", "package-too-long"} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			artifact := catalogFixture().Artifacts[0]
			artifact.URL = server.URL + "/package"
			policy := Policy{Origin: server.URL}
			cache := t.TempDir()
			if _, err := DownloadArtifact(context.Background(), server.Client(), policy, artifact, cache); err == nil {
				t.Fatal("bad artifact accepted")
			}
			if _, err := os.Stat(filepath.Join(cache, artifact.SHA256)); !os.IsNotExist(err) {
				t.Fatal("failed package published")
			}
			matches, _ := filepath.Glob(filepath.Join(cache, ".download-*"))
			if len(matches) != 0 {
				t.Fatal("partial package left behind")
			}
		})
	}
}
