package exec

import (
	"context"

	"github.com/rs/zerolog/log"
	"github.com/zibbp/ganymede/internal/config"
	"github.com/zibbp/ganymede/internal/hls"
	"github.com/zibbp/ganymede/internal/livestream"
	"github.com/zibbp/ganymede/internal/utils"
)

func tryProxyServer(proxyURL string, testURL string, header string, proxyType utils.ProxyType) (*hls.Multivariant, bool) {
	switch proxyType {
	case utils.ProxyTypeTwitchHLS:
		return tryTwitchHLSProxy(proxyURL, testURL, header)
	case utils.ProxyTypeHTTP:
		return tryHTTPProxy(proxyURL, testURL, header)
	default:
		log.Error().Msgf("Unknown proxy type: %s", proxyType)
		return nil, false
	}
}

func tryTwitchHLSProxy(proxyURL string, testURL string, header string) (*hls.Multivariant, bool) {
	source, err := livestream.ResolveProxy(context.Background(), config.ProxyListItem{
		URL:       proxyURL,
		Header:    header,
		ProxyType: utils.ProxyTypeTwitchHLS,
	}, testURL)
	if err != nil {
		log.Error().Err(err).Msg("error testing Twitch HLS proxy server")
		return nil, false
	}

	return source.Playlist, true
}

func tryHTTPProxy(proxyURL string, testURL string, header string) (*hls.Multivariant, bool) {
	source, err := livestream.ResolveProxy(context.Background(), config.ProxyListItem{
		URL:       proxyURL,
		Header:    header,
		ProxyType: utils.ProxyTypeHTTP,
	}, testURL)
	if err != nil {
		log.Error().Err(err).Msg("error testing HTTP proxy server")
		return nil, false
	}

	return source.Playlist, true
}
