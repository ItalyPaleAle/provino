<script lang="ts">
  import { formatTime } from "../lib/format";
  import type { AuthorizationRecord } from "../lib/types";
  import HeaderTable from "./HeaderTable.svelte";
  import JsonBlock from "./JsonBlock.svelte";

  let { authorizations }: { authorizations: AuthorizationRecord[] } = $props();

  // What the summary of a request says about the decision
  function decision(a: AuthorizationRecord): { label: string; kind: string } {
    if (a.status === 200) {
      return { label: "allowed", kind: "optimized" };
    }
    if (a.status === 403) {
      return { label: "refused", kind: "failed" };
    }
    return { label: `error ${a.status}`, kind: "failed" };
  }

  // The profile the browser asked for, from the context provino forwarded
  function profileOf(a: AuthorizationRecord): string {
    const request = a.request as { context?: { profile?: string } } | null;
    return request?.context?.profile ?? "no profile";
  }
</script>

<p class="muted">
  Before it stores a browser upload, provino asks this app's authorizer what the upload may be. These are the requests it made for your uploads,
  newest first, with what the app answered.
</p>

{#if authorizations.length === 0}
  <p class="muted">No requests yet.</p>
{/if}

<div class="exchanges">
  {#each authorizations as a (a.id)}
    {@const d = decision(a)}
    <details class="exchange">
      <summary>
        <span class="time">{formatTime(a.receivedAt)}</span>
        <span class="badge {d.kind}">{d.label}</span>
        <span>{profileOf(a)}</span>
      </summary>
      <div class="exchange-body">
        <div>
          <h3>Request from provino</h3>
          <HeaderTable headers={a.requestHeaders} />
          <JsonBlock value={a.request} />
        </div>
        <div>
          <h3>Answer: {a.status}</h3>
          <JsonBlock value={a.response} />
        </div>
      </div>
    </details>
  {/each}
</div>
