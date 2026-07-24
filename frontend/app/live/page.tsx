"use client";

import {
  Button,
  Card,
  Center,
  Container,
  Stack,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { FormEvent, useState } from "react";
import { usePageTitle } from "@/app/util/util";
import classes from "./LiveForm.module.css";

const TWITCH_LOGIN_PATTERN = /^[a-z0-9_]{1,25}$/i;
const TWITCH_CHANNEL_URL_PATTERN =
  /^(?:https?:\/\/)?(?:www\.)?twitch\.tv\/([a-z0-9_]{1,25})(?:[/?#]|$)/i;

function normalizeTwitchLogin(value: string): string | null {
  const trimmedValue = value.trim();
  const channelURLMatch = trimmedValue.match(TWITCH_CHANNEL_URL_PATTERN);
  const login = (channelURLMatch?.[1] ?? trimmedValue.replace(/^@/, ""))
    .toLowerCase();

  return TWITCH_LOGIN_PATTERN.test(login) ? login : null;
}

export default function LivePage() {
  const t = useTranslations("LivePage");
  const router = useRouter();
  const [channelInput, setChannelInput] = useState("");
  const [inputError, setInputError] = useState("");

  usePageTitle(t("pageTitle"));

  const watchLive = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();

    const login = normalizeTwitchLogin(channelInput);
    if (!login) {
      setInputError(t("invalidChannel"));
      return;
    }

    router.push(`/live/${encodeURIComponent(login)}`);
  };

  return (
    <Container size="md" mt={20}>
      <Center>
        <Card
          className={classes.card}
          component="form"
          shadow="sm"
          p="lg"
          radius="md"
          withBorder
          onSubmit={watchLive}
        >
          <Stack gap="md">
            <Stack gap={4} align="center">
              <Title order={1}>{t("pageTitle")}</Title>
              <Text c="dimmed" ta="center">
                {t("formMessage")}
              </Text>
            </Stack>

            <TextInput
              value={channelInput}
              onChange={(event) => {
                setChannelInput(event.currentTarget.value);
                setInputError("");
              }}
              error={inputError}
              placeholder={t("channelPlaceholder")}
              autoFocus
            />

            <Button type="submit" color="violet">
              {t("watchButton")}
            </Button>
          </Stack>
        </Card>
      </Center>
    </Container>
  );
}
