<script lang="ts">
  // Test-only host: mounts a row component (DiscoveredRow/MemberRow, which each
  // render a bare <tr>) inside a valid <table><tbody> so jsdom keeps the row in
  // the DOM instead of hoisting it. Not shipped — only imported by *.test.ts.
  import type { Component } from 'svelte'

  interface Props {
    // The row component under test (DiscoveredRow/MemberRow). Typed loosely on
    // purpose: this is a generic test host, and each test passes that row's own
    // strongly-typed props via childProps.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    component: Component<any>
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    childProps: Record<string, any>
  }
  let { component, childProps }: Props = $props()
  const Row = $derived(component)
</script>

<table>
  <tbody>
    <Row {...childProps} />
  </tbody>
</table>
