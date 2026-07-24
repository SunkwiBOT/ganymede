"use client";

import {
  Alert,
  Box,
  Button,
  Center,
  Stack,
  Text,
  useComputedColorScheme,
  useMantineTheme,
} from "@mantine/core";
import { useFullscreenDocument, useMediaQuery } from "@mantine/hooks";
import { IconAlertCircle, IconRefresh } from "@tabler/icons-react";
import { env } from "next-runtime-env";
import { useTranslations } from "next-intl";
import { useEffect, useMemo, useState } from "react";
import LiveStreamPlayer from "@/app/components/videos/LiveStreamPlayer";
import TemporaryLiveTitleBar from "@/app/components/videos/TemporaryLiveTitleBar";
import GanymedeLoadingText from "@/app/components/utils/GanymedeLoadingText";
import { useLivePlayback } from "@/app/hooks/useLivePlayback";
import useSettingsStore from "@/app/store/useSettingsStore";
import classes from "./LivePage.module.css";

interface TwitchLivePageProps {
  login: string;
}

function TwitchLiveChat({ login }: { login: string }) {
  const [parentHost, setParentHost] = useState<string | null>(null);
  const computedColorScheme = useComputedColorScheme("dark", {
    getInitialValueInEffect: true,
  });

  useEffect(() => {
    setParentHost(window.location.hostname);
  }, []);

  const chatSrc = useMemo(() => {
    if (!parentHost) {
      return "";
    }

    const params = new URLSearchParams({ parent: parentHost });
    if (computedColorScheme === "dark") {
      params.append("darkpopout", "");
    }

    return `https://www.twitch.tv/embed/${encodeURIComponent(login)}/chat?${params.toString()}`;
  }, [computedColorScheme, login, parentHost]);

  if (!chatSrc) {
    return null;
  }

  return (
    <iframe
      className={classes.twitchChatEmbed}
      src={chatSrc}
      title={`Twitch chat ${login}`}
    />
  );
}

export default function TwitchLivePage({ login }: TwitchLivePageProps) {
  const t = useTranslations("LivePage");
  const theme = useMantineTheme();
  const isMobile = useMediaQuery(`(max-width: ${theme.breakpoints.sm})`);
  const { fullscreen } = useFullscreenDocument();
  const videoTheaterMode = useSettingsStore(
    (state) => state.videoTheaterMode,
  );
  const hideChat = useSettingsStore((state) => state.hideChat);
  const chatOnLeft = useSettingsStore((state) => state.chatOnLeft);
  const { data, isPending, isError, refetch, isFetching } =
    useLivePlayback(login);

  const playerSrc = useMemo(() => {
    if (!data?.playback_url) {
      return "";
    }

    if (/^https?:\/\//i.test(data.playback_url)) {
      return data.playback_url;
    }

    const apiBase = (env("NEXT_PUBLIC_API_URL") ?? "").replace(/\/$/, "");
    const playbackPath = data.playback_url.startsWith("/")
      ? data.playback_url
      : `/${data.playback_url}`;

    return `${apiBase}${playbackPath}`;
  }, [data?.playback_url]);

  useEffect(() => {
    if (data?.title) {
      document.title = data.title;
    }
  }, [data?.title]);

  if (isPending) {
    return <GanymedeLoadingText message={t("loadingPlayer")} />;
  }

  if (isError || !data || !playerSrc) {
    return (
      <Center className={classes.errorPage}>
        <Alert
          className={classes.error}
          color="red"
          icon={<IconAlertCircle size={20} />}
          title={t("unavailableTitle")}
        >
          <Stack gap="sm">
            <Text size="sm">{t("unavailableMessage")}</Text>
            <Button
              color="red"
              variant="light"
              loading={isFetching}
              leftSection={<IconRefresh size={16} />}
              onClick={() => refetch()}
            >
              {t("retry")}
            </Button>
          </Stack>
        </Alert>
      </Center>
    );
  }

  const showChat = !hideChat;

  return (
    <div>
      <Box
        className={
          isMobile
            ? classes.containerMobile
            : `${classes.container} ${
                chatOnLeft ? classes.containerChatOnLeft : ""
              }`
        }
      >
        <div
          className={
            isMobile
              ? undefined
              : showChat
                ? classes.leftColumn
                : classes.leftColumnNoChat
          }
        >
          <div
            className={
              isMobile
                ? undefined
                : videoTheaterMode || fullscreen
                  ? classes.videoPlayerTheaterMode
                  : classes.videoPlayer
            }
          >
            <LiveStreamPlayer login={data.login} src={playerSrc} />
          </div>
        </div>

        {showChat && (
          <div
            className={
              isMobile ? classes.chatColumnMobile : classes.rightColumn
            }
          >
            <div
              className={
                isMobile
                  ? undefined
                  : videoTheaterMode || fullscreen
                    ? classes.chatColumnTheaterMode
                    : classes.chatColumn
              }
            >
              <TwitchLiveChat login={data.login} />
            </div>
          </div>
        )}
      </Box>

      {!videoTheaterMode && (
        <TemporaryLiveTitleBar
          displayName={data.display_name || data.login}
          login={data.login}
          profileImageURL={data.profile_image_url}
          startedAt={data.started_at}
          title={data.title || data.display_name || data.login}
        />
      )}
    </div>
  );
}
