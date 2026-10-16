package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
	"github.com/simulot/immich-go/internal/assets"
	"github.com/simulot/immich-go/internal/fshelper"
)

const (
	EndPointGetJobs                = "GetJobs"
	EndPointSendJobCommand         = "SendJobCommand"
	EndPointCreateJob              = "CreateJob"
	EndPointGetAllAlbums           = "GetAllAlbums"
	EndPointGetAlbumInfo           = "GetAlbumInfo"
	EndPointAddAsstToAlbum         = "AddAssetToAlbum"
	EndPointCreateAlbum            = "CreateAlbum"
	EndPointGetAssetAlbums         = "GetAssetAlbums"
	EndPointDeleteAlbum            = "DeleteAlbum"
	EndPointPingServer             = "PingServer"
	EndPointValidateConnection     = "ValidateConnection"
	EndPointGetServerStatistics    = "GetServerStatistics"
	EndPointGetAssetStatistics     = "GetAssetStatistics"
	EndPointGetSupportedMediaTypes = "GetSupportedMediaTypes"
	EndPointGetAllAssets           = "GetAllAssets"
	EndPointUpsertTags             = "UpsertTags"
	EndPointTagAssets              = "TagAssets"
	EndPointBulkTagAssets          = "BulkTagAssets"
	EndPointGetAllTags             = "GetAllTags"
	EndPointAssetUpload            = "AssetUpload"
	EndPointAssetReplace           = "AssetReplace"
	EndPointCopyAsset              = "CopyAsset"
	EndPointGetAboutInfo           = "GetAboutInfo"
	EndPointGetSearchSuggestions   = "GetSearchSuggestions"
	EndPointGetAllPeople           = "GetAllPeople"
	EndPointSignUpAdmin            = "SignUpAdmin"
	EndPointAdminLogin             = "AdminLogin"
	EndPointLogin                  = "Login"
	EndPointGetUserInfo            = "GetUserInfo"
	EndPointUpdateAdminOnboarding  = "UpdateAdminOnboarding"
	EndPointCreateApiKey           = "CreateApiKey"
)

type TooManyInternalError struct {
	error
}

func (e TooManyInternalError) Is(target error) bool {
	_, ok := target.(*TooManyInternalError)
	return ok
}

// serverCall permit to decorate request and responses in one line
type serverCall struct {
	endPoint           string
	ic                 *ImmichClient
	err                error
	ctx                context.Context
	hasResponseHandler bool
	hasStreamBody      bool
}

func (ic *ImmichClient) newServerCall(ctx context.Context, api string) *serverCall {
	sc := &serverCall{
		endPoint: api,
		ic:       ic,
		ctx:      ctx,
	}
	return sc
}

func (sc *serverCall) Err(req *http.Request, resp *http.Response, msg serverError) error {
	ce := callError{
		endPoint: sc.endPoint,
		err:      sc.err,
	}
	if req != nil {
		ce.method = req.Method
		ce.url = req.URL.String()
	}
	if resp != nil {
		ce.status = resp.StatusCode
	}
	ce.message = msg
	return ce
}

// Builds the correct error message based on server version
func (sc *serverCall) decodeServerError(resp *http.Response) serverError {
	var body []byte
	if resp.Body != nil {
		defer resp.Body.Close()
		if isJSON(resp.Header.Get("Content-Type")) {
			body, _ = io.ReadAll(resp.Body)
		}
	}

	e := serverErrorV3{CorrelationID: resp.Header.Get("X-Correlation-ID")}
	json.Unmarshal(body, &e)
	return e
}

func (sc *serverCall) joinError(err error) error {
	sc.err = errors.Join(sc.err, err)
	return err
}

type requestFunction func(sc *serverCall) *http.Request

var callSequence atomic.Int64

type callSequenceID string

const ctxCallSequenceID callSequenceID = "api-call-sequence"

func (sc *serverCall) request(
	method string,
	url string,
	opts ...serverRequestOption,
) *http.Request {
	if sc.ic.apiTraceWriter != nil && sc.endPoint != EndPointGetJobs {
		seq := callSequence.Add(1)
		sc.ctx = context.WithValue(sc.ctx, ctxCallSequenceID, seq)
	}
	req, err := http.NewRequestWithContext(sc.ctx, method, url, nil)
	if sc.joinError(err) != nil {
		return nil
	}
	opts = append(opts, setAPIKey())
	for _, opt := range opts {
		if opt != nil {
			if sc.joinError(opt(sc, req)) != nil {
				return nil
			}
		}
	}
	return req
}

func getRequest(url string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(http.MethodGet, sc.ic.endPoint+url, opts...)
	}
}

func postRequest(url string, cType string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(
			http.MethodPost,
			sc.ic.endPoint+url,
			append(opts, setContentType(cType))...)
	}
}

func deleteRequest(url string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(http.MethodDelete, sc.ic.endPoint+url, opts...)
	}
}

func putRequest(url string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(http.MethodPut, sc.ic.endPoint+url, opts...)
	}
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ce callError
	if errors.As(err, &ce) {
		if ce.status == 0 {
			return ce.err != nil
		}
		return isRetryableStatus(ce.status)
	}
	return true
}

func (ic *ImmichClient) retryWait(ctx context.Context, attempt int) error {
	base := ic.RetriesDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	backoff := base * time.Duration(1<<min(attempt, 5))
	jitter := time.Duration(rand.Int64N(int64(base)/2 + 1))
	timer := time.NewTimer(backoff + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (sc *serverCall) do(fnRequest requestFunction, opts ...serverResponseOption) error {
	maxAttempts := 1
	if sc.ic != nil && sc.ic.Retries > 0 && sc.endPoint != EndPointGetJobs {
		maxAttempts = sc.ic.Retries + 1
	}

	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := sc.ic.retryWait(sc.ctx, attempt-1); err != nil {
				sc.err = err
				return sc.Err(nil, nil, nil)
			}
		}

		sc.err = nil
		sc.hasResponseHandler = false
		req := fnRequest(sc)
		if sc.err != nil || req == nil {
			if req != nil && req.Body != nil {
				_ = req.Body.Close()
			}
			return sc.Err(req, nil, nil)
		}

		resp, err := sc.ic.client.Do(req)
		if err != nil {
			sc.err = err
			lastErr = sc.Err(req, nil, nil)
			if !sc.hasStreamBody && attempt+1 < maxAttempts && sc.ctx.Err() == nil {
				continue
			}
			return lastErr
		}

		if resp.StatusCode >= 300 {
			lastErr = sc.Err(req, resp, sc.decodeServerError(resp))
			if !sc.hasStreamBody && attempt+1 < maxAttempts && isRetryableStatus(resp.StatusCode) && sc.ctx.Err() == nil {
				continue
			}
			return lastErr
		}

		for _, opt := range opts {
			if opt != nil {
				_ = sc.joinError(opt(sc, resp))
			}
		}
		if !sc.hasResponseHandler && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if sc.err != nil {
			lastErr = sc.Err(req, resp, nil)
			if !sc.hasStreamBody && attempt+1 < maxAttempts && sc.ctx.Err() == nil {
				continue
			}
			return lastErr
		}
		return nil
	}
	return lastErr
}

type serverRequestOption func(sc *serverCall, req *http.Request) error

func setBody(body io.ReadCloser) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		sc.hasStreamBody = true
		req.Body = body
		return nil
	}
}

func setImmichChecksum(a *assets.Asset) serverRequestOption {
	if a.Checksum == "" {
		return nil
	}
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Set("x-immich-checksum", a.Checksum)
		return nil
	}
}

func setAcceptJSON() serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Add("Accept", "application/json")
		return nil
	}
}

func setOctetStream() serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Add("Accept", "application/octet-stream")
		return nil
	}
}

func setAPIKey() serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Set("x-api-key", sc.ic.key)
		return nil
	}
}

func setJSONBody(object any) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		data, err := json.Marshal(object)
		if err != nil {
			return err
		}
		req.Body = io.NopCloser(bytes.NewReader(data))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
		req.ContentLength = int64(len(data))
		req.Header.Set("Content-Type", "application/json")
		return nil
	}
}

func setContentType(cType string) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Set("Content-Type", cType)
		return nil
	}
}

type URLRequester interface {
	SetURL(u *url.URL) error
}

func UrlRequest(ur URLRequester) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		if ur != nil {
			return ur.SetURL(req.URL)
		}
		return nil
	}
}

type serverResponseOption func(sc *serverCall, resp *http.Response) error

func responseJSON[T any](object *T) serverResponseOption {
	return func(sc *serverCall, resp *http.Response) error {
		sc.hasResponseHandler = true
		if resp != nil {
			if resp.Body != nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					return nil
				}
				err := json.NewDecoder(resp.Body).Decode(object)
				if err != nil {
					err = fmt.Errorf("can't decode JSON response: %w", err)
				}
				return err
			}
		}
		return errors.New("can't decode nil response")
	}
}

func responseCopy(buffer *bytes.Buffer) serverResponseOption {
	return func(sc *serverCall, resp *http.Response) error {
		sc.hasResponseHandler = true
		if resp != nil {
			if resp.Body != nil {
				newBody := fshelper.TeeReadCloser(resp.Body, buffer)
				resp.Body = newBody
				return nil
			}
		}
		return nil
	}
}

func responseOctetStream(rc *io.ReadCloser) serverResponseOption {
	return func(sc *serverCall, resp *http.Response) error {
		sc.hasResponseHandler = true
		if resp != nil {
			if resp.Body != nil {
				*rc = resp.Body
				return nil
			}
		}
		return nil
	}
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}
