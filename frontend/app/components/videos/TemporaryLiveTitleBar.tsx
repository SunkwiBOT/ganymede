"use client";

import {
  Avatar,
  Badge,
  Divider,
  Group,
  Text,
  Tooltip,
} from "@mantine/core";
import { IconCalendarEvent, IconClock } from "@tabler/icons-react";
import dayjs from "dayjs";
import { useTranslations } from "next-intl";
import LiveElapsedTimer from "./LiveElapsedTimer";
import classes from "./TitleBar.module.css";

interface TemporaryLiveTitleBarProps {
  displayName: string;
  login: string;
  profileImageURL?: string;
  startedAt?: string;
  title: string;
}

export default function TemporaryLiveTitleBar({
  displayName,
  login,
  profileImageURL,
  startedAt,
  title,
}: TemporaryLiveTitleBarProps) {
  const videoT = useTranslations("VideoComponents");
  const liveT = useTranslations("LivePage");
  const twitchURL = `https://www.twitch.tv/${encodeURIComponent(login)}`;

  return (
    <div className={classes.titleBarContainer}>
      <div className={classes.titleBar}>
        <a
          className={classes.channelAvatarLink}
          href={twitchURL}
          target="_blank"
          rel="noopener noreferrer"
          referrerPolicy="no-referrer"
          aria-label={`${displayName} on Twitch`}
        >
          <Avatar
            src={profileImageURL}
            radius="xl"
            alt={displayName}
            mr={10}
            imageProps={{ referrerPolicy: "no-referrer" }}
          />
        </a>

        <Divider size="sm" orientation="vertical" mr={10} />

        <div className={classes.titleBarTitle}>
          <Tooltip label={title} openDelay={250}>
            <Text size="xl" lineClamp={1} pt={3}>
              {title}
            </Text>
          </Tooltip>
        </div>

        <div className={classes.titleBarRight}>
          <div className={classes.titleBarBadge}>
            {startedAt && (
              <>
                <Group mr={15}>
                  <Tooltip
                    label={videoT("liveElapsedTooltip")}
                    openDelay={250}
                  >
                    <div className={classes.titleBarBadge}>
                      <Text className={classes.liveElapsedTime} mr={5}>
                        <LiveElapsedTimer streamedAt={startedAt} />
                      </Text>
                      <IconClock size={20} />
                    </div>
                  </Tooltip>
                </Group>

                <Group mr={15}>
                  <Tooltip
                    label={`${videoT("streamedOnTooltip")} ${new Date(
                      startedAt,
                    ).toLocaleString()}`}
                    openDelay={250}
                  >
                    <div className={classes.titleBarBadge}>
                      <Text mr={5}>
                        {dayjs(startedAt).format("YYYY/MM/DD")}
                      </Text>
                      <IconCalendarEvent size={20} />
                    </div>
                  </Tooltip>
                </Group>
              </>
            )}

            <Tooltip label={liveT("temporaryTooltip")} openDelay={250}>
              <Badge variant="default">{liveT("temporaryBadge")}</Badge>
            </Tooltip>
          </div>
        </div>
      </div>
    </div>
  );
}
