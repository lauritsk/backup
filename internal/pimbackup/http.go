package pimbackup

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

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/pimbackup/catalog"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

func (s *Service) Serve(ctx context.Context, info buildinfo.Info) error {
	serveContext, cancel := context.WithCancel(ctx)
	defer cancel()

	handler := s.HTTPHandler(info)
	server := &http.Server{
		Addr:              s.config.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: s.config.Server.ReadHeaderTimeout.Duration,
		ReadTimeout:       s.config.Server.ReadTimeout.Duration,
		IdleTimeout:       s.config.Server.IdleTimeout.Duration,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(slogWriter{s.logger}, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return serveContext
		},
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	s.logger.Info("HTTP server listening", "address", listener.Addr().String())
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		s.runScheduler(serveContext)
	}()
	defer func() {
		cancel()
		<-schedulerDone
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()
	select {
	case <-serveContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), s.config.Server.ShutdownTimeout.Duration)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	}
}

type slogWriter struct {
	logger interface {
		Error(string, ...any)
	}
}

func (w slogWriter) Write(value []byte) (int, error) {
	w.logger.Error(strings.TrimSpace(string(value)))
	return len(value), nil
}

func (s *Service) HTTPHandler(info buildinfo.Info) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		if err := s.Ready(request.Context()); err != nil {
			s.logger.Warn("readiness check failed", "error", err)
			writeAPIError(writer, http.StatusServiceUnavailable, "not_ready", "readiness checks failed")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/version", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, info)
	})
	mux.HandleFunc("GET /api/v1/accounts", s.handleAccounts)
	mux.HandleFunc("GET /api/v1/mailboxes", s.handleMailboxes)
	mux.HandleFunc("GET /api/v1/messages", s.handleMessages)
	mux.HandleFunc("GET /api/v1/messages/{id}", s.handleMessage)
	mux.HandleFunc("GET /api/v1/messages/{id}/raw", s.handleRawMessage)
	mux.HandleFunc("GET /api/v1/collections", s.handleCollections)
	mux.HandleFunc("GET /api/v1/objects", s.handleObjects)
	mux.HandleFunc("GET /api/v1/objects/{id}", s.handleObject)
	mux.HandleFunc("GET /api/v1/objects/{id}/raw", s.handleRawObject)
	mux.HandleFunc("GET /api/v1/runs", s.handleRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.handleRun)
	mux.HandleFunc("POST /api/v1/backup", s.handleBackup)
	mux.HandleFunc("POST /api/v1/verify", s.handleVerify)
	mux.HandleFunc("POST /api/v1/restore", s.handleRestore)
	return s.recoverPanic(s.authenticate(mux))
}

func (s *Service) authenticate(next http.Handler) http.Handler {
	token := s.config.Server.ResolvedAuthToken
	if token == "" {
		if s.config.Server.AllowUnauthenticated {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if !loopbackRequestHost(request.Host) {
				writeAPIError(writer, http.StatusForbidden, "invalid_host", "request Host must name a loopback address")
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" || request.URL.Path == "/readyz" {
			next.ServeHTTP(writer, request)
			return
		}
		parts := strings.Fields(request.Header.Get("Authorization"))
		provided := ""
		validScheme := len(parts) == 2 && strings.EqualFold(parts[0], "Bearer")
		if validScheme {
			provided = parts[1]
		}
		got := sha256.Sum256([]byte(provided))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 || !validScheme {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="pimbackup"`)
			writeAPIError(writer, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func loopbackRequestHost(value string) bool {
	host := value
	if parsedHost, _, err := net.SplitHostPort(value); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Service) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("HTTP handler panic", "error", recovered, "path", request.URL.Path)
				writeAPIError(writer, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(writer, request)
	})
}

func (s *Service) handleAccounts(writer http.ResponseWriter, request *http.Request) {
	accounts, err := s.ListAccounts(request.Context())
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"accounts": accounts})
}

func (s *Service) handleMailboxes(writer http.ResponseWriter, request *http.Request) {
	includeInactive, err := queryBool(request, "include_inactive", false)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mailboxes, err := s.ListMailboxes(request.Context(), request.URL.Query().Get("account"), includeInactive)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"mailboxes": mailboxes})
}

func (s *Service) handleMessages(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryInt(request, "limit", 100)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	offset, err := queryInt(request, "offset", 0)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 1000 and offset cannot be negative")
		return
	}
	uidValidity, err := queryUint32(request, "uid_validity", 0)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	messages, err := s.ListMessages(request.Context(), model.MessageFilter{
		AccountID:   request.URL.Query().Get("account"),
		Mailbox:     request.URL.Query().Get("mailbox"),
		UIDValidity: uidValidity,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"messages": messages, "limit": limit, "offset": offset})
}

func (s *Service) handleMessage(writer http.ResponseWriter, request *http.Request) {
	id, err := pathID(request)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	message, err := s.GetMessage(request.Context(), id)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, message)
}

func (s *Service) handleRawMessage(writer http.ResponseWriter, request *http.Request) {
	id, err := pathID(request)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	message, file, err := s.OpenMessage(request.Context(), id)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "message/rfc822")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", strconv.FormatInt(message.Size, 10))
	writer.Header().Set("ETag", `"sha256:`+message.SHA256+`"`)
	writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="message-%d.eml"`, message.ID))
	writer.WriteHeader(http.StatusOK)
	if _, err := io.Copy(writer, file); err != nil {
		s.logger.Warn("streaming raw message failed", "message_id", message.ID, "error", err)
	}
}

func (s *Service) handleCollections(writer http.ResponseWriter, request *http.Request) {
	includeInactive, err := queryBool(request, "include_inactive", false)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	kind := request.URL.Query().Get("kind")
	if kind != "" && kind != "mail" && kind != "contact" && kind != "calendar" {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "kind must be mail, contact, or calendar")
		return
	}
	collections, err := s.ListCollections(request.Context(), request.URL.Query().Get("account"), kind, includeInactive)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"collections": collections})
}

func (s *Service) handleObjects(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryInt(request, "limit", 100)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	offset, err := queryInt(request, "offset", 0)
	if err != nil || limit < 1 || limit > 1000 || offset < 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 1000 and offset cannot be negative")
		return
	}
	kind := request.URL.Query().Get("kind")
	if kind != "" && kind != "mail" && kind != "contact" && kind != "calendar" {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "kind must be mail, contact, or calendar")
		return
	}
	objects, err := s.ListObjects(request.Context(), model.ObjectFilter{AccountID: request.URL.Query().Get("account"), Collection: request.URL.Query().Get("collection"), Kind: kind, Limit: limit, Offset: offset})
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"objects": objects, "limit": limit, "offset": offset})
}

func (s *Service) handleObject(writer http.ResponseWriter, request *http.Request) {
	id, err := pathID(request)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	object, err := s.GetObject(request.Context(), id)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, object)
}

func (s *Service) handleRawObject(writer http.ResponseWriter, request *http.Request) {
	id, err := pathID(request)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	object, file, err := s.OpenObject(request.Context(), id)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	defer file.Close()
	contentType := map[string]string{"mail": "message/rfc822", "contact": "text/vcard; charset=utf-8", "calendar": "text/calendar; charset=utf-8"}[object.Kind]
	extension := map[string]string{"mail": ".eml", "contact": ".vcf", "calendar": ".ics"}[object.Kind]
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	writer.Header().Set("ETag", `"sha256:`+object.SHA256+`"`)
	writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="object-%d%s"`, object.ID, extension))
	writer.WriteHeader(http.StatusOK)
	if _, err := io.Copy(writer, file); err != nil {
		s.logger.Warn("streaming raw object failed", "object_id", object.ID, "error", err)
	}
}

func (s *Service) handleRuns(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryInt(request, "limit", 100)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	offset, err := queryInt(request, "offset", 0)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 1000 and offset cannot be negative")
		return
	}
	runs, err := s.ListRuns(request.Context(), limit, offset)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"runs": runs, "limit": limit, "offset": offset})
}

func (s *Service) handleRun(writer http.ResponseWriter, request *http.Request) {
	run, err := s.GetRun(request.Context(), request.PathValue("id"))
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, run)
}

func (s *Service) handleBackup(writer http.ResponseWriter, request *http.Request) {
	var input model.BackupRequest
	if err := decodeOptionalJSON(writer, request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	run, err := s.QueueBackup(request.Context(), input)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, run)
}

func (s *Service) handleVerify(writer http.ResponseWriter, request *http.Request) {
	var input model.VerifyRequest
	if err := decodeOptionalJSON(writer, request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	run, err := s.QueueVerify(request.Context(), input)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, run)
}

func (s *Service) handleRestore(writer http.ResponseWriter, request *http.Request) {
	var input model.RestoreRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !input.Confirm {
		writeAPIError(writer, http.StatusBadRequest, "confirmation_required", "restore requires confirm = true")
		return
	}
	run, err := s.QueueRestore(request.Context(), input)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, run)
}

func decodeOptionalJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	if err := requireJSONContentType(request); err != nil {
		return err
	}
	if request.Body == nil || request.ContentLength == 0 {
		return nil
	}
	err := decodeJSON(writer, request, target)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	if err := requireJSONContentType(request); err != nil {
		return err
	}
	reader := http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("decode JSON body: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("JSON body must contain one value")
	}
	return nil
}

func requireJSONContentType(request *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	return nil
}

func pathID(request *http.Request) (int64, error) {
	value, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil || value <= 0 {
		return 0, errors.New("message id must be a positive integer")
	}
	return value, nil
}

func queryInt(request *http.Request, name string, fallback int) (int, error) {
	value := request.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func queryUint32(request *http.Request, name string, fallback uint32) (uint32, error) {
	value := request.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be an unsigned 32-bit integer", name)
	}
	return uint32(parsed), nil
}

func queryBool(request *http.Request, name string, fallback bool) (bool, error) {
	value := request.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return parsed, nil
}

func writeServiceError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeAPIError(writer, http.StatusNotFound, "not_found", "record not found")
	case errors.Is(err, ErrOperationBusy):
		writeAPIError(writer, http.StatusConflict, "operation_busy", err.Error())
	default:
		writeAPIError(writer, http.StatusInternalServerError, "operation_failed", err.Error())
	}
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		encoded = []byte(`{"error":{"code":"encoding_failed","message":"response encoding failed"}}`)
	}
	encoded = append(encoded, '\n')
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}
