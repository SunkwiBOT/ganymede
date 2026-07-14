package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withTwitchTestServers(t *testing.T, authHandler http.HandlerFunc, apiHandler http.HandlerFunc) {
	t.Helper()

	authServer := httptest.NewServer(authHandler)
	apiServer := httptest.NewServer(apiHandler)
	t.Cleanup(func() {
		authServer.Close()
		apiServer.Close()
	})

	previousAuthURL := TwitchAuthUrl
	previousAPIURL := TwitchApiUrl
	TwitchAuthUrl = authServer.URL
	TwitchApiUrl = apiServer.URL
	t.Cleanup(func() {
		TwitchAuthUrl = previousAuthURL
		TwitchApiUrl = previousAPIURL
	})
}

func writeAuthResponse(t *testing.T, w http.ResponseWriter, token string, expiresIn int) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(AuthTokenResponse{
		AccessToken: token,
		ExpiresIn:   expiresIn,
		TokenType:   "bearer",
	}); err != nil {
		t.Fatalf("failed to write auth response: %v", err)
	}
}

func TestTwitchConnectionAuthenticateStoresTokenAndExpiry(t *testing.T) {
	withTwitchTestServers(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST auth request, got %s", r.Method)
		}
		if got := r.URL.Query().Get("client_id"); got != "client-id" {
			t.Fatalf("expected client_id query, got %q", got)
		}
		if got := r.URL.Query().Get("client_secret"); got != "client-secret" {
			t.Fatalf("expected client_secret query, got %q", got)
		}
		if got := r.URL.Query().Get("grant_type"); got != "client_credentials" {
			t.Fatalf("expected grant_type query, got %q", got)
		}
		writeAuthResponse(t, w, "stored-token", 3600)
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected Helix request")
	})

	conn := &TwitchConnection{
		ClientId:     "client-id",
		ClientSecret: "client-secret",
	}
	before := time.Now()

	info, err := conn.Authenticate(context.Background())
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}

	if info.AccessToken != "stored-token" {
		t.Fatalf("expected connection info token stored-token, got %q", info.AccessToken)
	}

	conn.mu.RLock()
	defer conn.mu.RUnlock()

	if conn.AccessToken != "stored-token" {
		t.Fatalf("expected stored token, got %q", conn.AccessToken)
	}
	if !conn.tokenExpiresAt.After(before.Add(59 * time.Minute)) {
		t.Fatalf("expected expiry about an hour in the future, got %s", conn.tokenExpiresAt)
	}
}

func withTwitchGQLTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	previousGQLURL := TwitchGQLUrl
	TwitchGQLUrl = server.URL
	t.Cleanup(func() {
		TwitchGQLUrl = previousGQLURL
	})
}

func writeTwitchGQLVideoResponse(t *testing.T, w http.ResponseWriter, broadcastType string, seekPreviewsURL string, ownerLogin string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{
			"video": map[string]any{
				"broadcastType":   broadcastType,
				"createdAt":       "2026-07-14T12:00:00Z",
				"seekPreviewsURL": seekPreviewsURL,
				"owner": map[string]any{
					"login": ownerLogin,
				},
			},
		},
	}); err != nil {
		t.Fatalf("failed to write gql video response: %v", err)
	}
}

func writeTwitchVODMediaPlaylist(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if _, err := w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:10.0,\nsegment000.ts\n#EXT-X-ENDLIST\n")); err != nil {
		t.Fatalf("failed to write media playlist: %v", err)
	}
}

func TestParsePlaybackAccessTokenResponseAcceptsStreamToken(t *testing.T) {
	token, err := parsePlaybackAccessTokenResponse([]byte(`{
		"data": {
			"streamPlaybackAccessToken": {
				"value": "stream-token",
				"signature": "stream-signature"
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parsePlaybackAccessTokenResponse returned error: %v", err)
	}
	if token.Value != "stream-token" {
		t.Fatalf("expected stream token, got %q", token.Value)
	}
	if token.Signature != "stream-signature" {
		t.Fatalf("expected stream signature, got %q", token.Signature)
	}
}

func TestGetVideoPlaybackBuildsArchivePlaylistFromSeekPreviewsURL(t *testing.T) {
	const videoID = "123456"

	cdnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/storage123/chunked/index-dvr.m3u8" {
			writeTwitchVODMediaPlaylist(t, w)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(cdnServer.Close)

	withTwitchGQLTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected unauthenticated GQL metadata request, got %q", got)
		}
		writeTwitchGQLVideoResponse(t, w, "archive", cdnServer.URL+"/storage123/storyboards/storyboard.jpg", "channel")
	})

	playlist, err := (&TwitchConnection{}).GetVideoPlayback(context.Background(), videoID)
	if err != nil {
		t.Fatalf("GetVideoPlayback returned error: %v", err)
	}
	if len(playlist.Variants) != 1 {
		t.Fatalf("expected 1 valid variant, got %d", len(playlist.Variants))
	}
	variant := playlist.Variants[0]
	if variant.Video != "chunked" {
		t.Fatalf("expected chunked variant, got %q", variant.Video)
	}
	if want := cdnServer.URL + "/storage123/chunked/index-dvr.m3u8"; variant.URI != want {
		t.Fatalf("expected variant URI %q, got %q", want, variant.URI)
	}
}

func TestGetVideoPlaybackBuildsHighlightPlaylistURL(t *testing.T) {
	const videoID = "987654"

	cdnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/highlight-storage/chunked/highlight-"+videoID+".m3u8" {
			writeTwitchVODMediaPlaylist(t, w)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(cdnServer.Close)

	withTwitchGQLTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeTwitchGQLVideoResponse(t, w, "highlight", cdnServer.URL+"/highlight-storage/storyboards/storyboard.jpg", "channel")
	})

	playlist, err := (&TwitchConnection{}).GetVideoPlayback(context.Background(), videoID)
	if err != nil {
		t.Fatalf("GetVideoPlayback returned error: %v", err)
	}
	if len(playlist.Variants) != 1 {
		t.Fatalf("expected 1 valid variant, got %d", len(playlist.Variants))
	}
	if want := cdnServer.URL + "/highlight-storage/chunked/highlight-" + videoID + ".m3u8"; playlist.Variants[0].URI != want {
		t.Fatalf("expected highlight URI %q, got %q", want, playlist.Variants[0].URI)
	}
}

func TestGetVideoPlaybackBuildsUploadOwnerPlaylistURL(t *testing.T) {
	const videoID = "246810"
	var sawOwnerPath atomic.Bool

	cdnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/uploader/"+videoID+"/upload-storage/chunked/index-dvr.m3u8" {
			sawOwnerPath.Store(true)
			writeTwitchVODMediaPlaylist(t, w)
			return
		}
		if strings.Contains(r.URL.Path, "/upload-storage/chunked/index-dvr.m3u8") {
			t.Fatalf("expected owner upload path before storage-only fallback, got %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(cdnServer.Close)

	withTwitchGQLTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeTwitchGQLVideoResponse(t, w, "upload", cdnServer.URL+"/upload-storage/storyboards/storyboard.jpg", "uploader")
	})

	playlist, err := (&TwitchConnection{}).GetVideoPlayback(context.Background(), videoID)
	if err != nil {
		t.Fatalf("GetVideoPlayback returned error: %v", err)
	}
	if !sawOwnerPath.Load() {
		t.Fatal("expected owner upload playlist path to be requested")
	}
	if len(playlist.Variants) != 1 {
		t.Fatalf("expected 1 valid variant, got %d", len(playlist.Variants))
	}
	if want := cdnServer.URL + "/uploader/" + videoID + "/upload-storage/chunked/index-dvr.m3u8"; playlist.Variants[0].URI != want {
		t.Fatalf("expected upload URI %q, got %q", want, playlist.Variants[0].URI)
	}
}

func TestTwitchMakeHTTPRequestUsesCurrentToken(t *testing.T) {
	withTwitchTestServers(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected auth request")
	}, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Client-ID"); got != "client-id" {
			t.Fatalf("expected Client-ID client-id, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer current-token" {
			t.Fatalf("expected current token authorization, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	conn := &TwitchConnection{
		ClientId:       "client-id",
		ClientSecret:   "client-secret",
		AccessToken:    "current-token",
		tokenExpiresAt: time.Now().Add(time.Hour),
	}

	body, err := conn.twitchMakeHTTPRequest(context.Background(), http.MethodGet, "users", url.Values{"login": []string{"test"}}, nil)
	if err != nil {
		t.Fatalf("twitchMakeHTTPRequest returned error: %v", err)
	}
	if string(body) != `{"data":[]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestTwitchMakeHTTPRequestRefreshesNearExpiredToken(t *testing.T) {
	var authRequests atomic.Int32

	withTwitchTestServers(t, func(w http.ResponseWriter, r *http.Request) {
		authRequests.Add(1)
		writeAuthResponse(t, w, "fresh-token", 3600)
	}, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer fresh-token" {
			t.Fatalf("expected fresh token authorization, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	conn := &TwitchConnection{
		ClientId:       "client-id",
		ClientSecret:   "client-secret",
		AccessToken:    "stale-token",
		tokenExpiresAt: time.Now().Add(time.Minute),
	}

	if _, err := conn.twitchMakeHTTPRequest(context.Background(), http.MethodGet, "users", nil, nil); err != nil {
		t.Fatalf("twitchMakeHTTPRequest returned error: %v", err)
	}
	if got := authRequests.Load(); got != 1 {
		t.Fatalf("expected 1 auth request, got %d", got)
	}
}

func TestTwitchMakeHTTPRequestRefreshesAndRetriesAfterUnauthorized(t *testing.T) {
	var authRequests atomic.Int32
	var apiRequests atomic.Int32

	withTwitchTestServers(t, func(w http.ResponseWriter, r *http.Request) {
		request := authRequests.Add(1)
		if request == 1 {
			writeAuthResponse(t, w, "initial-token", 3600)
			return
		}
		writeAuthResponse(t, w, "retry-token", 3600)
	}, func(w http.ResponseWriter, r *http.Request) {
		request := apiRequests.Add(1)
		switch request {
		case 1:
			if got := r.Header.Get("Authorization"); got != "Bearer initial-token" {
				t.Fatalf("expected initial token authorization, got %q", got)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case 2:
			if got := r.Header.Get("Authorization"); got != "Bearer retry-token" {
				t.Fatalf("expected retry token authorization, got %q", got)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			t.Fatalf("unexpected API request %d", request)
		}
	})

	conn := &TwitchConnection{
		ClientId:     "client-id",
		ClientSecret: "client-secret",
	}

	body, err := conn.twitchMakeHTTPRequest(context.Background(), http.MethodGet, "users", nil, nil)
	if err != nil {
		t.Fatalf("twitchMakeHTTPRequest returned error: %v", err)
	}
	if string(body) != `{"data":[]}` {
		t.Fatalf("unexpected body: %s", body)
	}
	if got := authRequests.Load(); got != 2 {
		t.Fatalf("expected 2 auth requests, got %d", got)
	}
	if got := apiRequests.Load(); got != 2 {
		t.Fatalf("expected 2 API requests, got %d", got)
	}
}
