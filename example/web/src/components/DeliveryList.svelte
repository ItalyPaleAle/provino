<script lang="ts">
  import { formatTime, shortID } from "../lib/format";
  import type { DeliveryRecord } from "../lib/types";
  import HeaderTable from "./HeaderTable.svelte";
  import JsonBlock from "./JsonBlock.svelte";

  let { deliveries }: { deliveries: DeliveryRecord[] } = $props();

  // What each outcome means, shown when hovering it
  const outcomes: Record<string, string> = {
    applied: "The event updated the image",
    repeat: "Another delivery with the same webhook ID was already acknowledged",
    stale: "The image already shows a newer event",
    ignored: "The app doesn't know this event type",
    failed: "The app failed to record the event, so provino retries it",
  };
</script>

<p class="muted">
  The webhooks provino delivered about your images, newest first. Every attempt of one event carries the same webhook ID, and an event older than the
  image's latest one is recorded but not applied.
</p>

{#if deliveries.length === 0}
  <p class="muted">No deliveries yet.</p>
{/if}

<div class="exchanges">
  {#each deliveries as d (d.id)}
    <details class="exchange">
      <summary>
        <span class="time">{formatTime(d.receivedAt)}</span>
        <span class="badge {d.type?.replace('image.', '')}">{d.type || "unknown"}</span>
        <code>{shortID(d.imageId)}</code>
        <span class="tag" title={outcomes[d.outcome] ?? ""}>{d.outcome}</span>
      </summary>
      <div class="exchange-body single">
        <div>
          <h3>Headers</h3>
          <HeaderTable headers={d.requestHeaders} />
          <h3>Body</h3>
          <JsonBlock value={d.body} />
          <p class="muted small">Answered {d.status} · event of {formatTime(d.timestamp)}</p>
        </div>
      </div>
    </details>
  {/each}
</div>
