<script lang="ts">
  let { headers }: { headers: Record<string, string> } = $props();

  let sorted = $derived(Object.entries(headers).toSorted(([a], [b]) => a.localeCompare(b)));
</script>

<table class="headers">
  <tbody>
    {#each sorted as [name, value] (name)}
      <tr><th>{name}</th><td><code>{value}</code></td></tr>
    {/each}
  </tbody>
</table>
