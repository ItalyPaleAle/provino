<script lang="ts">
  import { formatBox, formatBytes, formatTime } from "../lib/format";
  import type { ImageRecord } from "../lib/types";

  let { image }: { image: ImageRecord } = $props();

  let variants = $derived(image.manifest?.variants ?? []);

  // Preview the smallest file that's at least 300 pixels wide, so the card stays light
  let preview = $derived(
    variants
      .filter((v) => !v.skipped)
      .toSorted((a, b) => a.width - b.width)
      .find((v) => v.width >= 300) ?? variants[0],
  );
</script>

<article class="image">
  <div class="preview">
    {#if preview}
      <img src={`/media/${encodeURI(preview.key)}`} alt="" loading="lazy" />
    {:else}
      <span>No variants</span>
    {/if}
  </div>

  <div class="details">
    <div class="meta">
      <span class="badge {image.status.replace('image.', '')}">{image.status}</span>
      <code>{image.namespace}/{image.imageId}</code>
    </div>
    {#if image.error}
      <p class="error-text">{image.error}</p>
    {/if}
    {#if image.manifest}
      <p class="muted small">
        Original: {image.manifest.sourceFormat}, {image.manifest.sourceWidth}×{image.manifest.sourceHeight}, {formatBytes(image.manifest.sourceSize)}
        {#if image.profile}· profile {image.profile}{/if}
        · job <code>{image.jobId}</code> · {formatTime(image.updatedAt)}
      </p>
    {/if}

    {#if variants.length > 0}
      <div class="table-wrap">
        <table>
          <thead>
            <tr><th>Box</th><th>Format</th><th>Size</th><th>Bytes</th><th></th></tr>
          </thead>
          <tbody>
            {#each variants as v (v.requestedResolution.width + "x" + v.requestedResolution.height + v.fit + v.format + v.quality)}
              <tr>
                <td>
                  {formatBox(v.requestedResolution)}
                  {v.fit || "contain"}
                  {#if v.skipped && v.skippedFor}
                    <span class="tag" title={`The source can't fill this box, so the entry repeats the file of the ${formatBox(v.skippedFor)} box`}>skipped</span>
                  {/if}
                </td>
                <td>{v.format}{v.quality ? ` q${v.quality}` : ""}</td>
                <td>{v.width}×{v.height}</td>
                <td>{formatBytes(v.size)}</td>
                <td><a href={`/media/${encodeURI(v.key)}`} target="_blank" rel="noopener" title={v.key}>open</a></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</article>
