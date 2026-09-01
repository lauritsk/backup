package cloudbackup

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/cloudbackup/catalog"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func (s *Service) Serve(ctx context.Context, info buildinfo.Info) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Addr: s.config.Server.Listen, Handler: s.HTTPHandler(info), ReadHeaderTimeout: s.config.Server.ReadHeaderTimeout.Duration, ReadTimeout: s.config.Server.ReadTimeout.Duration, IdleTimeout: s.config.Server.IdleTimeout.Duration, MaxHeaderBytes: 1 << 20, ErrorLog: log.New(logWriter{s.logger}, "", 0), BaseContext: func(net.Listener) context.Context { return serveCtx }}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	schedulerDone := make(chan struct{})
	go func() { defer close(schedulerDone); s.scheduler(serveCtx) }()
	defer func() { cancel(); <-schedulerDone }()
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), s.config.Server.ShutdownTimeout.Duration)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			server.Close()
			return err
		}
		err := <-result
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type logWriter struct {
	logger interface{ Error(string, ...any) }
}

func (w logWriter) Write(value []byte) (int, error) {
	w.logger.Error(strings.TrimSpace(string(value)))
	return len(value), nil
}
func (s *Service) scheduler(ctx context.Context) {
	if !s.config.Schedule.Enabled {
		return
	}
	trigger := func() {
		_, err := s.QueueBackup(ctx, model.BackupRequest{})
		if err != nil && !errors.Is(err, ErrOperationBusy) {
			s.logger.Error("scheduled backup could not be queued", "error", err)
		}
	}
	if s.config.Schedule.RunOnStart {
		trigger()
	}
	ticker := time.NewTicker(s.config.Schedule.Interval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			trigger()
		}
	}
}

func (s *Service) HTTPHandler(info buildinfo.Info) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeHTTPJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Ready(r.Context()); err != nil {
			writeAPIError(w, 503, "not_ready", "readiness checks failed")
			return
		}
		writeHTTPJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) { writeHTTPJSON(w, 200, info) })
	mux.HandleFunc("GET /api/v1/sources", s.handleSources)
	mux.HandleFunc("GET /api/v1/files", s.handleFiles)
	mux.HandleFunc("GET /api/v1/file", s.handleFile)
	mux.HandleFunc("GET /api/v1/file/raw", s.handleRawFile)
	mux.HandleFunc("GET /api/v1/manifests", s.handleManifests)
	mux.HandleFunc("GET /api/v1/manifests/{id}", s.handleManifest)
	mux.HandleFunc("GET /api/v1/runs", s.handleRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.handleRun)
	mux.HandleFunc("POST /api/v1/backup", s.handleBackup)
	mux.HandleFunc("POST /api/v1/verify", s.handleVerify)
	mux.HandleFunc("POST /api/v1/restore", s.handleRestore)
	return s.authenticate(mux)
}
func (s *Service) authenticate(next http.Handler) http.Handler {
	token := s.config.Server.ResolvedAuthToken
	if token == "" {
		if s.config.Server.AllowUnauthenticated {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !loopbackHost(r.Host) {
				writeAPIError(w, 403, "invalid_host", "request Host must name a loopback address")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		parts := strings.Fields(r.Header.Get("Authorization"))
		provided := ""
		valid := len(parts) == 2 && strings.EqualFold(parts[0], "Bearer")
		if valid {
			provided = parts[1]
		}
		got := sha256.Sum256([]byte(provided))
		if !valid || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cloudbackup"`)
			writeAPIError(w, 401, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func loopbackHost(value string) bool {
	host := value
	if parsed, _, err := net.SplitHostPort(value); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (s *Service) handleSources(w http.ResponseWriter, r *http.Request) {
	value, err := s.ListSources(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, map[string]any{"sources": value})
}
func (s *Service) handleFiles(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || limit < 1 || limit > 1000 || offset < 0 {
		writeAPIError(w, 400, "invalid_request", "invalid pagination")
		return
	}
	value, err := s.ListFiles(r.Context(), r.URL.Query().Get("source"), r.URL.Query().Get("prefix"), limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, map[string]any{"files": value, "limit": limit, "offset": offset})
}
func (s *Service) handleFile(w http.ResponseWriter, r *http.Request) {
	value, err := s.GetFile(r.Context(), r.URL.Query().Get("source"), r.URL.Query().Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, value)
}
func (s *Service) handleRawFile(w http.ResponseWriter, r *http.Request) {
	file, opened, err := s.OpenFile(r.Context(), r.URL.Query().Get("source"), r.URL.Query().Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer opened.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("ETag", `"sha256:`+file.SHA256+`"`)
	w.WriteHeader(200)
	_, _ = io.Copy(w, opened)
}
func (s *Service) handleManifests(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || limit < 1 || limit > 1000 || offset < 0 {
		writeAPIError(w, 400, "invalid_request", "invalid pagination")
		return
	}
	value, err := s.ListManifests(r.Context(), r.URL.Query().Get("source"), limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, map[string]any{"manifests": value, "limit": limit, "offset": offset})
}
func (s *Service) handleManifest(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		writeAPIError(w, 400, "invalid_request", "source is required")
		return
	}
	value, err := s.GetManifest(r.Context(), source, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, value)
}
func (s *Service) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		writeAPIError(w, 400, "invalid_request", "invalid pagination")
		return
	}
	value, err := s.ListRuns(r.Context(), limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, map[string]any{"runs": value, "limit": limit, "offset": offset})
}
func (s *Service) handleRun(w http.ResponseWriter, r *http.Request) {
	value, err := s.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, value)
}
func (s *Service) handleBackup(w http.ResponseWriter, r *http.Request) {
	var input model.BackupRequest
	if err := decodeOptionalJSON(w, r, &input); err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	run, err := s.QueueBackup(r.Context(), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 202, run)
}
func (s *Service) handleVerify(w http.ResponseWriter, r *http.Request) {
	var input model.VerifyRequest
	if err := decodeOptionalJSON(w, r, &input); err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	run, err := s.QueueVerify(r.Context(), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 202, run)
}
func (s *Service) handleRestore(w http.ResponseWriter, r *http.Request) {
	var input model.RestoreRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, "confirmation_required", "restore requires confirm = true")
		return
	}
	run, err := s.QueueRestore(r.Context(), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 202, run)
}
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return decodeJSON(w, r, target)
}
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
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
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeAPIError(w, 404, "not_found", "record not found")
	case errors.Is(err, ErrOperationBusy):
		writeAPIError(w, 409, "operation_busy", err.Error())
	default:
		writeAPIError(w, 500, "operation_failed", err.Error())
	}
}
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeHTTPJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		status = 500
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
