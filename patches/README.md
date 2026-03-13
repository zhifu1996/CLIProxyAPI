# Fork Patch Files

Upgrade workflow: `git reset --hard upstream/main` → copy custom files → `git apply` patches.

## Files

- `fork_modifications.patch` — All modifications to upstream files (apply with `git apply`)
- `custom_files.txt` — List of fork-only files to copy back after reset
- `deleted_files.txt` — Upstream files to delete after reset

## Usage

See `upgrade.sh` in the project root.
