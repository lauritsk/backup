package appbackup

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/catalog"
	"github.com/lauritsk/backup/internal/appbackup/model"
	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/httpapi"
	"github.com/lauritsk/backup/internal/safeerror"
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
	mux.HandleFunc("GET /api/v1/applications", s.handleApplications)
	mux.HandleFunc("GET /api/v1/recovery-points", s.handleRecoveryPoints)
	mux.HandleFunc("GET /api/v1/recovery-points/{id}", s.handleRecoveryPoint)
	mux.HandleFunc("GET /api/v1/recovery-points/{id}/contents", s.handleContents)
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
			w.Header().Set("WWW-Authenticate", `Bearer realm="appbackup"`)
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
func (s *Service) handleApplications(w http.ResponseWriter, r *http.Request) {
	value, err := s.ListApplications(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, map[string]any{"applications": value})
}
func (s *Service) handleRecoveryPoints(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	value, err := s.ListRecoveryPoints(r.Context(), r.URL.Query().Get("application"), limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, map[string]any{"recovery_points": value, "limit": limit, "offset": offset})
}
func (s *Service) handleRecoveryPoint(w http.ResponseWriter, r *http.Request) {
	value, err := s.GetRecoveryPoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, value)
}
func (s *Service) handleContents(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	value, err := s.ListRecoveryPointContents(r.Context(), r.PathValue("id"), limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeHTTPJSON(w, 200, map[string]any{"paths": value, "limit": limit, "offset": offset})
}
func (s *Service) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
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

var (
	decodeOptionalJSON = httpapi.DecodeOptionalJSON
	decodeJSON         = httpapi.DecodeJSON
	pagination         = httpapi.Pagination
	writeHTTPJSON      = httpapi.WriteJSON
)

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, os.ErrNotExist):
		writeAPIError(w, 404, "not_found", "record not found")
	case errors.Is(err, ErrOperationBusy):
		writeAPIError(w, 409, "operation_busy", err.Error())
	default:
		writeAPIError(w, 500, "operation_failed", safeerror.Clean(err).Error())
	}
}
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeHTTPJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
