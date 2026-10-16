package immich

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// callError represents errors returned by the server
type callError struct {
	endPoint string
	method   string
	url      string
	status   int
	err      error
	message  serverError
}

type serverError interface {
	lines() []string
}

type serverErrorV3 struct {
	Message       any             `json:"message"`
	Errors        json.RawMessage `json:"errors"`
	CorrelationID string          `json:"-"` // from the X-Correlation-ID header
}

func (e serverErrorV3) lines() []string {
	var out []string
	switch m := e.Message.(type) {
	case string:
		if m != "" {
			out = append(out, m)
		}
	case []any:
		for _, item := range m {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	if len(e.Errors) > 0 {
		out = append(out, string(e.Errors))
	}
	if e.CorrelationID != "" {
		out = append(out, "correlationId: "+e.CorrelationID)
	}
	return out
}

func (ce callError) Is(target error) bool {
	_, ok := target.(*callError)
	return ok
}

func (ce callError) Error() string {
	head := fmt.Sprintf("%s, %s, %s", ce.endPoint, ce.method, ce.url)
	if ce.status > 0 {
		head += fmt.Sprintf(", %d %s", ce.status, http.StatusText(ce.status))
	}
	lines := []string{head}
	if ce.err != nil && !errors.Is(ce.err, &callError{}) {
		lines = append(lines, ce.err.Error())
	}
	if ce.message != nil {
		lines = append(lines, ce.message.lines()...)
	}
	return strings.Join(lines, "\n") + "\n"
}
