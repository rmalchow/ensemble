<script lang="ts">
  import { clusterInfo, session, connected } from '../../lib/stores'
  import { logout } from '../../lib/api'
  import { navigate } from '../../lib/router'
  import Chip from '../ui/Chip.svelte'

  let menuOpen = $state(false)

  async function doLogout() {
    menuOpen = false
    try {
      await logout()
    } catch {
      // logout is best-effort; the session is cleared regardless.
    }
    session.set(null)
    navigate('/login', true)
  }
</script>

<header class="header">
  <div class="identity">
    <span class="cluster">{$clusterInfo?.name ?? 'Ensemble'}</span>
    {#if $session?.nodeId}
      <span class="node mono">{$session.nodeId}</span>
    {/if}
  </div>

  <div class="right">
    <Chip tone={$connected ? 'success' : 'danger'}>
      {$connected ? 'live' : 'offline'}
    </Chip>

    <div class="user">
      <button
        type="button"
        class="user-btn"
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        onclick={() => (menuOpen = !menuOpen)}
      >
        <span class="avatar" aria-hidden="true">●</span>
      </button>
      {#if menuOpen}
        <div class="menu" role="menu">
          <button type="button" role="menuitem" onclick={doLogout}>Log out</button>
        </div>
      {/if}
    </div>
  </div>
</header>

<style>
  .header {
    height: var(--header-height);
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: 0 var(--space-5);
    background: var(--surface);
    border-bottom: 1px solid var(--border);
    z-index: var(--z-header);
  }
  .identity {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
    min-width: 0;
  }
  .cluster {
    font-weight: 600;
  }
  .node {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .right {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .user {
    position: relative;
  }
  .user-btn {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: 999px;
    width: 2rem;
    height: 2rem;
    display: grid;
    place-items: center;
    color: var(--accent-bright);
  }
  .user-btn:hover {
    background: var(--surface-3);
  }
  .menu {
    position: absolute;
    top: calc(100% + var(--space-2));
    right: 0;
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    box-shadow: var(--shadow-modal);
    min-width: 8rem;
    padding: var(--space-1);
    z-index: var(--z-modal);
  }
  .menu button {
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    color: var(--text);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
  }
  .menu button:hover {
    background: var(--surface-3);
  }
</style>
