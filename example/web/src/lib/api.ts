// Calls to the example app's API
import { auth, signOut } from "./session.svelte";
import type { AuthorizationRecord, Config, DeliveryRecord, ImageRecord } from "./types";

export async function getConfig(): Promise<Config> {
  const res = await fetch("/api/config");
  return res.json();
}

// Call the app's API with the signed-in user's token, signing out when the token is no longer accepted
async function apiGet<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Authorization: `Bearer ${auth.session?.token}` } });
  if (res.status === 401) {
    signOut("Your session expired. Sign in again.");
    throw new Error("unauthorized");
  }
  if (!res.ok) {
    throw new Error(`${path} answered ${res.status}`);
  }
  return res.json();
}

export const getImages = () => apiGet<ImageRecord[]>("/api/images");
export const getAuthorizations = () => apiGet<AuthorizationRecord[]>("/api/authorizations");
export const getDeliveries = () => apiGet<DeliveryRecord[]>("/api/deliveries");
