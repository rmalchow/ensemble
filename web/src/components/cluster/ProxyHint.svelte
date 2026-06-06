<script lang="ts">
  // The subtle "via <serving> → <target>" hint shown when an Adopt/Takeover/
  // Forget runs against a node other than the serving node (09 §0 proxying, §4).
  // Net-new to ensemble (mpvsync had no proxy notion). May be promoted to a
  // shared lib component if Dashboard/Groups reuse it (P2.7 §9).
  interface Props {
    serving: string
    target: string
  }
  let { serving, target }: Props = $props()
</script>

<span class="proxy-hint" title="This action proxies through the serving node to the target">
  via <span class="mono">{serving}</span> → <span class="mono">{target}</span>
</span>

<style>
  .proxy-hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
