/**
 * The `alter.card` client slot: cards are the pluggable modules that render on
 * the "My alter" home tab (the alter's own page). The AlterPane is the card
 * HOST: it renders every registered card through this LIST seat (in slot
 * order); the built-in cards ship with this package (profile, settings), and
 * any other dsh plugin can add another card — memory, skills, group memory,
 * whatever — by registering into `alter.card` the same way a third-party room
 * registers into `group.room` (see "How to write a room plugin" in the README,
 * and group-room.ts for the room variant of this pattern).
 *
 * The seat is DECLARED by the soulmirror-page registration's `children` table
 * (client/index.ts); the page hands `renderSlot` down to AlterPane as plain
 * props, so the authorizing identity stays the page entry. This module holds
 * only the contract + the SlotMap merge (no runtime value import), so it is
 * unit-testable under node.
 */
// Import the augmented module so the augmentation below resolves in every
// program that pulls this file.
import type {} from '@deepseek-ai/dsh-client-ui-slots'
import type { SettingsScope } from '@deepseek-ai/dsh-client-runtime/client'
import type { SoulmirrorSettingsValues } from './SettingsSection.tsx'

/** What a card needs about the alter session itself. */
export interface AlterCardAlter {
  readonly sessionId: string | undefined
  readonly status: 'idle' | 'running'
}

/**
 * Owner props of one `alter.card` occupant: everything a card needs to render
 * one module of the alter's page. `scope` is the live `soulmirror` settings
 * namespace (a card reads values via `getSnapshot()` and writes via `set`).
 * The contract is deliberately small and additive — new cards extend the
 * SURFACE (their own settings / data) rather than this shape.
 */
export interface AlterCardOwnerProps {
  readonly alter: AlterCardAlter
  readonly scope: SettingsScope<SoulmirrorSettingsValues>
}

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface SlotMap {
    /**
     * Pluggable cards of the alter's home tab. One registrant per card module;
     * the AlterPane renders all of them in slot order. Built-in keys: `profile`
     * and `settings`.
     */
    'alter.card': {
      kind: 'list'
      scope: 'root'
      owner: AlterCardOwnerProps
    }
  }
}
