<script lang="ts">
  // Advisory password-strength meter (P1.4 §2; 09 §1 wireframe "strength: ▓▓▓▓▓░
  // good"). Renders 4 segments filled to the estimated score plus the word
  // label. Purely client-side hint — never gates submit (the argon2id policy is
  // server-side).
  import { estimate } from '../lib/pwstrength'

  interface Props {
    password: string
  }
  let { password }: Props = $props()

  const result = $derived(estimate(password))
  const tone = $derived(
    result.score <= 1 ? 'weak' : result.score === 2 ? 'fair' : result.score === 3 ? 'good' : 'strong',
  )
</script>

<div class="strength" aria-hidden={password ? undefined : 'true'}>
  <div class="bars">
    {#each [0, 1, 2, 3] as i (i)}
      <span class="seg {i < result.score ? tone : ''}" class:filled={i < result.score}></span>
    {/each}
  </div>
  {#if password}
    <span class="label {tone}">{result.label}</span>
  {/if}
</div>

<style>
  .strength {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .bars {
    display: flex;
    gap: var(--space-1);
    flex: 1;
  }
  .seg {
    height: 4px;
    flex: 1;
    border-radius: 999px;
    background: var(--surface-3);
    transition: background 0.15s ease;
  }
  .seg.weak {
    background: var(--danger-bright);
  }
  .seg.fair {
    background: var(--warn-bright);
  }
  .seg.good {
    background: var(--accent-bright);
  }
  .seg.strong {
    background: var(--success-bright);
  }
  .label {
    font-size: var(--text-xs);
    min-width: 3.5rem;
    text-align: right;
  }
  .label.weak {
    color: var(--danger-bright);
  }
  .label.fair {
    color: var(--warn-bright);
  }
  .label.good {
    color: var(--accent-bright);
  }
  .label.strong {
    color: var(--success-bright);
  }
</style>
