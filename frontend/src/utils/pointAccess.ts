// Access-mode vocabulary shared with the backend write guard
// (internal/services/services.go, readOnlyAccessModes). Stored devices use the
// canonical 'r'/'rw'/'w' from the device editor, but protocol templates and
// older API clients write the long forms, so both sides accept the aliases.

const READ_ONLY_MODES = new Set(['r', 'read', 'readonly', 'read_only'])

/** True when a point may not be written. Empty access mode is treated as writable. */
export function isReadOnlyPoint(p: { access_mode?: string } | undefined | null): boolean {
  if (!p?.access_mode) return false
  return READ_ONLY_MODES.has(p.access_mode.trim().toLowerCase())
}
