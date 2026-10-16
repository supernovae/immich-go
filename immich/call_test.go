package immich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type testServer struct {
	// endpoint       string
	responseStatus int
	responseBody   string
}

func (ts *testServer) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.WriteHeader(ts.responseStatus)
	_, _ = resp.Write([]byte(ts.responseBody))
}

func TestCall(t *testing.T) {
	tt := []struct {
		name        string
		requestFn   requestFunction
		expectedErr bool
		server      testServer
	}{
		{
			name:        "happy path",
			requestFn:   getRequest("/assets", setAcceptJSON()),
			expectedErr: false,
			server: testServer{
				responseStatus: http.StatusOK,
				responseBody:   `{"status": "All correct"}`,
			},
		},
		{
			name:        "bad url",
			requestFn:   getRequest("/ass\nets", setAcceptJSON()),
			expectedErr: true,
			server: testServer{
				responseStatus: http.StatusOK,
				responseBody:   `{"status": "All correct"}`,
			},
		},
		{
			name:        "post / ok",
			requestFn:   postRequest("/albums", "application/json", setAcceptJSON(), setJSONBody(struct{ Name string }{Name: "test"})),
			expectedErr: false,
			server: testServer{
				responseStatus: http.StatusOK,
				responseBody:   `{"Name": "test"}`,
			},
		},
		{
			name:        "bad request / post",
			requestFn:   postRequest("/albums", "application/json", setAcceptJSON(), setJSONBody(struct{ Name string }{Name: "test"})),
			expectedErr: true,
			server: testServer{
				responseStatus: http.StatusBadRequest,
				responseBody:   `{"error": "Bad request", "statusCode": "400", "message": ["String1","String2"]}`,
			},
		},
	}

	for _, tst := range tt {
		t.Run(tst.name, func(t *testing.T) {
			server := httptest.NewServer(&tst.server)
			defer server.Close()
			ctx := context.Background()
			ic, err := NewImmichClient(server.URL, "1234")
			if err != nil {
				t.Fail()
				return
			}
			// ic.EnableAppTrace(true)
			r := map[string]string{}
			err = ic.newServerCall(ctx, tst.name).do(tst.requestFn, responseJSON(&r))
			if tst.expectedErr && err == nil {
				t.Errorf("expected error, but no error")
			}
			if !tst.expectedErr && err != nil {
				t.Errorf("no error expected, but error: %s", err.Error())
			}
			if err != nil {
				t.Logf("error received: %s", err.Error())
			}
			t.Logf("response received: %#v", r)
		})
	}
}

func TestCall_RetriesTransient5xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message": "Temporary overload"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status": "recovered"}`))
	}))
	defer server.Close()

	ic, err := NewImmichClient(server.URL, "1234", OptionRetries(3, 5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewImmichClient error: %v", err)
	}

	var r map[string]string
	err = ic.newServerCall(context.Background(), "retry-test").do(
		postRequest("/albums", "application/json", setAcceptJSON(), setJSONBody(struct{ Name string }{Name: "test"})),
		responseJSON(&r),
	)
	if err != nil {
		t.Fatalf("expected retry to recover, got error: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
	if r["status"] != "recovered" {
		t.Errorf("expected status 'recovered', got %q", r["status"])
	}
}
