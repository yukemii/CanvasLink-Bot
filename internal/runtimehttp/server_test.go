package runtimehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"google.golang.org/api/idtoken"
)

type memoryStore struct {
	mu   sync.Mutex
	busy bool
	seen map[int]bool
	fail bool
}

func (s *memoryStore) TryRuntimeLock(context.Context, string) (func() error, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, false, errors.New("db down")
	}
	if s.busy {
		return nil, false, nil
	}
	s.busy = true
	return func() error { s.mu.Lock(); defer s.mu.Unlock(); s.busy = false; return nil }, true, nil
}
func (s *memoryStore) ClaimTelegramUpdate(_ context.Context, id int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[id] {
		return false, nil
	}
	s.seen[id] = true
	return true, nil
}
func testServer(t *testing.T) (*Server, *memoryStore, *atomic.Int64) {
	t.Helper()
	db := &memoryStore{seen: map[int]bool{}}
	calls := &atomic.Int64{}
	s, err := New(db, func(context.Context, tgbotapi.Update) { calls.Add(1) }, func(context.Context) error { calls.Add(1); return nil }, Options{WebhookSecret: strings.Repeat("x", 32), SchedulerAudience: "https://service.example", SchedulerEmail: "scheduler@test.iam.gserviceaccount.com"})
	if err != nil {
		t.Fatal(err)
	}
	return s, db, calls
}

const testUpdate = `{"update_id":123,"message":{"message_id":1,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"hello"}}`

func webhookRequest(s *Server, method, secret, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/telegram/webhook", strings.NewReader(body))
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	w := httptest.NewRecorder()
	s.webhook(w, r)
	return w
}
func TestWebhookAuthenticationAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, method, secret, body string
		code                       int
	}{
		{"missing secret", "POST", "", testUpdate, 401}, {"wrong secret", "POST", "bad", testUpdate, 401},
		{"method", "GET", strings.Repeat("x", 32), testUpdate, 405},
		{"malformed", "POST", strings.Repeat("x", 32), "{", 400},
		{"trailing data", "POST", strings.Repeat("x", 32), testUpdate + " {}", 400},
		{"oversize", "POST", strings.Repeat("x", 32), `{"update_id":1,"padding":"` + strings.Repeat("x", 1<<20) + `"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, calls := testServer(t)
			w := webhookRequest(s, tc.method, tc.secret, tc.body)
			if w.Code != tc.code || calls.Load() != 0 {
				t.Fatalf("code=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}
func TestWebhookRetryAcrossInstances(t *testing.T) {
	s, db, calls := testServer(t)
	if w := webhookRequest(s, "POST", s.options.WebhookSecret, testUpdate); w.Code != 204 {
		t.Fatal(w.Code)
	}
	other := *s
	other.store = db
	if w := webhookRequest(&other, "POST", s.options.WebhookSecret, testUpdate); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate dispatched: %d", calls.Load())
	}
}
func TestWebhookBusyDoesNotConsumeUpdate(t *testing.T) {
	s, db, calls := testServer(t)
	db.busy = true
	if w := webhookRequest(s, "POST", s.options.WebhookSecret, testUpdate); w.Code != 503 {
		t.Fatal(w.Code)
	}
	db.busy = false
	if w := webhookRequest(s, "POST", s.options.WebhookSecret, testUpdate); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if calls.Load() != 1 {
		t.Fatal("retry lost")
	}
}
func TestWebhookDoesNotAcknowledgeUntilHandlerFinishes(t *testing.T) {
	s, _, _ := testServer(t)
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.handle = func(context.Context, tgbotapi.Update) { close(entered); <-finish }
	go func() { webhookRequest(s, "POST", s.options.WebhookSecret, testUpdate); close(done) }()
	<-entered
	select {
	case <-done:
		t.Fatal("early acknowledgement")
	default:
	}
	close(finish)
	<-done
}
func TestSchedulerIdentityAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, email string
		verified            bool
		verifyErr, tickErr  bool
		want                int
	}{
		{"valid", "https://accounts.google.com", "scheduler@test.iam.gserviceaccount.com", true, false, false, 204},
		{"other principal", "https://accounts.google.com", "other@test.iam.gserviceaccount.com", true, false, false, 401},
		{"unverified", "https://accounts.google.com", "scheduler@test.iam.gserviceaccount.com", false, false, false, 401},
		{"issuer", "https://attacker.example", "scheduler@test.iam.gserviceaccount.com", true, false, false, 401},
		{"invalid signature", "https://accounts.google.com", "scheduler@test.iam.gserviceaccount.com", true, true, false, 401},
		{"worker fails", "https://accounts.google.com", "scheduler@test.iam.gserviceaccount.com", true, false, true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, calls := testServer(t)
			s.verify = func(_ context.Context, token, audience string) (*idtoken.Payload, error) {
				if audience != s.options.SchedulerAudience || token != "signed" {
					t.Fatal("incorrect validation input")
				}
				if tc.verifyErr {
					return nil, errors.New("bad token")
				}
				return &idtoken.Payload{Issuer: tc.issuer, Claims: map[string]interface{}{"email": tc.email, "email_verified": tc.verified}}, nil
			}
			if tc.tickErr {
				s.tick = func(context.Context) error { return errors.New("failed") }
			}
			r := httptest.NewRequest("POST", "/internal/tick", nil)
			r.Header.Set("Authorization", "Bearer signed")
			w := httptest.NewRecorder()
			s.scheduled(w, r)
			if w.Code != tc.want {
				t.Fatalf("code=%d", w.Code)
			}
			if tc.want == 401 && calls.Load() != 0 {
				t.Fatal("unauthenticated execution")
			}
		})
	}
	s, _, calls := testServer(t)
	w := httptest.NewRecorder()
	s.scheduled(w, httptest.NewRequest(http.MethodPost, "/internal/tick", nil))
	if w.Code != 401 || calls.Load() != 0 {
		t.Fatal("missing auth accepted")
	}
}

func TestPublicHealthRoute(t *testing.T) {
	s, _, calls := testServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	for _, tc := range []struct {
		path, method string
		want         int
	}{{"/health", "GET", 200}, {"/health", "POST", 405}, {"/healthz", "GET", 404}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("health route performed user work")
	}
}
