package http

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/zibbp/ganymede/internal/livestream"
)

var twitchLoginPattern = regexp.MustCompile(`^[a-zA-Z0-9_]{1,25}$`)

type LivePlaybackResponse struct {
	Login           string     `json:"login"`
	DisplayName     string     `json:"display_name"`
	Title           string     `json:"title"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	ProfileImageURL string     `json:"profile_image_url,omitempty"`
	PlaybackURL     string     `json:"playback_url"`
}

// StartLivePlayback godoc
//
//	@Summary		Start temporary live playback
//	@Description	Resolve a Twitch live stream using Ganymede's configured OAuth token and livestream proxies. No VOD or queue item is created.
//	@Tags			Live
//	@Produce		json
//	@Param			login	path		string	true	"Twitch channel login"
//	@Success		200		{object}	LivePlaybackResponse
//	@Failure		400		{object}	utils.ErrorResponse
//	@Failure		502		{object}	utils.ErrorResponse
//	@Router			/live/playback/{login} [get]
func (h *Handler) StartLivePlayback(c echo.Context) error {
	login := strings.ToLower(strings.TrimSpace(c.Param("login")))
	if !twitchLoginPattern.MatchString(login) {
		return ErrorResponse(c, http.StatusBadRequest, "invalid Twitch channel login")
	}

	sessionID, err := h.LivePlayback.Start(c.Request().Context(), login)
	if err != nil {
		log.Warn().
			Err(err).
			Str("channel_login", login).
			Msg("failed to start temporary live playback")
		return ErrorResponse(c, http.StatusBadGateway, "live stream is offline or unavailable")
	}

	response := LivePlaybackResponse{
		Login:       login,
		DisplayName: login,
		Title:       login,
		PlaybackURL: fmt.Sprintf("/api/v1/live/playback/session/%s/master.m3u8", sessionID),
	}

	// Playback must remain available even if the optional Twitch metadata
	// requests fail. The stream source has already been resolved above.
	if h.Service.PlatformTwitch != nil {
		stream, streamErr := h.Service.PlatformTwitch.GetLiveStream(c.Request().Context(), login)
		if streamErr != nil {
			log.Debug().
				Err(streamErr).
				Str("channel_login", login).
				Msg("failed to load temporary live playback stream metadata")
		} else {
			response.Login = stream.UserLogin
			response.DisplayName = stream.UserName
			response.Title = stream.Title
			startedAt := stream.StartedAt
			response.StartedAt = &startedAt
		}

		channel, channelErr := h.Service.PlatformTwitch.GetChannel(c.Request().Context(), &login, nil)
		if channelErr != nil {
			log.Debug().
				Err(channelErr).
				Str("channel_login", login).
				Msg("failed to load temporary live playback channel metadata")
		} else {
			response.Login = channel.Login
			response.DisplayName = channel.DisplayName
			response.ProfileImageURL = channel.ProfileImageURL
		}
	}

	return SuccessResponse(c, response, "live playback session created")
}

func (h *Handler) GetLivePlaybackMaster(c echo.Context) error {
	master, err := h.LivePlayback.Master(c.Param("session"))
	if err != nil {
		if errors.Is(err, livestream.ErrSessionNotFound) {
			return ErrorResponse(c, http.StatusNotFound, "live playback session expired")
		}
		return ErrorResponse(c, http.StatusInternalServerError, "failed to load live playlist")
	}

	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Blob(http.StatusOK, "application/vnd.apple.mpegurl", master)
}

func (h *Handler) ProxyLivePlaybackResource(c echo.Context) error {
	resource, err := h.LivePlayback.Fetch(
		c.Request().Context(),
		c.Param("session"),
		c.QueryParam("url"),
		c.QueryParam("signature"),
		c.QueryParams(),
		c.Request().Header.Get("Range"),
	)
	if err != nil {
		switch {
		case errors.Is(err, livestream.ErrSessionNotFound):
			return ErrorResponse(c, http.StatusNotFound, "live playback session expired")
		case errors.Is(err, livestream.ErrInvalidResource):
			return ErrorResponse(c, http.StatusNotFound, "live playback resource not found")
		default:
			log.Warn().Err(err).Msg("failed to proxy temporary live playback resource")
			return ErrorResponse(c, http.StatusBadGateway, "failed to load live stream resource")
		}
	}
	defer resource.Body.Close() //nolint:errcheck

	for name, values := range resource.Header {
		for _, value := range values {
			c.Response().Header().Add(name, value)
		}
	}

	contentType := resource.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return c.Stream(resource.StatusCode, contentType, resource.Body)
}
