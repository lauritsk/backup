// Package httpapi contains bounded JSON and pagination mechanics shared by HTTP APIs.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
)

const maxBodySize = 1 << 20

func DecodeOptionalJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	if err := decodeJSON(w, r, target); errors.Is(err, io.EOF) {
		return nil
	} else {
		return err
	}
}

func DecodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	return decodeJSON(w, r, target)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("JSON body must contain one value")
	}
	return nil
}

func requireJSONContentType(r *http.Request) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	return nil
}

func Pagination(r *http.Request) (limit, offset int, err error) {
	limit, err = queryInt(r, "limit", 100)
	if err != nil {
		return 0, 0, err
	}
	offset, err = queryInt(r, "offset", 0)
	if err != nil {
		return 0, 0, err
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		return 0, 0, errors.New("invalid pagination")
	}
	return limit, offset, nil
}

func queryInt(r *http.Request, name string, fallback int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		encoded = []byte(`{"error":{"code":"encoding_failed","message":"response encoding failed"}}`)
	}
	encoded = append(encoded, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
