package immich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/simulot/immich-go/internal/assets"
)

type callValues string

const (
	TimeFormat    string     = "2006-01-02T15:04:05.000Z"
	ctxCallValues callValues = "call-values"
)

func setContextValue(kv map[string]string) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		if sc.err != nil || kv == nil {
			return nil
		}
		sc.ctx = context.WithValue(sc.ctx, ctxCallValues, kv)
		return nil
	}
}

func (ic *ImmichClient) uploadAsset(ctx context.Context, la *assets.Asset, endPoint string, replaceID string) (AssetResponse, error) {
	if ic.dryRun {
		return AssetResponse{
			ID:     uuid.NewString(),
			Status: UploadCreated,
		}, nil
	}

	if ic.UploadDelay > 0 {
		timer := time.NewTimer(ic.UploadDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return AssetResponse{}, ctx.Err()
		case <-timer.C:
		}
	}

	var ar AssetResponse
	ext := path.Ext(la.OriginalFileName)
	if strings.TrimSuffix(la.OriginalFileName, ext) == "" {
		la.OriginalFileName = "No Name" + ext // fix #88, #128
	}
	if strings.ToUpper(ext) == ".MP" {
		ext = ".MP4" // #405
		la.OriginalFileName = la.OriginalFileName + ".MP4"
	}

	mtype := ic.TypeFromExt(ext)
	switch mtype {
	case "video", "image":
	default:
		return ar, fmt.Errorf("type file not supported: %s", path.Ext(la.OriginalFileName))
	}

	maxAttempts := max(1, ic.Retries+1)
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := ic.retryWait(ctx, attempt-1); err != nil {
				return ar, err
			}
		}
		ar, lastErr = ic.uploadAssetOnce(ctx, la, endPoint, replaceID, ext, mtype)
		if lastErr == nil {
			return ar, nil
		}
		if !isRetryableError(lastErr) || ctx.Err() != nil {
			return ar, lastErr
		}
	}
	return ar, lastErr
}

func (ic *ImmichClient) uploadAssetOnce(ctx context.Context, la *assets.Asset, endPoint, replaceID, ext, mtype string) (AssetResponse, error) {
	var ar AssetResponse

	f, err := la.OpenFile()
	if err != nil {
		return ar, err
	}
	defer f.Close()

	s, err := f.Stat()
	if err != nil {
		return ar, err
	}

	callValues := ic.prepareCallValues(la, s, ext, mtype)
	body, pw := io.Pipe()
	m := multipart.NewWriter(pw)

	errChan := make(chan error, 1)
	go func() {
		var gErr error
		defer func() {
			closeErr := m.Close()
			if gErr == nil {
				gErr = closeErr
			}
			_ = pw.CloseWithError(gErr)
			errChan <- gErr
		}()

		gErr = ic.writeMultipartFields(m, callValues)
		if gErr != nil {
			return
		}

		gErr = ic.writeFilePart(m, f, la.OriginalFileName, mtype)
		if gErr != nil {
			return
		}

		if la.FromSideCar != nil && strings.HasSuffix(strings.ToLower(la.FromSideCar.File.Name()), ".xmp") {
			gErr = ic.writeSideCarPart(m, la)
			if gErr != nil {
				return
			}
		}
	}()

	var errCall error
	switch endPoint {
	case EndPointAssetUpload:
		errCall = ic.newServerCall(ctx, EndPointAssetUpload).
			do(postRequest("/assets", m.FormDataContentType(), setContextValue(callValues), setAcceptJSON(), setImmichChecksum(la), setBody(body)), responseJSON(&ar))
	case EndPointAssetReplace:
		errCall = ic.newServerCall(ctx, EndPointAssetReplace).
			do(putRequest("/assets/"+replaceID+"/original", setContextValue(callValues), setAcceptJSON(), setImmichChecksum(la), setContentType(m.FormDataContentType()), setBody(body)), responseJSON(&ar))
	}
	// Always unblock the pipe writer if the HTTP client exited early
	_ = body.CloseWithError(errCall)
	gErr := <-errChan
	if (ar.Status == UploadDuplicate || errCall != nil) && errors.Is(gErr, io.ErrClosedPipe) {
		// Immich closes the connection early when x-immich-checksum matches a duplicate or when returning an HTTP error
		gErr = nil
	}
	if ar.Status == UploadDuplicate && errors.Is(errCall, io.ErrClosedPipe) {
		errCall = nil
	}
	return ar, errors.Join(errCall, gErr)
}

func (ic *ImmichClient) prepareCallValues(la *assets.Asset, s fs.FileInfo, ext, mtype string) map[string]string {
	callValues := map[string]string{}

	callValues["deviceAssetId"] = fmt.Sprintf("%s-%d", path.Base(la.OriginalFileName), s.Size())
	callValues["deviceId"] = ic.DeviceUUID
	callValues["assetType"] = mtype
	if !la.CaptureDate.IsZero() {
		callValues["fileCreatedAt"] = la.CaptureDate.Format(TimeFormat)
	} else {
		callValues["fileCreatedAt"] = s.ModTime().UTC().Format(TimeFormat)
	}
	callValues["fileModifiedAt"] = s.ModTime().UTC().Format(TimeFormat)
	callValues["isFavorite"] = myBool(la.Favorite).String()
	callValues["fileExtension"] = ext

	callValues["duration"] = "0"
	callValues["isReadOnly"] = "false"
	if la.Archived {
		callValues["visibility"] = "archive"
	} else {
		callValues["visibility"] = "timeline"
	}
	return callValues
}

func (ic *ImmichClient) writeMultipartFields(m *multipart.Writer, callValues map[string]string) error {
	for key, value := range callValues {
		err := m.WriteField(key, value)
		if err != nil {
			return err
		}
	}
	return nil
}

func (ic *ImmichClient) writeFilePart(m *multipart.Writer, f io.Reader, originalFileName, _ string) error {
	w, err := m.CreateFormFile("assetData", originalFileName)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func (ic *ImmichClient) writeSideCarPart(m *multipart.Writer, la *assets.Asset) error {
	scName := path.Base(la.OriginalFileName) + ".xmp"

	w, err := m.CreateFormFile("sidecarData", scName)
	if err != nil {
		return err
	}
	scf, err := la.FromSideCar.File.Open()
	if err != nil {
		return err
	}
	defer scf.Close()
	_, err = io.Copy(w, scf)
	return err
}
