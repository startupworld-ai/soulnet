/**
 * Module-level access to the host's directory picker (`ctx.workspaces`).
 * Set once in the client `apply` (bundle purity keeps `ctx` out of React
 * components); the AgentSettingsSheet reads it for its "browse" button next to
 * the working-directory field. Falls back to null when the host has no picker
 * or the user cancels, so the field stays editable by hand either way.
 */
let pick: (() => Promise<string | null>) | undefined

export function setDirectoryPicker(fn: (() => Promise<string | null>) | undefined): void {
  pick = fn
}

/** Open the host's native directory picker; resolves null when unavailable or cancelled. */
export async function pickDirectory(): Promise<string | null> {
  if (pick === undefined) return null
  try {
    return await pick()
  } catch {
    return null
  }
}
