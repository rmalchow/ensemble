<script lang="ts">
  // Settings (09 §8). Composes the four panels: change admin password, API keys,
  // cluster info + CA fingerprint, and the danger-zone leave/reset. Each panel
  // owns its own load + If-Match write; this screen only lays them out and wires
  // the leave → re-probe → /setup transition.
  import Card from '../components/ui/Card.svelte'
  import ChangePassword from '../components/settings/ChangePassword.svelte'
  import ApiKeys from '../components/settings/ApiKeys.svelte'
  import ClusterInfo from '../components/settings/ClusterInfo.svelte'
  import DangerZone from '../components/settings/DangerZone.svelte'
  import { clusterInfo } from '../lib/stores'
  import { navigate } from '../lib/router'
  import { pushToast } from '../lib/toast'

  // After a successful leave, re-probe and land on /setup (09 §8). A non-
  // coordinated wipe warns the operator that peers were offline.
  function onLeft(coordinated: boolean) {
    if (!coordinated) {
      pushToast(
        'Left locally, but peers were unreachable — forget this node from another node.',
        'error',
        { sticky: true },
      )
    }
    navigate('/setup')
  }

  // reload re-reads cluster info to refresh configVersion after a 409 (passed to
  // ChangePassword as its reload-&-reapply action). The ClusterInfo panel owns
  // the actual read; bumping its key remounts it to re-fetch.
  let infoKey = $state(0)
  function reapply() {
    infoKey++
  }
</script>

<div class="settings">
  <Card title="Admin password">
    <ChangePassword onReloadReapply={reapply} />
  </Card>

  <Card title="API keys">
    <ApiKeys />
  </Card>

  <Card title="Cluster">
    {#key infoKey}
      <ClusterInfo />
    {/key}
  </Card>

  <Card title="Danger zone">
    <DangerZone clusterName={$clusterInfo?.name} {onLeft} />
  </Card>
</div>

<style>
  .settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    max-width: 52rem;
  }
</style>
