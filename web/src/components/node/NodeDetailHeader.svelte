<script lang="ts">
  // Node-detail header (09 §6): node name, online + cert status, and a back link
  // to wherever the operator came from (Dashboard/Cluster/Groups). The online-dot
  // CSS idiom is reused from the ../media device-detail pane; the cert/back affordances
  // are Ensemble's. Back uses history.back() when there is history, else falls
  // back to the Cluster screen.
  import { navigate } from '../../lib/router'
  import type { NodeDetailView } from '../../lib/node'

  interface Props {
    node: NodeDetailView
  }
  let { node }: Props = $props()

  const certText = $derived(
    node.certSignedByCa === true
      ? 'cert ✔ (signed by CA)'
      : node.certSignedByCa === false
        ? 'cert present (unsigned)'
        : '',
  )

  function back() {
    if (typeof history !== 'undefined' && history.length > 1) history.back()
    else navigate('/cluster')
  }
</script>

<header class="node-header">
  <div class="title">
    <h2>Node: {node.name || node.id}</h2>
    <div class="status">
      <span class="dot" class:online={node.online !== false}></span>
      <span class="online-text">{node.online === false ? 'offline' : 'online'}</span>
      {#if certText}
        <span class="sep">·</span>
        <span class="cert" class:signed={node.certSignedByCa}>{certText}</span>
      {/if}
    </div>
  </div>
  <button type="button" class="back" onclick={back}>⤺ back</button>
</header>

<style>
  .node-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-4);
    margin-bottom: var(--space-4);
  }
  h2 {
    margin: 0 0 var(--space-1);
    font-size: var(--text-lg);
    color: var(--text);
  }
  .status {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--text-muted);
  }
  .dot {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 50%;
    background: var(--text-muted);
  }
  .dot.online {
    background: var(--success-bright);
  }
  .online-text {
    color: var(--text-dim);
  }
  .sep {
    opacity: 0.5;
  }
  .cert.signed {
    color: var(--success-bright);
  }
  .back {
    flex-shrink: 0;
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--text-dim);
    font: inherit;
    font-size: var(--text-sm);
    padding: 0.35rem 0.7rem;
    cursor: pointer;
  }
  .back:hover {
    border-color: var(--accent);
    color: var(--accent-bright);
  }
</style>
