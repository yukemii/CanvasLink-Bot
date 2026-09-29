// Package runtimehttp hosts authenticated, synchronous Cloud Run entrypoints.
package runtimehttp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"google.golang.org/api/idtoken"
)

type Storage interface {
	TryRuntimeLock(context.Context, string) (func() error, bool, error)
	ClaimTelegramUpdate(context.Context, int) (bool, error)
}
type Options struct{ WebhookSecret, SchedulerAudience, SchedulerEmail string }
type Server struct {
	store   Storage
	handle  func(context.Context, tgbotapi.Update)
	tick    func(context.Context) error
	options Options
	verify  func(context.Context, string, string) (*idtoken.Payload, error)
}

func New(db Storage, handle func(context.Context, tgbotapi.Update), tick func(context.Context) error, options Options) (*Server, error) {
	if db == nil || handle == nil || tick == nil || len(options.WebhookSecret) < 32 || options.SchedulerAudience == "" || options.SchedulerEmail == "" {
		return nil, errors.New("incomplete webhook runtime configuration")
	}
	validator, err := idtoken.NewValidator(context.Background())
	if err != nil {
		return nil, err
	}
	return &Server{store: db, handle: handle, tick: tick, options: options, verify: validator.Validate}, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/telegram/webhook", s.webhook)
	mux.HandleFunc("/internal/tick", s.scheduled)
	// Google Frontend reserves /healthz; use a routable public path.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func post(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return false
	}
	return true
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(s.options.WebhookSecret)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	var update tgbotapi.Update
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&update); err != nil || update.UpdateID <= 0 {
		http.Error(w, "invalid update", 400)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "invalid update", 400)
		return
	}
	var user int64
	switch {
	case update.CallbackQuery != nil && update.CallbackQuery.From != nil:
		user = update.CallbackQuery.From.ID
	case update.Message != nil && update.Message.From != nil:
		user = update.Message.From.ID
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if user <= 0 {
		http.Error(w, "invalid sender", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	release, acquired, err := s.store.TryRuntimeLock(ctx, "telegram:"+strconv.FormatInt(user, 10))
	if err != nil || !acquired {
		http.Error(w, "retry later", 503)
		return
	}
	defer func() {
		if err := release(); err != nil {
			log.Printf("release Telegram dispatch lock: %v", err)
		}
	}()
	claimed, err := s.store.ClaimTelegramUpdate(ctx, update.UpdateID)
	if err != nil {
		http.Error(w, "retry later", 503)
		return
	}
	if claimed {
		s.handle(ctx, update)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scheduled(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 240*time.Second)
	defer cancel()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(auth) > 16384 {
		http.Error(w, "unauthorized", 401)
		return
	}
	payload, err := s.verify(ctx, strings.TrimPrefix(auth, "Bearer "), s.options.SchedulerAudience)
	if err != nil || payload == nil || (payload.Issuer != "https://accounts.google.com" && payload.Issuer != "accounts.google.com") || payload.Claims["email"] != s.options.SchedulerEmail || payload.Claims["email_verified"] != true {
		http.Error(w, "unauthorized", 401)
		return
	}
	if err := s.tick(ctx); err != nil {
		log.Printf("scheduled cycle failed: %v", err)
		http.Error(w, "cycle failed", 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
