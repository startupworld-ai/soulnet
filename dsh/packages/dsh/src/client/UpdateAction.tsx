/**
 * Icon-only update button at the RIGHT end of the sidebar-foot SoulMirror row
 * (list seat `sidebar.footer.action`, ordered after the entry): invisible
 * until a newer release is known, then a small circled arrow. One click runs
 * the full install-restart-reload chain; while busy the tooltip narrates the
 * phase and the icon pulses. The reloaded page re-checks as current and the
 * button removes itself.
 */
import { useSyncExternalStore } from 'react'
import { Tooltip } from '@deepseek-ai/dsh-client-ui-primitives'

import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { NS } from './locales.ts'
import { upgradeStore } from './upgrade-store.ts'

/** Upward arrow (update available). */
function ArrowUpIcon({ size = 13 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" aria-hidden>
      <path d="M8 13.2V3.6M3.6 7.6 8 3.2l4.4 4.4" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

/** Spinning arc shown while the upgrade chain runs. */
function SpinnerIcon({ size = 13 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" aria-hidden className="sm-update-spin">
      <circle cx="8" cy="8" r="6" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeDasharray="28" strokeDashoffset="20" />
    </svg>
  )
}

export function UpdateAction({ t }: { wide: boolean } & PropsLocale<typeof NS>) {
  const upgrade = useSyncExternalStore(upgradeStore.subscribe, upgradeStore.getSnapshot)
  const busy = upgrade.phase === 'installing' || upgrade.phase === 'restarting' || upgrade.phase === 'reloading'
  if (!upgrade.hasUpdate && !busy) return null
  const label = busy
    ? upgrade.phase === 'installing'
      ? t('page.update.installing', { v: upgrade.latest ?? '' })
      : upgrade.phase === 'restarting'
        ? t('page.update.restarting')
        : t('page.update.reloading')
    : t('sidebar.update', { v: upgrade.latest ?? '' })
  return (
    <Tooltip label={label} delayMs={200}>
      <button
        type="button"
        className={`sm-update-fab${busy ? ' sm-busy' : ''}`}
        disabled={busy}
        aria-label={label}
        data-soulmirror-update-action={upgrade.latest}
        onClick={() => {
          if (busy || upgrade.latest === undefined) return
          void upgradeStore.run()
        }}
      >
        {busy ? <SpinnerIcon /> : <ArrowUpIcon />}
      </button>
    </Tooltip>
  )
}
