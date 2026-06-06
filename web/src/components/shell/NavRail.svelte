<script lang="ts">
  import { currentRoute, navigate } from '../../lib/router'

  // The five flat operational screens (09 §0). Node detail (/nodes/:id) is
  // reached contextually and is intentionally NOT a nav item.
  interface NavItem {
    name: string
    path: string
    label: string
    icon: string
  }
  const items: NavItem[] = [
    { name: 'dashboard', path: '/', label: 'Dashboard', icon: '◧' },
    { name: 'cluster', path: '/cluster', label: 'Cluster', icon: '⚇' },
    { name: 'groups', path: '/groups', label: 'Groups', icon: '♬' },
    { name: 'media', path: '/media', label: 'Media', icon: '▤' },
    { name: 'settings', path: '/settings', label: 'Settings', icon: '⚙' },
  ]

  function go(e: MouseEvent, path: string) {
    e.preventDefault()
    navigate(path)
  }
</script>

<nav class="rail" aria-label="Primary">
  <div class="brand">Ensemble</div>
  <ul>
    {#each items as item (item.name)}
      <li>
        <a
          href={item.path}
          class:active={$currentRoute.name === item.name}
          aria-current={$currentRoute.name === item.name ? 'page' : undefined}
          onclick={(e) => go(e, item.path)}
        >
          <span class="icon" aria-hidden="true">{item.icon}</span>
          <span class="label">{item.label}</span>
        </a>
      </li>
    {/each}
  </ul>
</nav>

<style>
  .rail {
    width: var(--nav-width);
    flex-shrink: 0;
    background: var(--raised);
    border-right: 1px solid var(--border);
    display: flex;
    flex-direction: column;
    padding: var(--space-3) var(--space-2);
    z-index: var(--z-nav);
  }
  .brand {
    font-weight: 700;
    font-size: var(--text-lg);
    padding: var(--space-2) var(--space-3) var(--space-4);
    color: var(--accent-bright);
    letter-spacing: 0.02em;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  a {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    color: var(--text-dim);
    font-size: var(--text-sm);
    text-decoration: none;
  }
  a:hover {
    background: var(--surface-2);
    color: var(--text);
    text-decoration: none;
  }
  a.active {
    background: var(--surface-3);
    color: var(--text);
    font-weight: 600;
  }
  .icon {
    width: 1.2rem;
    text-align: center;
    color: var(--text-muted);
  }
  a.active .icon {
    color: var(--accent-bright);
  }
</style>
