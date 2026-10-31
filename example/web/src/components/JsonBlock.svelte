<script lang="ts">
  let { value }: { value: unknown } = $props();

  // A body that wasn't JSON comes back as a string, which is shown as it is
  let text = $derived(typeof value === "string" ? value : JSON.stringify(value, null, 2));
</script>

<pre class="json">{text || "(empty)"}</pre>
