package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	session "github.com/canidam/echo-scs-session"
	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
	echoSwagger "github.com/swaggo/echo-swagger"
	_ "github.com/zibbp/ganymede/docs"
	"github.com/zibbp/ganymede/internal/api_key"
	"github.com/zibbp/ganymede/internal/config"
	"github.com/zibbp/ganymede/internal/database"
	"github.com/zibbp/ganymede/internal/platform"
	"github.com/zibbp/ganymede/internal/utils"
	"riverqueue.com/riverui"
)

type Services struct {
	AuthService         AuthService
	ChannelService      ChannelService
	VodService          VodService
	QueueService        QueueService
	ArchiveService      ArchiveService
	AdminService        AdminService
	UserService         UserService
	LiveService         LiveService
	PlaybackService     PlaybackService
	MetricsService      MetricsService
	PlaylistService     PlaylistService
	TaskService         TaskService
	ChapterService      ChapterService
	CategoryService     CategoryService
	BlockedVideoService BlockedVideoService
	NotificationService NotificationService
	ApiKeyService       ApiKeyService
	PlatformTwitch      platform.Platform
}

type Handler struct {
	Server         *echo.Echo
	Service        Services
	SessionManager *scs.SessionManager
	RiverUIServer  *riverui.Handler
}

var sessionManager *scs.SessionManager

// apiKeyService is the package-level ApiKeyService used by the auth
// middleware. It is wired in NewHandler, mirroring the sessionManager
// pattern above so middleware functions stay parameter-free and chain
// cleanly with Echo's middleware signature.
var apiKeyService *api_key.Service

func NewHandler(database *database.Database, authService AuthService, channelService ChannelService, vodService VodService, queueService QueueService, archiveService ArchiveService, adminService AdminService, userService UserService, liveService LiveService, playbackService PlaybackService, metricsService MetricsService, playlistService PlaylistService, taskService TaskService, chapterService ChapterService, categoryService CategoryService, blockedVideoService BlockedVideoService, notificationService NotificationService, apiKeySvc *api_key.Service, platformTwitch platform.Platform, riverUIServer *riverui.Handler) *Handler {
	log.Debug().Msg("creating route handler")
	envConfig := config.GetEnvConfig()

	// Stash the ApiKeyService at package scope so the auth middleware
	// (which has the plain echo.MiddlewareFunc signature) can reach it
	// without each route having to wrap a closure.
	apiKeyService = apiKeySvc

	sessionManager = scs.New()
	sessionManager.Store = pgxstore.New(database.ConnPool)
	// 30 days session lifetime
	sessionManager.Lifetime = (24 * time.Hour) * 30
	// Expire session if no activity for 7 days
	sessionManager.IdleTimeout = (24 * time.Hour) * 7

	h := &Handler{
		Server: echo.New(),
		Service: Services{
			AuthService:         authService,
			ChannelService:      channelService,
			VodService:          vodService,
			QueueService:        queueService,
			ArchiveService:      archiveService,
			AdminService:        adminService,
			UserService:         userService,
			LiveService:         liveService,
			PlaybackService:     playbackService,
			MetricsService:      metricsService,
			PlaylistService:     playlistService,
			TaskService:         taskService,
			ChapterService:      chapterService,
			CategoryService:     categoryService,
			BlockedVideoService: blockedVideoService,
			NotificationService: notificationService,
			ApiKeyService:       apiKeySvc,
			PlatformTwitch:      platformTwitch,
		},
		SessionManager: sessionManager,
		RiverUIServer:  riverUIServer,
	}

	// Enable gzip compression for API routes only.
	//
	// We use HasPrefix("/api/") rather than Contains("/api") because the
	// catch-all at the bottom of mapRoutes proxies every non-matching
	// path to the Next.js frontend, which serves its own gzipped
	// responses. A frontend path that happens to contain the substring
	// "api" (e.g. /admin/api-keys) was being double-gzipped: Next.js
	// gzipped once, Echo gzipped again, and the browser decoded only the
	// outer layer — leaving raw gzip bytes as the page body.
	h.Server.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		Skipper: func(c echo.Context) bool {
			return !strings.HasPrefix(c.Request().URL.Path, "/api/")
		},
	}))

	// Use sessions
	h.Server.Use(session.LoadAndSave(sessionManager))

	// Middleware
	h.Server.Validator = &utils.CustomValidator{Validator: validator.New()}

	h.Server.HideBanner = true

	// If frontend is external then allow cors
	// AllowOriginFunc reflects the request origin so credentials work (browsers
	// reject Access-Control-Allow-Origin: * with credentials).
	h.Server.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOriginFunc: func(origin string) (bool, error) {
			return true, nil
		},
		AllowMethods:     []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete},
		AllowCredentials: true,
	}))

	// Enable request logging in debug
	if envConfig.DEBUG {
		logger := log.Logger
		h.Server.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
			LogURI:    true,
			LogStatus: true,
			LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
				if !strings.Contains(v.URI, "/api") {
					return nil
				}
				logger.Info().
					Str("URI", v.URI).
					Int("status", v.Status).
					Msg("request")

				return nil
			},
		}))
	}

	h.mapRoutes()

	return h
}

func (h *Handler) mapRoutes() {
	log.Debug().Msg("mapping routes")

	// Basic health route
	h.Server.GET("/health", func(c echo.Context) error {
		return c.String(200, "OK")
	})

	// Setup Prometheus metrics route
	h.Server.GET("/metrics", func(c echo.Context) error {
		r, err := h.GatherMetrics()
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}

		handler := promhttp.HandlerFor(r, promhttp.HandlerOpts{})
		handler.ServeHTTP(c.Response(), c.Request())
		return nil
	})

	// Static files if not using nginx
	env := config.GetEnvConfig()
	// Use one handler for both GET + HEAD
	videosH := echo.WrapHandler(http.StripPrefix(env.VideosDir, http.FileServer(http.Dir(env.VideosDir))))
	tempH := echo.WrapHandler(http.StripPrefix(env.TempDir, http.FileServer(http.Dir(env.TempDir))))

	h.Server.GET(env.VideosDir+"/*", videosH, RequireLoginMiddleware)
	h.Server.HEAD(env.VideosDir+"/*", videosH, RequireLoginMiddleware)

	h.Server.GET(env.TempDir+"/*", tempH, RequireLoginMiddleware)
	h.Server.HEAD(env.TempDir+"/*", tempH, RequireLoginMiddleware)

	// RiverUI
	h.Server.Any("/riverui/", echo.WrapHandler(h.RiverUIServer), SessionRole(utils.EditorRole))
	h.Server.Any("/riverui/*", echo.WrapHandler(h.RiverUIServer), SessionRole(utils.EditorRole))

	// Swagger
	h.Server.GET("/swagger/*", echoSwagger.WrapHandler)

	// Proxy frontend server
	frontendURL, _ := url.Parse("http://127.0.0.1:3000")
	h.Server.Any("/*", echo.WrapHandler(http.StripPrefix("/", httputil.NewSingleHostReverseProxy(frontendURL))))

	// create v1 group and setup v1 routes
	v1 := h.Server.Group("/api/v1")
	groupV1Routes(v1, h)
}

func groupV1Routes(e *echo.Group, h *Handler) {

	// Auth
	authGroup := e.Group("/auth")
	authGroup.POST("/register", h.Register)
	authGroup.POST("/login", h.Login)
	authGroup.POST("/logout", h.Logout, AuthGuardMiddleware)
	authGroup.GET("/me", h.Me, SessionOnly)
	authGroup.POST("/change-password", h.ChangePassword, SessionOnly)
	authGroup.GET("/oauth/login", h.OAuthLogin)
	authGroup.GET("/oauth/callback", h.OAuthCallback)

	// Channel
	//
	// Write/admin endpoints accept either a session cookie or an API
	// key. GETs stay public unless REQUIRE_LOGIN is enabled.
	channelGroup := e.Group("/channel")
	channelGroup.POST("", h.CreateChannel, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeChannelWrite))
	channelGroup.GET("", h.GetChannels, PublicUnlessRequireLogin(utils.ApiKeyScopeChannelRead))
	channelGroup.GET("/:id", h.GetChannel, PublicUnlessRequireLogin(utils.ApiKeyScopeChannelRead))
	channelGroup.GET("/name/:name", h.GetChannelByName, PublicUnlessRequireLogin(utils.ApiKeyScopeChannelRead))
	channelGroup.PUT("/:id", h.UpdateChannel, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeChannelWrite))
	channelGroup.DELETE("/:id", h.DeleteChannel, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeChannelAdmin))
	channelGroup.POST("/:id/update-image", h.UpdateChannelImage, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeChannelWrite))

	// VOD
	//
	// Write/admin endpoints (POST/PUT/DELETE for the VOD itself) accept
	// either a session cookie or an API key — see issue #1070, where
	// external scripts need to delete VODs after archiving them. Read
	// endpoints stay unauthenticated unless REQUIRE_LOGIN is enabled.
	vodGroup := e.Group("/vod")
	vodGroup.POST("", h.CreateVod, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeVodWrite))
	vodGroup.GET("", h.GetVods, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id", h.GetVod, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/external_id/:external_id", h.GetVod, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/search", h.SearchVods, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.PUT("/:id", h.UpdateVod, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeVodWrite))
	vodGroup.DELETE("/:id", h.DeleteVod, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeVodAdmin))
	vodGroup.GET("/:id/playlist", h.GetVodPlaylists, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id/clips", h.GetVodClips, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/paginate", h.GetVodsPagination, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/pagination", h.GetVodsPagination, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id/chat", h.GetVodChatComments, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id/chat/seek", h.GetNumberOfVodChatCommentsFromTime, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id/chat/userid", h.GetUserIdFromChat, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id/chat/emotes", h.GetChatEmotes, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id/chat/badges", h.GetChatBadges, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.GET("/:id/chat/histogram", h.GetVodChatHistogram, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.POST("/:id/lock", h.LockVod, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeVodWrite))
	vodGroup.POST("/:id/generate-static-thumbnail", h.GenerateStaticThumbnail, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeVodWrite))
	vodGroup.POST("/:id/generate-sprite-thumbnails", h.GenerateSpriteThumbnails, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeVodWrite))
	vodGroup.GET("/:id/thumbnails/vtt", h.GetVodSpriteThumbnails, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	vodGroup.POST("/:id/ffprobe", h.GetFFprobe, SessionOrAPIKey(utils.ArchiverRole, utils.ApiKeyScopeVodWrite))

	// Queue
	//
	// Issue #1070 calls out "running actions" — i.e. starting tasks from
	// scripts. The queue is the surface where archive/transcode jobs
	// live, so we accept API keys on every queue endpoint. Read endpoints
	// require read scope (matches ArchiverRole), writes require write
	// scope (matches EditorRole), and POST/DELETE that previously
	// required AdminRole now require admin scope.
	queueGroup := e.Group("/queue")
	queueGroup.POST("", h.CreateQueueItem, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeQueueAdmin))
	queueGroup.GET("", h.GetQueueItems, SessionOrAPIKey(utils.ArchiverRole, utils.ApiKeyScopeQueueRead))
	queueGroup.GET("/:id", h.GetQueueItem, SessionOrAPIKey(utils.ArchiverRole, utils.ApiKeyScopeQueueRead))
	queueGroup.PUT("/:id", h.UpdateQueueItem, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeQueueWrite))
	queueGroup.DELETE("/:id", h.DeleteQueueItem, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeQueueAdmin))
	queueGroup.GET("/:id/tail", h.ReadQueueLogFile, SessionOrAPIKey(utils.ArchiverRole, utils.ApiKeyScopeQueueRead))
	queueGroup.POST("/:id/stop", h.StopQueueItem, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeQueueAdmin))
	queueGroup.POST("/task/start", h.StartQueueTask, SessionOrAPIKey(utils.ArchiverRole, utils.ApiKeyScopeQueueWrite))

	// Twitch
	twitchGroup := e.Group("/twitch")
	twitchGroup.GET("/channel", h.GetTwitchChannel, RequireLoginMiddleware)
	twitchGroup.GET("/video", h.GetTwitchVideo, RequireLoginMiddleware)
	// twitchGroup.GET("/gql/video", h.GQLGetTwitchVideo)
	// twitchGroup.GET("/categories", h.GetTwitchCategories)

	// Archive
	//
	// All POSTs accept either a session cookie or an API key. Archive
	// channel/video are write-tier (Archiver role); the chat converter
	// is admin-tier (Admin role).
	archiveGroup := e.Group("/archive")
	archiveGroup.POST("/channel", h.ArchiveChannel, SessionOrAPIKey(utils.ArchiverRole, utils.ApiKeyScopeArchiveWrite))
	archiveGroup.POST("/video", h.ArchiveVideo, SessionOrAPIKey(utils.ArchiverRole, utils.ApiKeyScopeArchiveWrite))
	archiveGroup.POST("/convert-twitch-live-chat", h.ConvertTwitchChat, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeArchiveAdmin))

	// Admin: system stats and info.
	//
	// Read-only system endpoints accept either a session cookie or an
	// API key with system:read. The /admin/api-keys management endpoints
	// further down stay session-only — minting keys requires the admin
	// web UI to prevent key-mints-key escalation.
	adminGroup := e.Group("/admin")
	adminGroup.GET("/video-statistics", h.GetVideoStatistics, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeSystemRead))
	adminGroup.GET("/system-overview", h.GetSystemOverview, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeSystemRead))
	adminGroup.GET("/storage-distribution", h.GetStorageDistribution, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeSystemRead))
	adminGroup.GET("/info", h.GetInfo, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeSystemRead))

	// Admin: API keys. Session-only — admins must use the web UI to mint
	// or revoke keys. This avoids the chicken-and-egg of needing a key
	// to manage keys, and means a stolen key cannot mint or escalate
	// other keys.
	adminGroup.GET("/api-keys", h.ListApiKeys, SessionRole(utils.AdminRole))
	adminGroup.POST("/api-keys", h.CreateApiKey, SessionRole(utils.AdminRole))
	adminGroup.PUT("/api-keys/:id", h.UpdateApiKey, SessionRole(utils.AdminRole))
	adminGroup.DELETE("/api-keys/:id", h.DeleteApiKey, SessionRole(utils.AdminRole))

	// User
	//
	// All endpoints require AdminRole for sessions. API keys are gated
	// at user:read for GETs, user:write for PUT, user:admin for DELETE
	// — same tier-by-method pattern used elsewhere.
	userGroup := e.Group("/user")
	userGroup.GET("", h.GetUsers, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeUserRead))
	userGroup.GET("/:id", h.GetUser, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeUserRead))
	userGroup.PUT("/:id", h.UpdateUser, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeUserWrite))
	userGroup.DELETE("/:id", h.DeleteUser, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeUserAdmin))

	// Config
	configGroup := e.Group("/config")
	configGroup.GET("", h.GetConfig, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeConfigRead))
	configGroup.PUT("", h.UpdateConfig, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeConfigWrite))

	// Live
	//
	// Editor-role surface: GETs at live:read, mutations at live:write
	// (DELETE inclusive, mirroring the role tier).
	liveGroup := e.Group("/live")
	liveGroup.GET("", h.GetLiveWatchedChannels, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeLiveRead))
	liveGroup.POST("", h.AddLiveWatchedChannel, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeLiveWrite))
	liveGroup.PUT("/:id", h.UpdateLiveWatchedChannel, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeLiveWrite))
	liveGroup.DELETE("/:id", h.DeleteLiveWatchedChannel, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeLiveWrite))
	liveGroup.GET("/check", h.Check, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeLiveRead))
	// liveGroup.GET("/vod", h.CheckVodWatchedChannels, SessionRole(utils.EditorRole))
	// liveGroup.POST("/archive", h.ArchiveLiveChannel, SessionRole(utils.ArchiverRole))

	// Playback
	playbackGroup := e.Group("/playback")
	playbackGroup.GET("", h.GetAllProgress, SessionOnly)
	playbackGroup.GET("/:id", h.GetProgress, SessionOnly)
	playbackGroup.POST("/progress", h.UpdateProgress, SessionOnly)
	playbackGroup.POST("/status", h.UpdateStatus, SessionOnly)
	playbackGroup.DELETE("/:id", h.DeleteProgress, SessionOnly)
	playbackGroup.GET("/last", h.GetLastPlaybacks, SessionOnly)
	playbackGroup.POST("/start", h.StartPlayback, RequireLoginMiddleware)

	// Playlist
	//
	// All write endpoints accept either a session cookie or an API key.
	// Issue #1070's second use case: scripts that auto-create or
	// reorder playlists. GET endpoints stay unauthenticated unless
	// REQUIRE_LOGIN is enabled.
	playlistGroup := e.Group("/playlist")
	playlistGroup.GET("/:id", h.GetPlaylist, PublicUnlessRequireLogin(utils.ApiKeyScopePlaylistRead))
	playlistGroup.GET("", h.GetPlaylists, PublicUnlessRequireLogin(utils.ApiKeyScopePlaylistRead))
	playlistGroup.POST("", h.CreatePlaylist, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))
	playlistGroup.POST("/:id", h.AddVodToPlaylist, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))
	playlistGroup.DELETE("/:id/vod", h.DeleteVodFromPlaylist, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))
	playlistGroup.DELETE("/:id", h.DeletePlaylist, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))
	playlistGroup.PUT("/:id", h.UpdatePlaylist, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))
	playlistGroup.PUT("/:id/multistream/delay", h.SetVodDelayOnPlaylistMultistream, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))
	playlistGroup.PUT("/:id/rules", h.SetPlaylistRules, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))
	playlistGroup.GET("/:id/rules", h.GetPlaylistRules, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistRead))
	playlistGroup.POST("/:id/rules/test", h.TestPlaylistRules, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopePlaylistWrite))

	// Task
	taskGroup := e.Group("/task")
	taskGroup.POST("/start", h.StartTask, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeTaskAdmin))

	// Notification
	//
	// All endpoints require AdminRole for sessions. API keys are gated
	// at notification:read for GETs, notification:write for create/
	// update/test, notification:admin for DELETE.
	notificationGroup := e.Group("/notification")
	notificationGroup.GET("", h.GetNotifications, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeNotificationRead))
	notificationGroup.GET("/:id", h.GetNotification, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeNotificationRead))
	notificationGroup.POST("", h.CreateNotification, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeNotificationWrite))
	notificationGroup.PUT("/:id", h.UpdateNotification, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeNotificationWrite))
	notificationGroup.DELETE("/:id", h.DeleteNotification, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeNotificationAdmin))
	notificationGroup.POST("/:id/test", h.TestNotification, SessionOrAPIKey(utils.AdminRole, utils.ApiKeyScopeNotificationWrite))

	// Chapter
	chapterGroup := e.Group("/chapter")
	chapterGroup.GET("/video/:videoId", h.GetVideoChapters, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))
	chapterGroup.GET("/video/:videoId/webvtt", h.GetWebVTTChapters, PublicUnlessRequireLogin(utils.ApiKeyScopeVodRead))

	// Category
	categoryGroup := e.Group("/category")
	categoryGroup.GET("", h.GetCategories, RequireLoginMiddleware)

	// Blocked
	//
	// Public reads stay public unless REQUIRE_LOGIN is enabled. Write
	// endpoints (block/unblock) accept API keys at blocked_video:write
	// — Editor role for sessions.
	blockedGroup := e.Group("/blocked-video")
	blockedGroup.GET("", h.GetBlockedVideos, PublicUnlessRequireLogin(utils.ApiKeyScopeBlockedVideoRead))
	blockedGroup.POST("/:id", h.CreateBlockedVideo, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeBlockedVideoWrite))
	blockedGroup.DELETE("/:id", h.DeleteBlockedVideo, SessionOrAPIKey(utils.EditorRole, utils.ApiKeyScopeBlockedVideoWrite))
	blockedGroup.GET("/:id", h.IsVideoBlocked, PublicUnlessRequireLogin(utils.ApiKeyScopeBlockedVideoRead))
}

func (h *Handler) Serve(ctx context.Context) error {
	appPort := os.Getenv("APP_PORT")
	if appPort == "" {
		appPort = "4000"
	}
	// Run the server in a goroutine
	serverErrCh := make(chan error, 1)
	go func() {
		if err := h.Server.Start(fmt.Sprintf(":%s", appPort)); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
		close(serverErrCh)
	}()

	// Listen for the context to be canceled or an error to occur in the server
	select {
	case <-ctx.Done():
		log.Info().Msg("Context canceled, shutting down the server")
	case err := <-serverErrCh:
		if err != nil {
			log.Fatal().Err(err).Msg("failed to start server")
		}
	}

	// Shutdown the server with a timeout of 10 seconds
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Server.Shutdown(shutdownCtx); err != nil {
		log.Fatal().Err(err).Msg("failed to shutdown server")
	}

	return nil
}
