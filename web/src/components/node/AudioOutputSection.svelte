<script lang="ts">
  // Audio-output section wrapper (09 §6, D17, 06 §1.5). Chooses variant A vs B by
  // the LIVE renderEnabled flag (draft.capabilities.render ?? loaded.caps.render,
  // previewed by nodeStore.isRenderEnabled) so the force-control-only toggle flips
  // it pre-save:
  //   A (renderEnabled): ChannelRole + Gain + HwDelay (manual entry) + Calibration
  //                      helper.
  //   B (!renderEnabled): the "Control / media only" panel — channel/gain/HWDelay
  //                      + helper are HIDDEN.
  // The draft values (channel/gain/hwDelay) are read by the screen and threaded in
  // so edits + Revert reflect live; `disabled` = offline || saving.
  import ChannelRoleRadio from './ChannelRoleRadio.svelte'
  import GainSlider from './GainSlider.svelte'
  import HwDelayControl from './HwDelayControl.svelte'
  import CalibrationHelper from './CalibrationHelper.svelte'
  import ControlMediaOnlyPanel from './ControlMediaOnlyPanel.svelte'
  import Field from '../ui/Field.svelte'
  import type { NodeDetailView } from '../../lib/node'
  import type { Channel } from '../../lib/types'

  import DeviceField from './DeviceField.svelte'

  interface Props {
    node: NodeDetailView
    renderEnabled: boolean
    disabled: boolean
    channel: Channel
    gainDb: number
    hwDelayUs: number
    device: string
    liveSyncErrorUs: number | null
  }
  let {
    node,
    renderEnabled,
    disabled,
    channel,
    gainDb,
    hwDelayUs,
    device,
    liveSyncErrorUs,
  }: Props = $props()
</script>

{#if renderEnabled}
  <div class="audio-out" data-variant="A">
    <Field
      label="Audio device"
      id="audio-device"
      hint="persisted per-node — empty = auto-select backend default"
    >
      <DeviceField value={device} {node} {disabled} />
    </Field>

    <Field label="Channel role" id="channel-role">
      <ChannelRoleRadio value={channel} {disabled} />
    </Field>

    <Field label="Gain (dB)" id="gain">
      <GainSlider value={gainDb} {disabled} />
    </Field>

    <Field
      label="Hardware delay (HWDelayUs)"
      id="hw-delay"
      hint="manual entry — integer µs"
    >
      <HwDelayControl value={hwDelayUs} {disabled} />
    </Field>

    <CalibrationHelper {node} {liveSyncErrorUs} {disabled} />
  </div>
{:else}
  <div class="audio-out" data-variant="B">
    <ControlMediaOnlyPanel />
  </div>
{/if}

<style>
  .audio-out {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
</style>
