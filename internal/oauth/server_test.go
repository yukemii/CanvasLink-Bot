package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
)

type fakeGoogleDisconnectStore struct {
	token                *store.GoogleToken
	tokenErr             error
	disconnectErr        error
	disconnectCalled     bool
	disconnectContextErr error
	resetErr             error
	resetCalled          bool
	resetContextErr      error
}

func (f *fakeGoogleDisconnectStore) GetGoogleToken(context.Context, int64) (*store.GoogleToken, error) {
	return f.token, f.tokenErr
}

func (f *fakeGoogleDisconnectStore) DisconnectGoogle(ctx context.Context, _ int64) error {
	f.disconnectCalled = true
	f.disconnectContextErr = ctx.Err()
	return f.disconnectErr
}

func (f *fakeGoogleDisconnectStore) ResetUser(ctx context.Context, _ int64) error {
	f.resetCalled = true
	f.resetContextErr = ctx.Err()
	return f.resetErr
}

type fakeOAuthStateStore struct {
	acquired        bool
	lockErr         error
	releaseErr      error
	createErr       error
	lockHeld        bool
	createCalled    bool
	releaseCalls    int
	createdUserID   int64
	createdState    string
	createdVerifier string
	createdRevision int64
	accountRevision int64
	statePersisted  bool
	beforeLock      func()
}

func (f *fakeOAuthStateStore) GetTelegramAccount(context.Context, int64) (*store.TelegramAccount, error) {
	return &store.TelegramAccount{StateRevision: f.accountRevision}, nil
}

func (f *fakeOAuthStateStore) TryFeedSyncLock(context.Context, int64) (func() error, bool, error) {
	if f.lockErr != nil {
		return nil, false, f.lockErr
	}
	if !f.acquired {
		return nil, false, nil
	}
	if f.beforeLock != nil {
		f.beforeLock()
	}
	f.lockHeld = true
	return func() error {
		f.releaseCalls++
		f.lockHeld = false
		return f.releaseErr
	}, true, nil
}

func (f *fakeOAuthStateStore) CreateOAuthState(
	_ context.Context,
	state string,
	userID int64,
	verifier string,
	_ time.Time,
	expectedStateRevision int64,
) error {
	f.createCalled = true
	f.createdState = state
	f.createdUserID = userID
	f.createdVerifier = verifier
	f.createdRevision = expectedStateRevision
	if !f.lockHeld {
		return errors.New("state created without operation lock")
	}
	if expectedStateRevision != f.accountRevision {
		return store.ErrNotFound
	}
	f.statePersisted = true
	return f.createErr
}

func TestCallbackRejectsNonGET(t *testing.T) {
	t.Parallel()

	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/oauth/callback", nil)
	response := httptest.NewRecorder()

	server.handleCallbackHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestCallbackHandlesProviderDenialWithoutReflectingDetails(t *testing.T) {
	t.Parallel()

	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/oauth/callback?error=access_denied&error_description=%3Cscript%3E", nil)
	response := httptest.NewRecorder()

	server.handleCallbackHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if strings.Contains(response.Body.String(), "<script>") {
		t.Fatalf("response reflected provider-controlled HTML: %s", response.Body.String())
	}
}

func TestPKCEChallengeIsStable(t *testing.T) {
	t.Parallel()

	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := pkceChallenge(verifier); got != want {
		t.Errorf("pkceChallenge() = %q, want %q", got, want)
	}
}

func TestValidOAuthState(t *testing.T) {
	t.Parallel()

	if !validOAuthState("0123456789abcdef0123456789abcdef") {
		t.Fatal("valid generated OAuth state was rejected")
	}
	for _, state := range []string{
		"",
		"short",
		"0123456789ABCDEF0123456789ABCDEF",
		"0123456789abcdef0123456789abcdeg",
		"0123456789abcdef0123456789abcdef0",
	} {
		if validOAuthState(state) {
			t.Errorf("unsafe OAuth state %q was accepted", state)
		}
	}
}

func TestDisconnectDeletesLocallyWhenStoredTokenIsUnreadable(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := &fakeGoogleDisconnectStore{tokenErr: errors.New("decrypt failed")}
	revoked := false

	err := disconnectGoogle(ctx, storage, 42, func(context.Context, string) error {
		revoked = true
		return nil
	})
	if err != nil {
		t.Fatalf("disconnectGoogle() error = %v", err)
	}
	if !storage.disconnectCalled {
		t.Fatal("DisconnectGoogle() was not called after token read failure")
	}
	if storage.disconnectContextErr != nil {
		t.Fatalf("local disconnect inherited cancelled caller context: %v", storage.disconnectContextErr)
	}
	if revoked {
		t.Fatal("provider revocation was attempted with an unreadable token")
	}
}

func TestDisconnectRevokesReadableTokenAfterLocalDelete(t *testing.T) {
	t.Parallel()

	storage := &fakeGoogleDisconnectStore{
		token: &store.GoogleToken{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
		},
	}
	var revokedToken string

	err := disconnectGoogle(context.Background(), storage, 42, func(_ context.Context, token string) error {
		if !storage.disconnectCalled {
			t.Fatal("provider revocation ran before local deletion")
		}
		revokedToken = token
		return errors.New("provider unavailable")
	})
	if err != nil {
		t.Fatalf("disconnectGoogle() error = %v; provider revocation must be best-effort", err)
	}
	if revokedToken != "refresh-token" {
		t.Fatalf("revoked token = %q, want refresh token", revokedToken)
	}
}

func TestDisconnectReturnsLocalDeleteFailureWithoutRevoking(t *testing.T) {
	t.Parallel()

	deleteErr := errors.New("delete failed")
	storage := &fakeGoogleDisconnectStore{
		token:         &store.GoogleToken{RefreshToken: "refresh-token"},
		disconnectErr: deleteErr,
	}
	revoked := false

	err := disconnectGoogle(context.Background(), storage, 42, func(context.Context, string) error {
		revoked = true
		return nil
	})
	if !errors.Is(err, deleteErr) {
		t.Fatalf("disconnectGoogle() error = %v, want %v", err, deleteErr)
	}
	if revoked {
		t.Fatal("provider revocation ran after local deletion failed")
	}
}

func TestOAuthStateCreationHoldsAndReleasesUserLock(t *testing.T) {
	t.Parallel()

	storage := &fakeOAuthStateStore{acquired: true, accountRevision: 7}
	err := createOAuthStateWithLock(
		context.Background(),
		storage,
		"state",
		42,
		"verifier",
		time.Now().Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("createOAuthStateWithLock() error = %v", err)
	}
	if !storage.createCalled || storage.createdUserID != 42 ||
		storage.createdState != "state" || storage.createdVerifier != "verifier" {
		t.Fatalf("created state = user:%d state:%q verifier:%q", storage.createdUserID, storage.createdState, storage.createdVerifier)
	}
	if !storage.statePersisted || storage.createdRevision != 7 {
		t.Fatalf("state persisted=%v revision=%d; want true, 7", storage.statePersisted, storage.createdRevision)
	}
	if storage.lockHeld || storage.releaseCalls != 1 {
		t.Fatalf("lock held=%v release calls=%d; want false, 1", storage.lockHeld, storage.releaseCalls)
	}
}

func TestOAuthStateCreationRefusesBusyUserLock(t *testing.T) {
	t.Parallel()

	storage := &fakeOAuthStateStore{}
	err := createOAuthStateWithLock(
		context.Background(),
		storage,
		"state",
		42,
		"verifier",
		time.Now().Add(time.Minute),
	)
	if err == nil || storage.createCalled {
		t.Fatalf("createOAuthStateWithLock() error=%v createCalled=%v; want busy error and no state", err, storage.createCalled)
	}
}

func TestOAuthStateCreationSurfacesReleaseFailure(t *testing.T) {
	t.Parallel()

	releaseErr := errors.New("unlock failed")
	storage := &fakeOAuthStateStore{acquired: true, releaseErr: releaseErr}
	err := createOAuthStateWithLock(
		context.Background(),
		storage,
		"state",
		42,
		"verifier",
		time.Now().Add(time.Minute),
	)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("createOAuthStateWithLock() error = %v, want %v", err, releaseErr)
	}
}

func TestOAuthStateCreationRejectsRevisionChangedWhileWaitingForLock(t *testing.T) {
	t.Parallel()

	storage := &fakeOAuthStateStore{
		acquired:        true,
		accountRevision: 7,
	}
	storage.beforeLock = func() {
		storage.accountRevision++
	}

	err := createOAuthStateWithLock(
		context.Background(),
		storage,
		"state",
		42,
		"verifier",
		time.Now().Add(time.Minute),
	)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("createOAuthStateWithLock() error = %v, want revision mismatch", err)
	}
	if storage.statePersisted {
		t.Fatal("OAuth state persisted after account revision changed while lock acquisition was delayed")
	}
	if storage.createdRevision != 7 || storage.accountRevision != 8 {
		t.Fatalf("expected revision=%d current revision=%d; want 7, 8", storage.createdRevision, storage.accountRevision)
	}
	if storage.releaseCalls != 1 || storage.lockHeld {
		t.Fatalf("release calls=%d lock held=%v; want 1, false", storage.releaseCalls, storage.lockHeld)
	}
}

func TestResetDeletesLocallyBeforeBestEffortRevocation(t *testing.T) {
	t.Parallel()

	storage := &fakeGoogleDisconnectStore{
		token: &store.GoogleToken{RefreshToken: "refresh-token"},
	}
	var revokedToken string
	err := resetUserAndRevoke(context.Background(), storage, 42, func(_ context.Context, token string) error {
		if !storage.resetCalled {
			t.Fatal("provider revocation ran before local reset")
		}
		revokedToken = token
		return errors.New("provider unavailable")
	})
	if err != nil {
		t.Fatalf("resetUserAndRevoke() error = %v", err)
	}
	if revokedToken != "refresh-token" {
		t.Fatalf("revoked token = %q, want refresh-token", revokedToken)
	}
}

func TestResetStillDeletesWhenStoredTokenIsUnreadable(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := &fakeGoogleDisconnectStore{tokenErr: errors.New("decrypt failed")}
	revoked := false
	err := resetUserAndRevoke(ctx, storage, 42, func(context.Context, string) error {
		revoked = true
		return nil
	})
	if err != nil {
		t.Fatalf("resetUserAndRevoke() error = %v", err)
	}
	if !storage.resetCalled || storage.resetContextErr != nil {
		t.Fatalf("reset called=%v context error=%v; want called with fresh context", storage.resetCalled, storage.resetContextErr)
	}
	if revoked {
		t.Fatal("provider revocation ran with unreadable token")
	}
}

func TestResetFailureDoesNotRevoke(t *testing.T) {
	t.Parallel()

	resetErr := errors.New("reset failed")
	storage := &fakeGoogleDisconnectStore{
		token:    &store.GoogleToken{RefreshToken: "refresh-token"},
		resetErr: resetErr,
	}
	revoked := false
	err := resetUserAndRevoke(context.Background(), storage, 42, func(context.Context, string) error {
		revoked = true
		return nil
	})
	if !errors.Is(err, resetErr) {
		t.Fatalf("resetUserAndRevoke() error = %v, want %v", err, resetErr)
	}
	if revoked {
		t.Fatal("provider revocation ran after local reset failed")
	}
}
