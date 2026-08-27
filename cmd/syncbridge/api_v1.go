package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const maxJSONBody = 1 << 20

type apiErrorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiErrorBody{Error: apiError{Code: code, Message: message}})
}

func requireMethod(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, method := range allowed {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	return false
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errUnsupportedContentType
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return fmt.Errorf("decode JSON trailer: %w", err)
	}
	return nil
}

var errUnsupportedContentType = errors.New("content type must be application/json")

func handleDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnsupportedContentType) {
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
		return
	}
	writeAPIError(w, http.StatusBadRequest, "invalid_json", "invalid JSON request")
}

func parsePositiveID(value string) (int, error) {
	id, err := strconv.Atoi(value)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func revisionETag(revision uint64) string { return fmt.Sprintf("\"%d\"", revision) }

func parseIfMatch(r *http.Request) (uint64, error) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if value == "" {
		return 0, errors.New("If-Match is required")
	}
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimSpace(strings.TrimPrefix(value, "W/"))
	}
	value = strings.Trim(value, "\"")
	rev, err := strconv.ParseUint(value, 10, 64)
	if err != nil || rev == 0 {
		return 0, errors.New("invalid If-Match revision")
	}
	return rev, nil
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func mapServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrJobNotFound), errors.Is(err, ErrRunNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, ErrRevisionConflict), errors.Is(err, ErrOverlap), errors.Is(err, ErrRunNotActive):
		writeAPIError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}
