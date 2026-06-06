<script lang="ts">
  import type { Snippet } from 'svelte'
  import NavRail from './NavRail.svelte'
  import Header from './Header.svelte'

  interface Props {
    children?: Snippet // the routed screen, mounted into <main>
  }
  let { children }: Props = $props()
</script>

<div class="shell">
  <NavRail />
  <div class="content">
    <Header />
    <main>{@render children?.()}</main>
  </div>
</div>

<style>
  .shell {
    display: flex;
    height: 100%;
    background: var(--raised);
  }
  .content {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  main {
    flex: 1;
    overflow: auto;
    padding: var(--space-6);
    background: var(--raised);
  }
</style>
