<script lang="ts">
  import { startUpload, uploads } from "../lib/uploads.svelte";
  import type { Config } from "../lib/types";

  let { config, onsettled }: { config: Config; onsettled: () => void } = $props();

  // svelte-ignore state_referenced_locally
  let profile = $state(config.profiles[0]?.id ?? "");
  let dragging = $state(false);

  function uploadAll(files: FileList | null | undefined): void {
    for (const file of files ?? []) {
      startUpload(config, file, profile, onsettled);
    }
  }
</script>

<section class="card">
  <h2>Upload</h2>
  <p class="muted">
    Files go straight from your browser to provino at <code>{config.provinoUrl}</code>, with your token. provino asks this app which variants the
    profile you pick generates.
  </p>

  <fieldset class="profiles">
    <legend class="visually-hidden">Profile</legend>
    {#each config.profiles as p (p.id)}
      <label class="profile" class:selected={profile === p.id}>
        <input type="radio" name="profile" value={p.id} bind:group={profile} />
        <span><strong>{p.label}</strong><small>{p.description}</small></span>
      </label>
    {/each}
  </fieldset>

  <label
    class="drop"
    class:over={dragging}
    ondragover={(e) => {
      e.preventDefault();
      dragging = true;
    }}
    ondragleave={() => (dragging = false)}
    ondrop={(e) => {
      e.preventDefault();
      dragging = false;
      uploadAll(e.dataTransfer?.files);
    }}
  >
    <input
      type="file"
      accept="image/*"
      multiple
      onchange={(e) => {
        uploadAll(e.currentTarget.files);
        e.currentTarget.value = "";
      }}
    />
    <strong>Choose images</strong> or drop them here
  </label>

  {#if uploads.length > 0}
    <ul class="uploads">
      {#each uploads as upload (upload.id)}
        <li class={upload.state}>
          <span class="name">{upload.file}</span>
          <progress max="100" value={upload.progress}></progress>
          <span class="status">
            {upload.message}
            {#if upload.accepted}
              · <code>{upload.accepted.namespace}/{upload.accepted.imageId}</code>
            {/if}
          </span>
        </li>
      {/each}
    </ul>
  {/if}
</section>
