#!/bin/bash
# =============================================================================
# Test Zoho Cliq Bot - Send message to channel
# =============================================================================
# Usage:
#   ./test-zoho-cliq-send-message.sh                    # Send default test message
#   ./test-zoho-cliq-send-message.sh "Custom message"   # Send custom message
#   ./test-zoho-cliq-send-message.sh refresh             # Just refresh and print access token
#   ./test-zoho-cliq-send-message.sh update-sci-env      # Update SCI .env with Zoho Cliq vars
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SECRETS_FILE="$SCRIPT_DIR/secrets.env"
SCI_ENV_FILE="$SCRIPT_DIR/../../services/sci/.env"

# Load secrets
if [ ! -f "$SECRETS_FILE" ]; then
    echo "Error: $SECRETS_FILE not found. Run setup-zoho-cliq-oauth.sh first."
    exit 1
fi
source "$SECRETS_FILE"

# --- Command: update-sci-env ---
if [ "${1:-}" = "update-sci-env" ]; then
    echo "Updating SCI .env with Zoho Cliq variables..."

    VARS=(
        "ZOHO_CLIQ_CLIENT_ID"
        "ZOHO_CLIQ_CLIENT_SECRET"
        "ZOHO_CLIQ_REFRESH_TOKEN"
        "ZOHO_CLIQ_BOT_NAME"
        "ZOHO_CLIQ_CHANNEL_NAME"
    )

    for VAR in "${VARS[@]}"; do
        VALUE="${!VAR:-}"
        if [ -z "$VALUE" ]; then
            echo "  Warning: $VAR is empty, skipping"
            continue
        fi
        if grep -q "^${VAR}=" "$SCI_ENV_FILE" 2>/dev/null; then
            sed -i '' "s|^${VAR}=.*|${VAR}=${VALUE}|" "$SCI_ENV_FILE"
            echo "  Updated: $VAR"
        else
            echo "" >> "$SCI_ENV_FILE"
            echo "# Zoho Cliq Bot Integration" >> "$SCI_ENV_FILE"
            echo "${VAR}=${VALUE}" >> "$SCI_ENV_FILE"
            echo "  Added: $VAR"
        fi
    done

    echo "Done. SCI .env updated at: $SCI_ENV_FILE"
    exit 0
fi

# Validate required vars
for VAR in ZOHO_CLIQ_CLIENT_ID ZOHO_CLIQ_CLIENT_SECRET ZOHO_CLIQ_REFRESH_TOKEN; do
    if [ -z "${!VAR:-}" ]; then
        echo "Error: $VAR not set. Run setup-zoho-cliq-oauth.sh first."
        exit 1
    fi
done

# Refresh access token
echo "Refreshing access token..."
TOKEN_RESPONSE=$(curl -s -X POST "https://accounts.zoho.com/oauth/v2/token" \
    -d "grant_type=refresh_token" \
    -d "client_id=${ZOHO_CLIQ_CLIENT_ID}" \
    -d "client_secret=${ZOHO_CLIQ_CLIENT_SECRET}" \
    -d "refresh_token=${ZOHO_CLIQ_REFRESH_TOKEN}")

ACCESS_TOKEN=$(echo "$TOKEN_RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null || echo "")

if [ -z "$ACCESS_TOKEN" ]; then
    echo "Error: Failed to get access token"
    echo "Response: $TOKEN_RESPONSE"
    exit 1
fi

echo "Access token obtained (expires in 1hr)"

# --- Command: refresh ---
if [ "${1:-}" = "refresh" ]; then
    echo ""
    echo "Access Token: $ACCESS_TOKEN"
    echo ""
    echo "Use in curl:"
    echo "  curl -X POST 'https://cliq.zoho.com/api/v2/channelsbyname/tobytime/message?bot_unique_name=tobytime' \\"
    echo "    -H 'Authorization: Zoho-oauthtoken $ACCESS_TOKEN' \\"
    echo "    -H 'Content-Type: application/json' \\"
    echo "    -d '{\"text\":\"Hello!\"}'"
    exit 0
fi

# --- Command: send message ---
CHANNEL="${ZOHO_CLIQ_CHANNEL_NAME:-tobytime}"
BOT="${ZOHO_CLIQ_BOT_NAME:-tobytime}"
MESSAGE="${1:-Test message from Zoho Cliq integration at $(date '+%Y-%m-%d %H:%M:%S')}"

echo "Sending to #${CHANNEL} as ${BOT}..."

# Write JSON to temp file to avoid shell escaping issues
TEMP_MSG=$(mktemp)
python3 -c "import json; print(json.dumps({'text': '''${MESSAGE}'''}))" > "$TEMP_MSG"

RESPONSE=$(curl -s -X POST \
    "https://cliq.zoho.com/api/v2/channelsbyname/${CHANNEL}/message?bot_unique_name=${BOT}" \
    -H "Authorization: Zoho-oauthtoken ${ACCESS_TOKEN}" \
    -H "Content-Type: application/json" \
    -d @"$TEMP_MSG")

rm -f "$TEMP_MSG"

if [ -z "$RESPONSE" ]; then
    echo "Message sent successfully! (204 No Content)"
else
    echo "Response: $RESPONSE"
fi
