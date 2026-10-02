package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"embed"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codeLifetime    = 2 * time.Minute
	sessionLifetime = 5 * time.Minute
	pollLifetime    = 25 * time.Second
	maxBodyBytes    = 32 << 10
	maxSignals      = 64
	maxSessions     = 10000
)

type signal struct {
	ID   uint64          `json:"id"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
	To   string          `json:"-"`
}

type session struct {
	id         string
	code       string
	codeUntil  time.Time
	expires    time.Time
	joined     bool
	hostToken  [32]byte
	peerToken  [32]byte
	messages   []signal
	nextID     uint64
	notify     chan struct{}
}

type bucket struct {
	start time.Time
	count int
}

type service struct {
	mu       sync.Mutex
	sessions map[string]*session
	codes    map[string]string
	limits   map[string]bucket
}

type createResponse struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in_seconds"`
}

type joinRequest struct {
	Code string `json:"code"`
}

type joinResponse struct {
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in_seconds"`
}

type sendRequest struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type errorResponse struct {
	Error string `json:"error""
}

//go:embed web/index.html
var appFS embed.FS

func newService() *service {
	return &service{
		sessions: make(map[string]*session),
		codes:    make(map[string]string),
		limits:   make(map[string]bucket),
	}
}

func main() {
	svc := newService()
	go svc.cleanupLoop()

	addr := strings.TrimSpace(getenv("ADDR", ":8080"))
	server := &http.Server{
		Addr:              addr,
		Handler:           svc,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Avoid printing addresses or request data. The process supervisor can
		// report a generic non-zero exit status.
		panic("pairing service failed to start")
	}
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.applyCORS(w, r) {
		return
	}

	switch {
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		page, err := appFS.ReadFile("web/index.html")
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(page)
	case r.URL.Path == "/v1/config":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"stun_url": getenv("STUN_URL", "")})
	case r.URL.Path == "/healthz":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.URL.Path == "/v1/pairings" && r.Method == http.MethodPost:
		s.create(w, r)
	case r.URL.Path == "/v1/pairings/join" && r.Method == http.MethodPost:
		s.join(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/pairings/"):
		s.pairingRoute(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *service) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	allowed := false
	for _, value := range strings.Split(getenv("ALLOWED_ORIGINS", "http://tauri.localhost,http://localhost:8080,http://127.0.0.1:8080"), ",") {
		if strings.TrimSpace(value) == origin {
			allowed = true
			break
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "origin_not_allowed")
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

func (s *service) create(w http.ResponseWriter, r *http.Request) {
	if !s.allow(clientKey(r, "create"), 10, time.Minute) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}

	id, err := randomToken(18)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	token, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	hash := sha256.Sum256([]byte(token))

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= maxSessions {
		writeError(w, http.StatusServiceUnavailable, "busy")
		return
	}

	var code string
	for attempt := 0; attempt < 10; attempt++ {
		n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "unavailable")
			return
		}
		candidate := fmt.Sprintf("%09d", n.Int64())
		if _, exists := s.codes[candidate]; !exists {
			code = candidate
			break
		}
	}
	if code == "" {
		writeError(w, http.StatusServiceUnavailable, "busy")
		return
	}

	now := time.Now()
	sess := &session{
		id:        id,
		code:      code,
		codeUntil: now.Add(codeLifetime),
		expires:   now.Add(sessionLifetime),
		hostToken: hash,
		notify:    make(chan struct{}),
	}
	s.sessions[id] = sess
	s.codes[code] = id

	writeJSON(w, http.StatusCreated, createResponse{
		SessionID: id,
		Code:      code,
		Token:     token,
		ExpiresIn: int(codeLifetime.Seconds()),
	})
}

func (s *service) join(w http.ResponseWriter, r *http.Request) {
	if !s.allow(clientKey(r, "join"), 20, time.Minute) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var req joinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	code, ok := normalizeCode(req.Code)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_code")
		return
	}

	token, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	hash := sha256.Sum256([]byte(token))

	s.mu.Lock()
	defer s.mu.Unlock()
	id, exists := s.codes[code]
	if !exists {
		writeError(w, http.StatusNotFound, "pairing_not_found")
		return
	}
	sess := s.sessions[id]
	if sess == nil || time.Now().After(sess.codeUntil) || sess.joined || time.Now().After(sess.expires) {
		delete(s.codes, code)
		if sess != nil && time.Now().After(sess.expires) {
			delete(s.sessions, id)
		}
		writeError(w, http.StatusNotFound, "pairing_not_found")
		return
	}

	sess.joined = true
	sess.peerToken = hash
	delete(s.codes, code)
	sess.nextID++
	sess.messages = append(sess.messages, signal{ID: sess.nextID, Kind: "peer_joined", Data: json.RawMessage(`{}`), To: "host"})
	s.pingLocked(sess)

	writeJSON(w, http.StatusOK, joinResponse{
		SessionID: id,
		Token:     token,
		ExpiresIn: int(time.Until(sess.expires).Seconds()),
	})
}

func (s *service) pairingRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/pairings/")
	parts := strings.Split(path, "/")
	if len(parts) == 1 && parts[0] != "" {
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		s.destroy(w, r, parts[0])
		return
	}
	if len(parts) != 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	id, action := parts[0], parts[1]

	switch action {
	case "messages":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		s.postSignal(w, r, id)
	case "events":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		s.poll(w, r, id)
	case "":
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		s.destroy(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *service) postSignal(w http.ResponseWriter, r *http.Request, id string) {
	var req sendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Data) == 0 || !json.Valid(req.Data) || len(req.Data) > maxBodyBytes/2 {
		writeError(w, http.StatusBadRequest, "invalid_message")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sess, role, ok := s.authenticatedLocked(w, r, id)
	if !ok {
		return
	}
	if !sess.joined {
		writeError(w, http.StatusConflict, "peer_not_joined")
		return
	}
	to, allowed := destination(role, req.Kind)
	if !allowed {
		writeError(w, http.StatusBadRequest, "invalid_message_kind")
		return
	}
	if len(sess.messages) >= maxSignals {
		writeError(w, http.StatusTooManyRequests, "signaling_queue_full")
		return
	}

	sess.nextID++
	sess.messages = append(sess.messages, signal{
		ID:   sess.nextID,
		Kind: req.Kind,
		Data: append(json.RawMessage(nil), req.Data...),
		To:   to,
	})
	s.pingLocked(sess)
	w.WriteHeader(http.StatusAccepted)
}

func destination(role, kind string) (string, bool) {
	switch kind {
	case "offer":
		return "peer", role == "host"
	case "answer":
		return "host", role == "peer"
	case "candidate", "verify":
		if role == "host" {
			return "peer", true
		}
		return "host", role == "peer"
	default:
		return "", false
	}
}

func (s *service) poll(w http.ResponseWriter, r *http.Request, id string) {
	after := uint64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
		after = value
	}

	deadline := time.NewTimer(pollLifetime)
	defer deadline.Stop()

	for {
		s.mu.Lock()
		sess, role, ok := s.authenticatedLocked(w, r, id)
		if !ok {
			s.mu.Unlock()
			return
		}
		if !sess.joined && role == "peer" {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "peer_not_joined")
			return
		}

		events := make([]signal, 0)
		for _, msg := range sess.messages {
			if msg.ID > after && msg.To == role {
				events = append(events, msg)
			}
		}
		if len(events) > 0 {
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"events": events})
			return
		}
		notify := sess.notify
		s.mu.Unlock()

		select {
		case <-notify:
			continue
		case <-deadline.C:
			writeJSON(w, http.StatusOK, map[string]any{"events": []signal{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *service) destroy(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, ok := s.authenticatedLocked(w, r, id)
	if !ok {
		return
	}
	s.removeLocked(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) authenticatedLocked(w http.ResponseWriter, r *http.Request, id string) (*session, string, bool) {
	sess := s.sessions[id]
	if sess == nil || time.Now().After(sess.expires) {
		if sess != nil {
			s.removeLocked(id)
		}
		writeError(w, http.StatusNotFound, "pairing_not_found")
		return nil, "", false
	}

	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, "", false
	}
	provided := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(header, prefix))))
	if subtle.ConstantTimeCompare(provided[:], sess.hostToken[:]) == 1 {
		return sess, "host", true
	}
	if sess.joined && subtle.ConstantTimeCompare(provided[:], sess.peerToken[:]) == 1 {
		return sess, "peer", true
	}
	writeError(w, http.StatusUnauthorized, "unauthorized")
	return nil, "", false
}

func (s *service) allow(key string, limit int, window time.Duration) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.limits[key]
	if now.Sub(entry.start) >= window {
		entry = bucket{start: now}
	}
	if entry.count >= limit {
		s.limits[key] = entry
		return false
	}
	entry.count++
	s.limits[key] = entry
	return true
}

func (s *service) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for id, sess := range s.sessions {
			if now.After(sess.expires) {
				s.removeLocked(id)
			}
		}
		for code, id := range s.codes {
			sess := s.sessions[id]
			if sess == nil || now.After(sess.codeUntil) || sess.joined {
				delete(s.codes, code)
			}
		}
		for key, entry := range s.limits {
			if now.Sub(entry.start) > 2*time.Minute {
				delete(s.limits, key)
			}
		}
		s.mu.Unlock()
	}
}

func (s *service) removeLocked(id string) {
	sess := s.sessions[id]
	if sess == nil {
		return
	}
	delete(s.codes, sess.code)
	delete(s.sessions, id)
	s.pingLocked(sess)
}

func (s *service) pingLocked(sess *session) {
	close(sess.notify)
	sess.notify = make(chan struct{})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func normalizeCode(raw string) (string, bool) {
	var digits strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '·':
			continue
		default:
			return "", false
		}
	}
	if digits.Len() != 9 {
		return "", false
	}
	return digits.String(), true
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func clientKey(r *http.Request, action string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return action + ":" + host
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Error: code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
