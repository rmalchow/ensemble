<script lang="ts" module>
  function screenTitle(name: string): string {
    switch (name) {
      case 'dashboard':
        return 'Dashboard'
      case 'cluster':
        return 'Cluster'
      case 'groups':
        return 'Groups'
      case 'media':
        return 'Media'
      case 'settings':
        return 'Settings'
      case 'node':
        return 'Node detail'
      default:
        return 'Not found'
    }
  }
</script>

<script lang="ts">
  // Root component. Runs the boot probe (GET /api/v1/status → /api/v1/auth/
  // session), installs the auth/init guard, and renders either a full-page route
  // (Setup / Login) or the AppShell with the routed screen. Screens themselves
  // are downstream pieces; this scaffold mounts lightweight placeholders so the
  // shell + routing + guard are exercisable end-to-end. (P0.4 §5.)
  import { onMount } from 'svelte'
  import { getStatus, getSession, clusterInfoFull, ApiError, type StatusProbe } from './lib/api'
  import { session, configVersion, clusterInfo } from './lib/stores'
  import { authGuard, currentRoute, startRouter } from './lib/router'
  import AppShell from './components/shell/AppShell.svelte'
  import ConfirmModal from './components/ConfirmModal.svelte'
  import Toasts from './components/Toasts.svelte'
  import StateMachine from './components/state/StateMachine.svelte'
  import Card from './components/ui/Card.svelte'
  import SetupWizard from './screens/SetupWizard.svelte'
  import Login from './screens/Login.svelte'
  import Settings from './screens/Settings.svelte'
  import Cluster from './screens/Cluster.svelte'
  import Dashboard from './screens/Dashboard.svelte'
  import Groups from './screens/Groups.svelte'
  import Media from './screens/Media.svelte'
  import NodeDetail from './routes/NodeDetail.svelte'

  // Boot phases: probe (loading) → error (probe failed, retry) → ready (router
  // installed; the template branches by route).
  type Phase = 'probe' | 'error' | 'ready'
  let phase = $state<Phase>('probe')
  let bootError = $state<{ code: string; message: string } | null>(null)

  // initialized/authenticated drive the guard. Undefined until the probe runs.
  let initialized = $state<boolean | undefined>(undefined)
  let authenticated = $state<boolean | undefined>(undefined)
  // probe holds the boot identity (nodeId / CSR fingerprint / clusterName) for
  // the Setup Wizard adopt panel and the Login footer chips.
  let probe = $state<StatusProbe | null>(null)

  let disposeRouter: (() => void) | null = null

  // guard delegates to the pure authGuard rule (unit-tested in router.test.ts),
  // closing over the live initialized/authenticated probe results.
  function guard(path: string): string | null {
    return authGuard(initialized, authenticated, path)
  }

  async function boot() {
    phase = 'probe'
    bootError = null
    try {
      const st = await getStatus()
      probe = st.data
      initialized = st.data.initialized
      // Seed the header when the probe carries the identity (bootstrap-open
      // path); never CLEAR it here — a member node's probe has no clusterName
      // (bootstrap 403) and must not wipe a value set by setup/login flows.
      if (st.data.clusterName) {
        clusterInfo.set({ name: st.data.clusterName, caFingerprint: st.data.fingerprint })
      }
      if (!initialized) {
        authenticated = false
        installRouter()
        return
      }
      const se = await getSession()
      authenticated = se.data.authenticated
      if (se.data.authenticated) {
        session.set({ authenticated: true, nodeId: se.data.nodeId })
        configVersion.set(se.data.configVersion)
        seedHeader()
      } else {
        session.set(null)
      }
      installRouter()
    } catch (e) {
      // Probe failed — never guess. Show the error state with Retry (09 screen 1
      // "Detection must be resilient").
      const err = e instanceof ApiError
        ? { code: e.code, message: e.message }
        : { code: 'unreachable', message: 'Could not reach this node.' }
      bootError = err
      phase = 'error'
    }
  }

  function installRouter() {
    disposeRouter?.()
    disposeRouter = startRouter(guard)
    phase = 'ready'
  }

  // seedHeader refreshes the header's cluster identity (name + CA fingerprint)
  // best-effort once authenticated; failures leave the current value in place.
  function seedHeader() {
    void clusterInfoFull()
      .then(({ data }) =>
        clusterInfo.set({
          name: data.cluster.name,
          caFingerprint: data.cluster.caFingerprint,
        }),
      )
      .catch(() => {})
  }

  onMount(() => {
    void boot()
    return () => disposeRouter?.()
  })

  const routeName = $derived($currentRoute.name)
</script>

{#if phase === 'error' && bootError}
  <div class="fullpage">
    <Card title="Cannot reach node">
      <StateMachine state="error" error={bootError} onRetry={boot} />
    </Card>
  </div>
{:else if phase === 'probe'}
  <div class="fullpage center">
    <StateMachine state="loading" />
  </div>
{:else if routeName === 'setup' && probe}
  <SetupWizard {probe} onComplete={boot} />
{:else if routeName === 'login'}
  <Login
    nodeId={probe?.nodeId}
    clusterName={$clusterInfo?.name}
    onAuthenticated={() => {
      // Flip the live guard flag so Login's post-login navigate is allowed
      // through (the guard closure reads this state at call time); Login itself
      // already refreshed the session/configVersion stores. Re-seed the header.
      authenticated = true
      seedHeader()
    }}
  />
{:else if routeName === 'settings'}
  <AppShell>
    <Settings />
  </AppShell>
{:else if routeName === 'cluster'}
  <AppShell>
    <Cluster />
  </AppShell>
{:else if routeName === 'dashboard'}
  <AppShell>
    <Dashboard />
  </AppShell>
{:else if routeName === 'groups'}
  <AppShell>
    <Groups />
  </AppShell>
{:else if routeName === 'media'}
  <AppShell>
    <Media />
  </AppShell>
{:else if routeName === 'node'}
  <AppShell>
    {#key $currentRoute.params.id}
      <NodeDetail id={$currentRoute.params.id} />
    {/key}
  </AppShell>
{:else}
  <AppShell>
    <Card title={screenTitle(routeName)}>
      <p class="muted">{screenTitle(routeName)} — not found.</p>
    </Card>
  </AppShell>
{/if}

<ConfirmModal />
<Toasts />

<style>
  .fullpage {
    min-height: 100%;
    padding: var(--space-8);
  }
  .fullpage.center {
    display: grid;
    place-items: center;
  }
  .muted {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
</style>
