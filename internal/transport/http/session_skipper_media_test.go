package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	session "github.com/canidam/echo-scs-session"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zibbp/ganymede/internal/config"
)

func TestNewSessionSkipperMediaRoutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path         string
		requireLogin bool
		skip         bool
	}{
		{"/data/videos/channel/vod/vod-video.mp4", true, false},
		{"/data/temp/stream_uuid-video_hls0/stream-video.m3u8", true, false},
		{"/data/videos-other/file.mp4", true, true},
		{"/data/videos", true, true},
		{"/_next/static/chunks/a.js", true, true},
		{"/api/v1/auth/me", true, false},
		{"/riverui/", true, false},
		{"/data/videos/channel/vod/vod-video.mp4", false, true},
		{"/data/temp/stream_uuid-video_hls0/stream-video.m3u8", false, true},
		{"/api/v1/auth/me", false, false},
	}
	e := echo.New()
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			skipper := newSessionSkipper(config.EnvConfig{
				VideosDir:    "/data/videos/",
				TempDir:      "/data/temp",
				RequireLogin: tc.requireLogin,
			})
			c := e.NewContext(httptest.NewRequest(http.MethodGet, tc.path, nil), httptest.NewRecorder())
			assert.Equal(t, tc.skip, skipper(c))
		})
	}
}

func TestSessionMiddlewareLoadsProtectedMediaSession(t *testing.T) {
	t.Parallel()
	sm, store := newTestSessionManager(t)
	token := seedSession(t, sm, store)

	var userID string
	e := echo.New()
	e.Use(session.LoadAndSaveWithConfig(session.Config{
		SessionManager: sm,
		Skipper: newSessionSkipper(config.EnvConfig{
			VideosDir:    "/data/videos",
			TempDir:      "/data/temp",
			RequireLogin: true,
		}),
	}))
	e.GET("/data/videos/*", func(c echo.Context) error {
		userID = sm.GetString(c.Request().Context(), "user_id")
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/data/videos/channel/vod/vod-video.mp4", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "user-1", userID)
	assert.EqualValues(t, 1, store.finds.Load(), "finds")
}
