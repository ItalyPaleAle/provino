<script lang="ts">
  import { onMount } from "svelte";
  import AuthorizationList from "./components/AuthorizationList.svelte";
  import DeliveryList from "./components/DeliveryList.svelte";
  import ImageCard from "./components/ImageCard.svelte";
  import Uploader from "./components/Uploader.svelte";
  import { getAuthorizations, getConfig, getDeliveries, getImages } from "./lib/api";
  import { auth, finishSignIn, loadSession, signIn, signOut } from "./lib/session.svelte";
  import type { AuthorizationRecord, Config, DeliveryRecord, ImageRecord } from "./lib/types";

  type Tab = "images" | "authorizer" | "webhooks";

  let config = $state<Config | null>(null);
  let tab = $state<Tab>("images");
  let images = $state<ImageRecord[]>([]);
  let authorizations = $state<AuthorizationRecord[]>([]);
  let deliveries = $state<DeliveryRecord[]>([]);

  onMount(async () => {
    try {
      config = await getConfig();
      if (location.pathname === "/callback") {
        await finishSignIn(config);
      }
      loadSession();
    } catch (err) {
      auth.error = `The page failed to start: ${(err as Error).message}`;
    }
  });

  // Load everything the app recorded for the signed-in user
  async function refresh(): Promise<void> {
    if (!auth.session) {
      return;
    }
    try {
      [images, authorizations, deliveries] = await Promise.all([getImages(), getAuthorizations(), getDeliveries()]);
    } catch {
      // A failed call already signed out, or the next refresh tries again
    }
  }

  // Refreshing on a timer also shows the outcome of jobs started from another tab or with the client API
  $effect(() => {
    if (!auth.session) {
      images = [];
      authorizations = [];
      deliveries = [];
      return;
    }
    void refresh();
    const timer = setInterval(refresh, 3000);
    return () => clearInterval(timer);
  });

  const tabs: { id: Tab; label: string; count: () => number }[] = [
    { id: "images", label: "Images", count: () => images.length },
    { id: "authorizer", label: "Authorizer", count: () => authorizations.length },
    { id: "webhooks", label: "Webhooks", count: () => deliveries.length },
  ];
</script>

<header class="topbar">
  <div class="brand">provino <span>example app</span></div>
  {#if auth.session}
    <div class="account">
      <span>Signed in as {auth.session.claims.name} ({auth.session.claims.sub})</span>
      <button class="link" onclick={() => signOut()}>Sign out</button>
    </div>
  {/if}
</header>

<main>
  {#if auth.error}
    <p class="banner error">{auth.error}</p>
  {/if}

  {#if !config}
    <p class="muted">Loading…</p>
  {:else if !auth.session}
    <section class="card intro">
      <h1>Try provino</h1>
      <p>
        Sign in with one of the test accounts, upload a few images, and watch provino ask this app to authorize them, generate their variants, and
        report back with a webhook.
      </p>
      <button class="primary" onclick={() => signIn(config!)}>Sign in</button>
    </section>
  {:else}
    <Uploader {config} onsettled={refresh} />

    <nav class="tabs" aria-label="Recorded data">
      {#each tabs as t (t.id)}
        <button class:active={tab === t.id} aria-current={tab === t.id} onclick={() => (tab = t.id)}>
          {t.label} <span class="count">{t.count()}</span>
        </button>
      {/each}
    </nav>

    <section class="card">
      {#if tab === "images"}
        {#if images.length === 0}
          <p class="muted">No images yet. An image appears here once provino's webhook reports its job.</p>
        {/if}
        <div class="images">
          {#each images as image (image.namespace + "/" + image.imageId)}
            <ImageCard {image} />
          {/each}
        </div>
      {:else if tab === "authorizer"}
        <AuthorizationList {authorizations} />
      {:else}
        <DeliveryList {deliveries} />
      {/if}
    </section>
  {/if}
</main>
