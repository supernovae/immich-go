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
