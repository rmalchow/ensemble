<script lang="ts">
  // Variant-B panel shown instead of channel/gain/HWDelay when the node is
  // sink-less (render === false, D17 / 06 §1.5). NEW content (no mpvsync
  // analogue). Static explanatory copy + a deep-link to the Capabilities panel
  // below where the operator can re-enable a probed sink or clear "force
  // control-only". The node remains master/origin/media-store/clock/UI — just not
  // a listener.
</script>

<div class="cmo" role="note">
  <h4>Control / media only — no audio output</h4>
  <p>
    This node advertises <code>render = false</code> (no usable + enabled audio
    sink). It can be a group <strong>master / origin</strong>, a
    <strong>media store</strong>, a <strong>clock</strong>, and the
    <strong>UI</strong> — but it is <strong>not a listener</strong>. Channel role,
    hardware delay, and gain do not apply and are hidden.
  </p>
  <p class="hint">
    To give it audio output, enable a sink in
    <a href="#capabilities">Capabilities &amp; audio backends</a> below (if a
    backend was probed) or clear <em>force control-only</em>.
  </p>
</div>

<style>
  .cmo {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--surface-2);
    padding: var(--space-4);
  }
  h4 {
    margin: 0 0 var(--space-2);
    font-size: var(--text-base);
    color: var(--text);
  }
  p {
    margin: 0 0 var(--space-2);
    font-size: var(--text-sm);
    color: var(--text-dim);
    line-height: 1.5;
  }
  p:last-child {
    margin-bottom: 0;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  code {
    font-family: var(--font-mono);
    color: var(--accent-bright);
  }
  a {
    color: var(--accent-bright);
  }
</style>
