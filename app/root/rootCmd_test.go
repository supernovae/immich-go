package root

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadStartup_ValidatesServerBeforePathParsing(t *testing.T) {
	var pingCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/server/ping" {
			pingCalled = true
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"bad gateway"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	logFile := filepath.Join(t.TempDir(), "test.log")
	ctx := context.Background()
	cmd, _ := RootImmichGoCommand(ctx)
	cmd.SetArgs([]string{
		"upload", "from-folder",
		"--server=" + server.URL,
		"--api-key=test-key",
		"--api-retries=0",
		"--no-ui",
		"--log-file=" + logFile,
		filepath.Join(t.TempDir(), "non-existent-folder-should-not-be-checked-first"),
	})

	err := cmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected error when server ping fails, got nil")
	}
	if !pingCalled {
		t.Fatal("expected server ping to be called before path parsing")
	}
	if !strings.Contains(err.Error(), "PingServer") {
		t.Fatalf("expected PingServer error, got: %v", err)
	}
}

func TestUploadFolderWithDuplicates_CompletesWithZeroPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/server/ping":
			_, _ = w.Write([]byte(`{"res":"pong"}`))
		case r.URL.Path == "/api/users/me":
			_, _ = w.Write([]byte(`{"id":"user-1","email":"test@example.com"}`))
		case r.URL.Path == "/api/server/about":
			_, _ = w.Write([]byte(`{"version":"v3.0.0"}`))
		case r.URL.Path == "/api/server/media-types":
			_, _ = w.Write([]byte(`{"image":[".jpg",".jpeg",".png"],"video":[".mp4",".mov"],"sidecar":[".xmp"]}`))
		case r.URL.Path == "/api/assets/statistics":
			_, _ = w.Write([]byte(`{"images":1,"videos":0,"total":1}`))
		case r.URL.Path == "/api/search/metadata":
			_, _ = w.Write([]byte(`{"assets":{"items":[],"nextPage":null}}`))
		case r.URL.Path == "/api/albums":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"album-1"}`))
		case r.URL.Path == "/api/assets" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"uploaded-asset-1","status":"created"}`))
		case strings.HasPrefix(r.URL.Path, "/api/assets/") && r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"id":"uploaded-asset-1"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()

	logFile := filepath.Join(t.TempDir(), "test.log")
	ctx := context.Background()
	cmd, a := RootImmichGoCommand(ctx)
	cmd.SetArgs([]string{
		"upload", "from-folder",
		"--server=" + server.URL,
		"--api-key=test-key",
		"--pause-immich-jobs=false",
		"--no-ui",
		"--log-file=" + logFile,
		"../upload/TEST_DATA/folder",
	})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	counters := a.FileProcessor().GetAssetCounters()
	if counters.Pending != 0 {
		t.Fatalf("expected 0 pending assets at end of batch, got %d", counters.Pending)
	}
	if counters.Processed == 0 {
		t.Fatalf("expected processed assets > 0, got 0")
	}
}
