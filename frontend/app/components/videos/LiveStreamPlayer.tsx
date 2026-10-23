"use client";

import { LiveVideoPlayer } from "@videojs/react/live-video";
import { HlsJsVideo } from "@videojs/react/media/hlsjs-video";
import { I18nProvider } from "@videojs/react/i18n";
import { LiveVideoSkin } from "@/components/videojs/live-video/skin";
import type { CSSProperties } from "react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useLocale } from "next-intl";
import useSettingsStore from "@/app/store/useSettingsStore";
import VideoPlayerHideChatIcon from "./PlayerHideChatIcon";
import VideoPlayerTheaterModeIcon from "./PlayerTheaterModeIcon";
import classes from "./LiveStreamPlayer.module.css";

interface LiveStreamPlayerProps {
  login: string;
  src: string;
}

const skinStyle = {
  "--media-accent-color": "var(--mantine-color-violet-6)",
  "--media-object-fit": "contain",
  "--media-border-radius": "0",
  "--media-video-border-radius": "0",
} as CSSProperties;

// Resuming a paused temporary stream jumps back to the live edge rather than
// replaying the buffered window.
function seekToLiveEdge(videoEl: HTMLVideoElement): void {
  const { seekable } = videoEl;
  if (seekable.length === 0) return;
  const end = seekable.end(seekable.length - 1);
  if (Number.isFinite(end) && end > 0) {
    videoEl.currentTime = end;
  }
}

const LiveStreamCustomControls = () => (
  <>
    <VideoPlayerTheaterModeIcon />
    <VideoPlayerHideChatIcon />
  </>
);

export default function LiveStreamPlayer({
  login,
  src,
}: LiveStreamPlayerProps) {
  const player = useRef<HTMLVideoElement>(null);
  const returnToLiveOnPlay = useRef(false);
  const [mounted, setMounted] = useState(false);
  const locale = useLocale();
  const autoplayVideo = useSettingsStore((state) => state.autoplayVideo);
  const videoTheaterMode = useSettingsStore(
    (state) => state.videoTheaterMode,
  );

  useEffect(() => {
    setMounted(true);
  }, []);

  const applyStoredVolume = useCallback(() => {
    const el = player.current;
    if (!el) return;
    try {
      const storedVolume = window.localStorage.getItem("ganymede-volume");
      if (storedVolume !== null) {
        const parsedVolume = Number.parseFloat(storedVolume);
        if (Number.isFinite(parsedVolume)) {
          el.volume = Math.min(1, Math.max(0, parsedVolume));
        }
      }
    } catch {
      // localStorage may be unavailable; keep element default.
    }
  }, []);

  const handleVolumeChange = useCallback(() => {
    const el = player.current;
    if (!el) return;
    try {
      window.localStorage.setItem("ganymede-volume", el.volume.toString());
    } catch {
      // Ignore storage errors.
    }
  }, []);

  const playerClassName = videoTheaterMode
    ? classes.mediaPlayerTheaterMode
    : classes.mediaPlayer;

  // Avoid SSR mismatch: Video.js media engines need the browser.
  if (!mounted) {
    return <div className={playerClassName} />;
  }

  return (
    <LiveVideoPlayer title={login}>
      <I18nProvider locale={locale}>
        <LiveVideoSkin
          className={playerClassName}
          style={skinStyle}
          customControls={<LiveStreamCustomControls />}
        >
          <HlsJsVideo
            ref={player}
            src={src}
            crossOrigin="use-credentials"
            playsInline
            autoPlay={autoplayVideo}
            preload="auto"
            onLoadedMetadata={applyStoredVolume}
            onVolumeChange={handleVolumeChange}
            onPause={() => {
              returnToLiveOnPlay.current = true;
            }}
            onPlay={() => {
              if (returnToLiveOnPlay.current && player.current) {
                seekToLiveEdge(player.current);
                returnToLiveOnPlay.current = false;
              }
            }}
          />
        </LiveVideoSkin>
      </I18nProvider>
    </LiveVideoPlayer>
  );
}
