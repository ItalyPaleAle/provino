// The signed-in user's token, from the app's OAuth2 server, with the authorization code grant
import type { Config } from "./types";

export interface Session {
  token: string;
  expiresAt: number;
  claims: { sub: string; name: string };
}

// Reactive state shared by every component
export const auth = $state<{ session: Session | null; error: string }>({ session: null, error: "" });

const tokenKey = "token";
const stateKey = "state";

function redirectURI(): string {
  return `${location.origin}/callback`;
}

// Read this tab's token, as long as it's valid for another minute
export function loadSession(): void {
  const stored = JSON.parse(sessionStorage.getItem(tokenKey) ?? "null") as { token: string; expiresAt: number } | null;
  if (!stored || stored.expiresAt < Date.now() + 60_000) {
    sessionStorage.removeItem(tokenKey);
    auth.session = null;
    return;
  }

  // The claims are only read to show who is signed in, so the token isn't verified here: provino and the app's API verify it
  const payload = stored.token.split(".")[1].replace(/-/g, "+").replace(/_/g, "/");
  auth.session = { ...stored, claims: JSON.parse(atob(payload)) };
}

// Send the browser to the sign-in page, keeping the state for when it comes back, so a response to another sign-in is refused
export function signIn(config: Config): void {
  const state = crypto.randomUUID();
  sessionStorage.setItem(stateKey, state);

  const query = new URLSearchParams({
    response_type: "code",
    client_id: config.clientId,
    redirect_uri: redirectURI(),
    scope: "ingest",
    state,
  });
  location.assign(`/oauth/authorize?${query}`);
}

// Exchange the code the sign-in page sent back for a token
export async function finishSignIn(config: Config): Promise<void> {
  const params = new URLSearchParams(location.search);
  const state = sessionStorage.getItem(stateKey);
  sessionStorage.removeItem(stateKey);
  history.replaceState(null, "", "/");

  if (!state || params.get("state") !== state) {
    auth.error = "The sign-in response doesn't belong to a sign-in this tab started. Sign in again.";
    return;
  }

  const res = await fetch("/oauth/token", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      client_id: config.clientId,
      code: params.get("code") ?? "",
      redirect_uri: redirectURI(),
    }),
  });
  const body = await res.json();
  if (!res.ok) {
    auth.error = `The sign-in failed: ${body.error_description ?? body.error}`;
    return;
  }
  sessionStorage.setItem(tokenKey, JSON.stringify({ token: body.access_token, expiresAt: Date.now() + body.expires_in * 1000 }));
}

export function signOut(message = ""): void {
  sessionStorage.removeItem(tokenKey);
  auth.session = null;
  auth.error = message;
}
