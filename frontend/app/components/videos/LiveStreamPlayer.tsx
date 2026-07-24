"use client";

import "@vidstack/react/player/styles/default/theme.css";
import "@vidstack/react/player/styles/default/layouts/video.css";

import {
  MediaPlayer,
  MediaPlayerInstance,
  MediaProvider,
} from "@vidstack/react";
import {
  defaultLayoutIcons,
  DefaultVideoLayout,
} from "@vidstack/react/player/layouts/default";
import { useEffect, useRef, useState } from "react";
import useSettingsStore from "@/app/store/useSettingsStore";
import VideoPlayerHideChatIcon from "./PlayerHideChatIcon";
import VideoPlayerTheaterModeIcon from "./PlayerTheaterModeIcon";
import classes from "./LiveStreamPlayer.module.css";

interface LiveStreamPlayerProps {
  login: string;
  src: string;
}

function seekToLiveEdge(player: MediaPlayerInstance): void {
  try {
    player.seekToLiveEdge();
    return;
  } catch {
    // The provider may still be initializing. Use the known seekable edge
    // when available; otherwise HLS will begin at its normal live position.
  }

  const seekableEnd = player.state.seekableEnd;
  if (Number.isFinite(seekableEnd) && seekableEnd > 0) {
    player.currentTime = seekableEnd;
  }
}

export default function LiveStreamPlayer({
  login,
  src,
}: LiveStreamPlayerProps) {
  const player = useRef<MediaPlayerInstance>(null);
  const returnToLiveOnPlay = useRef(false);
  const [volume, setVolume] = useState(1);
  const autoplayVideo = useSettingsStore((state) => state.autoplayVideo);
  const videoTheaterMode = useSettingsStore(
    (state) => state.videoTheaterMode,
  );

  useEffect(() => {
    const storedVolume = window.localStorage.getItem("ganymede-volume");
    if (storedVolume !== null) {
      const parsedVolume = Number.parseFloat(storedVolume);
      if (Number.isFinite(parsedVolume)) {
        setVolume(parsedVolume);
      }
    }

    const unsubscribe = player.current?.subscribe(({ volume: nextVolume }) => {
      window.localStorage.setItem("ganymede-volume", nextVolume.toString());
    });

    return unsubscribe;
  }, []);

  return (
    <MediaPlayer
      ref={player}
      className={
        videoTheaterMode
          ? classes.mediaPlayerTheaterMode
          : classes.mediaPlayer
      }
      title={login}
      src={{
        src,
        type: "application/x-mpegurl",
      }}
      aspect-ratio={16 / 9}
      crossOrigin="use-credentials"
      playsInline
      load="eager"
      volume={volume}
      autoPlay={autoplayVideo}
      streamType="live"
      onPause={() => {
        returnToLiveOnPlay.current = true;
      }}
      onPlay={() => {
        if (returnToLiveOnPlay.current && player.current) {
          seekToLiveEdge(player.current);
          returnToLiveOnPlay.current = false;
        }
      }}
    >
      <MediaProvider />
      <DefaultVideoLayout
        icons={defaultLayoutIcons}
        noScrubGesture={false}
        slots={{
          beforeFullscreenButton: <VideoPlayerTheaterModeIcon />,
          afterFullscreenButton: <VideoPlayerHideChatIcon />,
        }}
      />
    </MediaPlayer>
  );
}
