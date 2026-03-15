#!/bin/bash
#
# CLIProxyAPI Fork Upgrade Script
# Merges upstream changes, rebuilds Docker image, and restarts.
# Usage: ./upgrade.sh
#

set -euo pipefail

# --- Config ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$SCRIPT_DIR"
CONTAINER_NAME="cli-proxy-api"
IMAGE_NAME="cliproxyapi:latest"
API_PORT="${CLI_PROXY_API_PORT:-8317}"

DOCKER_RUN_OPTS=(
    -d --name "$CONTAINER_NAME" --restart=always
    -p 8085:8085 -p 8317:8317 -p 11451:11451
    -p 1455:1455 -p 51121:51121 -p 54545:54545
    -v "$PROJECT_DIR/config.yaml:/CLIProxyAPI/config.yaml"
    -v "$PROJECT_DIR/auths:/root/.cli-proxy-api"
    -v "$PROJECT_DIR/logs:/CLIProxyAPI/logs"
)

UPSTREAM_REMOTE="upstream"
UPSTREAM_BRANCH="main"
BACKUP_DIR="$PROJECT_DIR/backups"
TIMESTAMP=$(date +"%Y%m%d_%H%M%S")

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

# --- Main ---
cd "$PROJECT_DIR"

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}   CLIProxyAPI Fork Upgrade${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""

# Step 1: Backup
echo -e "${YELLOW}[1/5] Backup...${NC}"
BACKUP_PATH="$BACKUP_DIR/backup_$TIMESTAMP"
mkdir -p "$BACKUP_PATH"
[ -f config.yaml ] && cp config.yaml "$BACKUP_PATH/" && echo "  config.yaml"
[ -d auths ] && cp -r auths "$BACKUP_PATH/" && echo "  auths/"
log "Saved to $BACKUP_PATH"
echo ""

# Step 2: Export usage stats
echo -e "${YELLOW}[2/5] Export usage stats...${NC}"
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

# Step 3: Merge upstream
echo -e "${YELLOW}[3/5] Merge upstream ($UPSTREAM_REMOTE/$UPSTREAM_BRANCH)...${NC}"
git fetch "$UPSTREAM_REMOTE" "$UPSTREAM_BRANCH"

UPSTREAM_HEAD=$(git rev-parse "$UPSTREAM_REMOTE/$UPSTREAM_BRANCH")
LOCAL_HEAD=$(git rev-parse HEAD)
MERGE_BASE=$(git merge-base HEAD "$UPSTREAM_REMOTE/$UPSTREAM_BRANCH")

if [ "$UPSTREAM_HEAD" = "$MERGE_BASE" ]; then
    log "Already up to date with upstream"
    echo ""
    echo -ne "${YELLOW}Rebuild anyway? [y/N]: ${NC}"
    read -r REBUILD
    if [[ ! "$REBUILD" =~ ^[Yy]$ ]]; then
        log "Nothing to do."
        exit 0
    fi
else
    BEHIND=$(git rev-list --count HEAD.."$UPSTREAM_REMOTE/$UPSTREAM_BRANCH")
    log "Upstream has $BEHIND new commit(s), merging..."
    if ! git merge "$UPSTREAM_REMOTE/$UPSTREAM_BRANCH" -m "merge: upstream $UPSTREAM_BRANCH"; then
        err "Merge conflict! Resolve manually, then re-run."
        echo ""
        echo "  Fix conflicts, then:"
        echo "    git add -A && git merge --continue"
        echo "    ./upgrade.sh   # re-run to build & deploy"
        echo ""
        echo "  Or abort:"
        echo "    git merge --abort"
        exit 1
    fi
    log "Merge successful"

    # Verify build after merge
    echo "  Verifying build..."
    if ! go build ./...; then
        err "Build failed after merge! Fix errors, commit, then re-run."
        exit 1
    fi
    log "Build verified"
fi
echo ""

# Step 4: Stop, rebuild & start
echo -e "${YELLOW}[4/5] Stop container, rebuild & start...${NC}"
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

# Step 5: Restore usage stats
echo -e "${YELLOW}[5/5] Restore usage stats...${NC}"
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

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}   Upgrade complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
docker ps --filter "name=$CONTAINER_NAME" --format "table {{.Names}}\t{{.Status}}\t{{.Image}}"
