#!/bin/bash
# =============================================================================
# Zoho Cliq OAuth Setup Script (Server-based Application)
# =============================================================================
# This script helps you complete the OAuth flow to get a refresh token
# for the Zoho Cliq integration with Hatchet.
#
# Prerequisites:
#   - Server-based Application created at https://api-console.zoho.com/
#   - Client ID and Client Secret saved in secrets.env
#
# Usage:
#   1. Run: ./setup-zoho-cliq-oauth.sh
#   2. Open the URL in browser, authorize, and paste the redirect URL
#   3. Script exchanges code for refresh token and updates secrets.env
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SECRETS_FILE="$SCRIPT_DIR/secrets.env"
SCI_ENV_FILE="$SCRIPT_DIR/../../services/sci/.env"

# Load existing secrets
if [ ! -f "$SECRETS_FILE" ]; then
    echo "Error: $SECRETS_FILE not found"
    exit 1
fi
source "$SECRETS_FILE"

# Validate required vars
if [ -z "${ZOHO_CLIQ_CLIENT_ID:-}" ] || [ -z "${ZOHO_CLIQ_CLIENT_SECRET:-}" ]; then
    echo "Error: ZOHO_CLIQ_CLIENT_ID and ZOHO_CLIQ_CLIENT_SECRET must be set in secrets.env"
    exit 1
fi

# Default redirect URI - update if you used a different one
REDIRECT_URI="${ZOHO_CLIQ_REDIRECT_URI:-http://localhost:8080}"
SCOPES="ZohoCliq.Webhooks.CREATE,ZohoCliq.Channels.ALL,ZohoCliq.Messages.CREATE,ZohoCliq.Chats.READ,ZohoCliq.Channels.READ,ZohoCliq.Messages.READ,ZohoCliq.OrganizationMessages.READ,ZohoCliq.messageactions.CREATE,ZohoCliq.messageactions.READ,ZohoCliq.messageactions.DELETE"

echo "=== Zoho Cliq OAuth Setup ==="
echo ""

# Production-safe behavior:
#   - The existing ZOHO_CLIQ_REFRESH_TOKEN (used elsewhere) is NEVER overwritten.
#   - The new token from this auth flow is saved as ZOHO_CLIQ_REFRESH_TOKEN_PLAYGROUND.
#   - Both refresh tokens stay valid at Zoho concurrently (multiple per client allowed).
TARGET_VAR="ZOHO_CLIQ_REFRESH_TOKEN_PLAYGROUND"
if [ -n "${!TARGET_VAR:-}" ]; then
    echo "$TARGET_VAR already set in secrets.env."
    read -p "Generate a new one (replaces only the playground value)? (y/N): " REGENERATE
    if [ "$REGENERATE" != "y" ] && [ "$REGENERATE" != "Y" ]; then
        echo "Aborted."
        exit 0
    fi
fi

# Step 1: Generate auth URL
AUTH_URL="https://accounts.zoho.com/oauth/v2/auth?scope=${SCOPES}&client_id=${ZOHO_CLIQ_CLIENT_ID}&response_type=code&access_type=offline&redirect_uri=${REDIRECT_URI}&prompt=consent"

echo "Step 1: Opening authorization URL in your browser..."
echo ""
echo "$AUTH_URL"
echo ""

# Auto-open in browser (macOS)
open "$AUTH_URL" 2>/dev/null || xdg-open "$AUTH_URL" 2>/dev/null || echo "(Could not auto-open - copy the URL above manually)"
echo ""
echo "After authorizing, you'll be redirected to a URL like:"
echo "  ${REDIRECT_URI}?code=XXXXX&..."
echo ""
read -p "Paste the FULL redirect URL here: " REDIRECT_RESPONSE

# Extract code from URL
AUTH_CODE=$(echo "$REDIRECT_RESPONSE" | grep -oP 'code=\K[^&]+' 2>/dev/null || \
            echo "$REDIRECT_RESPONSE" | sed -n 's/.*code=\([^&]*\).*/\1/p')

if [ -z "$AUTH_CODE" ]; then
    echo "Error: Could not extract authorization code from URL"
    echo "If you copied just the code, paste it here:"
    read -p "Authorization code: " AUTH_CODE
fi

echo ""
echo "Step 2: Exchanging authorization code for tokens..."

# Exchange code for tokens
RESPONSE=$(curl -s -X POST "https://accounts.zoho.com/oauth/v2/token" \
    -d "grant_type=authorization_code" \
    -d "client_id=${ZOHO_CLIQ_CLIENT_ID}" \
    -d "client_secret=${ZOHO_CLIQ_CLIENT_SECRET}" \
    -d "redirect_uri=${REDIRECT_URI}" \
    -d "code=${AUTH_CODE}")

echo "Response: $RESPONSE"
echo ""

# Extract tokens
REFRESH_TOKEN=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('refresh_token',''))" 2>/dev/null || echo "")
ACCESS_TOKEN=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null || echo "")

if [ -z "$REFRESH_TOKEN" ]; then
    echo "Error: Failed to get refresh token. Check the response above."
    echo "Common issues:"
    echo "  - Authorization code expired (2 min TTL)"
    echo "  - Wrong redirect URI"
    echo "  - Code already used (generate a new one)"
    exit 1
fi

echo "Step 3: Updating secrets.env (writing $TARGET_VAR — leaving ZOHO_CLIQ_REFRESH_TOKEN untouched)..."

# Write/update the playground-only refresh token; never modify the production var.
if grep -q "^${TARGET_VAR}=" "$SECRETS_FILE"; then
    sed -i '' "s|^${TARGET_VAR}=.*|${TARGET_VAR}=${REFRESH_TOKEN}|" "$SECRETS_FILE"
else
    echo "${TARGET_VAR}=${REFRESH_TOKEN}" >> "$SECRETS_FILE"
fi

echo "secrets.env updated with $TARGET_VAR."
echo ""

# Step 4: Send one test post to #ducdev as the bot (verifies channel post works
# AND confirms the message appears as TobyTime, not the OAuth user).
CHANNEL="${ZOHO_CLIQ_CHANNEL_NAME:-ducdev}"
BOT="${ZOHO_CLIQ_BOT_NAME:-tobytime}"
echo "Step 4: Posting verification message to #${CHANNEL} as bot ${BOT}..."

TEMP_MSG=$(mktemp)
echo "{\"text\":\"OAuth re-auth complete. Playground refresh token active with reactions scope.\"}" > "$TEMP_MSG"

HTTP_CODE=$(curl -s -o /tmp/setup-test-resp -w "%{http_code}" -X POST \
    "https://cliq.zoho.com/api/v2/channelsbyname/${CHANNEL}/message?bot_unique_name=${BOT}" \
    -H "Authorization: Zoho-oauthtoken ${ACCESS_TOKEN}" \
    -H "Content-Type: application/json" \
    -d @"$TEMP_MSG")
rm -f "$TEMP_MSG"

echo "  HTTP ${HTTP_CODE}"
if [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "204" ]; then
    echo "  Posted as bot. Check #${CHANNEL} — sender should be ${BOT}, not your account."
else
    echo "  Response body:"; cat /tmp/setup-test-resp; echo
fi
echo ""
echo "=== Setup Complete ==="
echo ""
echo "Credentials saved to: $SECRETS_FILE"
echo ""
echo "To update SCI .env for local testing, run:"
echo "  $0 update-sci-env"
echo ""
echo "To test sending a message, run:"
echo "  $0 test"
