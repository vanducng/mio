.PHONY: up down logs build tail tunnel-create test-sig

up:
	docker compose up -d --build

down:
	docker compose down

build:
	docker compose build

logs:
	docker compose logs -f

tail:
	tail -f appdata/messages.jsonl

# One-time tunnel bootstrap (run on host, requires cloudflared CLI):
#   make tunnel-create
tunnel-create:
	cloudflared tunnel login
	cloudflared tunnel create mio
	cloudflared tunnel route dns mio mio.vanducng.dev
	@echo ""
	@echo "Move ~/.cloudflared/<UUID>.json into ./cloudflared/, then:"
	@echo "  cp cloudflared/config.example.yml cloudflared/config.yml"
	@echo "  # replace <UUID> in config.yml with the new tunnel UUID"

# Quick local round-trip: signs a sample payload and POSTs to the receiver.
# Requires receiver up and WEBHOOK_SECRET in .env.
test-sig:
	@set -a; . ./.env; set +a; \
	BODY='{"operation":"message_sent","data":{"message":{"text":"hello from make test-sig"}},"user":{"name":"local-test"},"chat":{"title":"#DucDev"}}'; \
	SIG=$$(printf '%s' "$$BODY" | openssl dgst -sha256 -hmac "$$WEBHOOK_SECRET" -hex | awk '{print $$2}'); \
	curl -sS -X POST http://127.0.0.1:8765/cliq \
	  -H "Content-Type: application/json" \
	  -H "X-Webhook-Signature: sha256=$$SIG" \
	  -d "$$BODY"; echo
