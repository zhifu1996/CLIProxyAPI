#!/bin/bash
#
# CLIProxyAPI Fork Upgrade Script
# Reset to upstream, re-apply custom files + patches, rebuild Docker image, restart.
# Usage: ./upgrade.sh
#

set -euo pipefail

# --- Config ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$SCRIPT_DIR"
CONTAINER_NAME="cli-proxy-api"
IMAGE_NAME="cliproxyapi:latest"
API_PORT="${CLI_PROXY_API_PORT:-8317}"

# Docker run options — edit ports/volumes here
DOCKER_RUN_OPTS=(
    -d --name "$CONTAINER_NAME" --restart=always
    -p 8085:8085 -p 8317:8317 -p 11451:11451
    -p 1455:1455 -p 51121:51121 -p 54545:54545
    -v "$PROJECT_DIR/config.yaml:/CLIProxyAPI/config.yaml"
    -v "$PROJECT_DIR/auths:/root/.cli-proxy-api"
    -v "$PROJECT_DIR/logs:/CLIProxyAPI/logs"
)

# Upstream remote (original repo)
UPSTREAM_REMOTE="upstream"
UPSTREAM_BRANCH="main"

# Backup
BACKUP_DIR="$PROJECT_DIR/backups"
TIMESTAMP=$(date +"%Y%m%d_%H%M%S")

# Stash dir for custom files during upgrade
STASH_DIR=$(mktemp -d "${TMPDIR:-/tmp}/cliproxy-upgrade.XXXXXX")

# Management API password (for usage export/import)
if [ -z "${MANAGEMENT_PASSWORD:-}" ] && [ -f "$PROJECT_DIR/.env" ]; then
    MANAGEMENT_PASSWORD=$(grep -E "^MANAGEMENT_PASSWORD=" "$PROJECT_DIR/.env" 2>/dev/null | cut -d'=' -f2- | tr -d '"' | tr -d "'" || true)
fi

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[OK]${NC} $*"; }
warn() { echo -e "${YELLOW}[!!]${NC} $*"; }
err()  { echo -e "${RED}[ERR]${NC} $*"; }

cleanup() {
    rm -rf "$STASH_DIR"
}
trap cleanup EXIT

# --- Main ---
cd "$PROJECT_DIR"

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}   CLIProxyAPI Fork Upgrade${NC}"
echo -e "${GREEN}   (reset + patch mode)${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""

# Step 1: Backup
echo -e "${YELLOW}[1/7] Backup...${NC}"
BACKUP_PATH="$BACKUP_DIR/backup_$TIMESTAMP"
mkdir -p "$BACKUP_PATH"
[ -f config.yaml ] && cp config.yaml "$BACKUP_PATH/" && echo "  config.yaml"
[ -d auths ] && cp -r auths "$BACKUP_PATH/" && echo "  auths/"
log "Saved to $BACKUP_PATH"
echo ""

# Step 2: Export usage stats
echo -e "${YELLOW}[2/7] Export usage stats...${NC}"
USAGE_BACKUP=""
if [ -n "${MANAGEMENT_PASSWORD:-}" ]; then
    USAGE_BACKUP="$BACKUP_PATH/usage_stats.json"
    if curl -sf -H "Authorization: Bearer $MANAGEMENT_PASSWORD" \
        "http://localhost:$API_PORT/v0/management/usage/export" -o "$USAGE_BACKUP" 2>/dev/null; then
        log "Usage exported"
    else
        warn "Export failed (service not running?), skipping"
        USAGE_BACKUP=""
    fi
else
    warn "MANAGEMENT_PASSWORD not set, skipping"
fi
echo ""

# Step 3: Fetch upstream & stash custom files
echo -e "${YELLOW}[3/7] Fetch upstream & stash custom files...${NC}"
git fetch "$UPSTREAM_REMOTE" "$UPSTREAM_BRANCH"

UPSTREAM_HEAD=$(git rev-parse "$UPSTREAM_REMOTE/$UPSTREAM_BRANCH")

# Stash custom files (new files not in upstream) before reset
echo "  Stashing custom files..."
if [ -f patches/custom_files.txt ]; then
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        if [ -f "$f" ]; then
            mkdir -p "$STASH_DIR/$(dirname "$f")"
            cp "$f" "$STASH_DIR/$f"
        fi
    done < patches/custom_files.txt
fi
# Also stash patches dir itself, upgrade.sh
mkdir -p "$STASH_DIR/patches"
cp -r patches/* "$STASH_DIR/patches/" 2>/dev/null || true
cp upgrade.sh "$STASH_DIR/upgrade.sh"
log "Custom files stashed to $STASH_DIR"
echo ""

# Step 4: Reset to upstream & apply customizations
echo -e "${YELLOW}[4/7] Reset to upstream & apply patches...${NC}"
git reset --hard "$UPSTREAM_REMOTE/$UPSTREAM_BRANCH"
log "Reset to $UPSTREAM_REMOTE/$UPSTREAM_BRANCH ($UPSTREAM_HEAD)"

# Copy back custom files
echo "  Restoring custom files..."
while IFS= read -r f; do
    [ -z "$f" ] && continue
    if [ -f "$STASH_DIR/$f" ]; then
        mkdir -p "$(dirname "$f")"
        cp "$STASH_DIR/$f" "$f"
    fi
done < "$STASH_DIR/patches/custom_files.txt"

# Copy back patches dir and upgrade.sh
cp -r "$STASH_DIR/patches" .
cp "$STASH_DIR/upgrade.sh" upgrade.sh

# Delete upstream files we don't want
if [ -f patches/deleted_files.txt ]; then
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        [ -f "$f" ] && rm -f "$f" && echo "  deleted: $f"
    done < patches/deleted_files.txt
fi

# Apply the modification patch
echo "  Applying fork_modifications.patch..."
if git apply --check patches/fork_modifications.patch 2>/dev/null; then
    git apply patches/fork_modifications.patch
    log "Patch applied cleanly"
else
    warn "Patch failed to apply cleanly. Trying with 3-way merge..."
    if git apply --3way patches/fork_modifications.patch; then
        log "Patch applied with 3-way merge (check for conflicts)"
    else
        err "Patch failed! Manual intervention required."
        echo ""
        echo "  The patch file is: patches/fork_modifications.patch"
        echo "  After resolving, run:  go build ./..."
        echo "  Then re-run upgrade.sh (it will skip the already-reset step)"
        exit 1
    fi
fi

# Update go dependencies
echo "  Running go mod tidy..."
go mod tidy 2>/dev/null || warn "go mod tidy had issues (may need manual fix)"
echo ""

# Verify build
echo -e "${YELLOW}[5/7] Verify build...${NC}"
if go build ./...; then
    log "Build successful"
else
    err "Build failed! Fix errors then re-run."
    exit 1
fi
echo ""

# Commit all customizations as a single commit
git add -A
git commit -m "feat: apply fork customizations (uTLS, gRPC, telemetry)" --allow-empty 2>/dev/null || true
log "Committed customizations"
echo ""

# Step 6: Stop, rebuild & start
echo -e "${YELLOW}[6/7] Stop container, rebuild & start...${NC}"
if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    docker stop "$CONTAINER_NAME" 2>/dev/null || true
    docker rm "$CONTAINER_NAME" 2>/dev/null || true
    log "Container stopped"
else
    warn "Container not found, skipping stop"
fi

docker build -t "$IMAGE_NAME" "$PROJECT_DIR"
docker run "${DOCKER_RUN_OPTS[@]}" "$IMAGE_NAME"
log "Container started"
echo ""

# Step 7: Restore usage stats
echo -e "${YELLOW}[7/7] Restore usage stats...${NC}"
if [ -n "$USAGE_BACKUP" ] && [ -f "$USAGE_BACKUP" ]; then
    sleep 5
    for i in 1 2 3; do
        if curl -sf -X POST \
            -H "Content-Type: application/json" \
            -H "Authorization: Bearer $MANAGEMENT_PASSWORD" \
            -d @"$USAGE_BACKUP" \
            "http://localhost:$API_PORT/v0/management/usage/import" >/dev/null 2>&1; then
            log "Usage stats restored"
            break
        else
            [ $i -lt 3 ] && sleep 3 || warn "Import failed after 3 attempts"
        fi
    done
else
    warn "No usage backup to restore"
fi
echo ""

# Cleanup old backups (keep last 10)
if [ -d "$BACKUP_DIR" ]; then
    cd "$BACKUP_DIR"
    ls -dt backup_* 2>/dev/null | tail -n +11 | xargs -r rm -rf
fi

# Done
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}   Upgrade complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
docker ps --filter "name=$CONTAINER_NAME" --format "table {{.Names}}\t{{.Status}}\t{{.Image}}"
