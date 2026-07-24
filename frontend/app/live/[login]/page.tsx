import type { Metadata } from "next";
import { notFound } from "next/navigation";
import TwitchLivePage from "./TwitchLivePage";

interface LivePageProps {
  params: Promise<{
    login: string;
  }>;
}

const TWITCH_LOGIN_PATTERN = /^[a-z0-9_]{1,25}$/i;

function normalizeTwitchLogin(login: string): string | null {
  const normalizedLogin = login.trim().toLowerCase();

  if (!TWITCH_LOGIN_PATTERN.test(normalizedLogin)) {
    return null;
  }

  return normalizedLogin;
}

export async function generateMetadata({
  params,
}: LivePageProps): Promise<Metadata> {
  const { login: rawLogin } = await params;
  const login = normalizeTwitchLogin(rawLogin);

  return {
    title: login ? `${login} — Twitch Live | Ganymede` : "Twitch Live | Ganymede",
  };
}

export default async function LivePage({ params }: LivePageProps) {
  const { login: rawLogin } = await params;
  const login = normalizeTwitchLogin(rawLogin);

  if (!login) {
    notFound();
  }

  return <TwitchLivePage login={login} />;
}
