SHELL := /bin/bash
COMPOSE := docker compose
CLI := $(COMPOSE) run --rm cli

.PHONY: help up down logs ps reset \
        streams streams-rm consumers consumers-rm bootstrap \
        publish-inbound publish-outbound \
        sub-ai sub-sender peek-inbound peek-outbound \
        replay info

help:
	@echo "make up                 # start NATS JetStream + nats-box CLI"
	@echo "make down               # stop containers (keeps volume)"
	@echo "make reset              # stop + wipe jetstream data"
	@echo ""
	@echo "make bootstrap          # create both streams + both consumers"
	@echo "make streams            # add MESSAGES_INBOUND + MESSAGES_OUTBOUND"
	@echo "make consumers          # add ai-consumer + sender-pool"
	@echo "make streams-rm         # delete both streams"
	@echo "make consumers-rm       # delete both consumers"
	@echo ""
	@echo "make publish-inbound TEXT='hi'  # publish a fake inbound message"
	@echo "make publish-outbound TEXT='ok' # publish a fake outbound command"
	@echo ""
	@echo "make sub-ai             # pull-subscribe as ai-consumer (Ctrl-C to stop)"
	@echo "make sub-sender         # pull-subscribe as sender-pool"
	@echo "make peek-inbound       # view inbound stream contents (read-only)"
	@echo "make peek-outbound      # view outbound stream contents"
	@echo ""
	@echo "make replay             # exercise replay: new consumer reads from start"
	@echo "make info               # show stream + consumer state"

up:
	$(COMPOSE) up -d nats
	@echo "NATS up at nats://localhost:4222 (monitoring http://localhost:8222)"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f nats

ps:
	$(COMPOSE) ps

reset:
	$(COMPOSE) down -v
	rm -rf appdata/jetstream

streams:
	$(CLI) stream add --config /streams/messages-inbound.json
	$(CLI) stream add --config /streams/messages-outbound.json

streams-rm:
	-$(CLI) stream rm -f MESSAGES_INBOUND
	-$(CLI) stream rm -f MESSAGES_OUTBOUND

consumers:
	$(CLI) consumer add MESSAGES_INBOUND --config /consumers/inbound-ai-consumer.json
	$(CLI) consumer add MESSAGES_OUTBOUND --config /consumers/outbound-sender-consumer.json

consumers-rm:
	-$(CLI) consumer rm -f MESSAGES_INBOUND ai-consumer
	-$(CLI) consumer rm -f MESSAGES_OUTBOUND sender-pool

bootstrap: streams consumers info

# --- publish helpers --------------------------------------------------------

TEXT ?= hello mio

publish-inbound:
	$(CLI) pub mio.inbound.zoho-cliq.workspace-1.thread-42 "$(TEXT)"

publish-outbound:
	$(CLI) pub mio.outbound.zoho-cliq.workspace-1.thread-42 "$(TEXT)"

# --- subscribe / inspect ----------------------------------------------------

sub-ai:
	$(CLI) consumer next MESSAGES_INBOUND ai-consumer --count=10

sub-sender:
	$(CLI) consumer next MESSAGES_OUTBOUND sender-pool --count=10

peek-inbound:
	$(CLI) stream view MESSAGES_INBOUND

peek-outbound:
	$(CLI) stream view MESSAGES_OUTBOUND

# --- replay drill -----------------------------------------------------------
# Adds a fresh ephemeral consumer that reads from sequence 1 — proves the
# inbound stream is replayable for prompt iteration.

replay:
	@echo "Adding replay consumer at seq=1; will print up to 20 msgs then exit."
	$(CLI) consumer add MESSAGES_INBOUND replay-drill \
		--target=replay-drill \
		--deliver=all --ack=explicit --pull \
		--max-deliver=1 --max-pending=1 --replay=instant \
		--filter='mio.inbound.>' --defaults
	-$(CLI) consumer next MESSAGES_INBOUND replay-drill --count=20
	-$(CLI) consumer rm -f MESSAGES_INBOUND replay-drill

info:
	@echo "=== streams ==="
	$(CLI) stream ls
	@echo "=== consumers (inbound) ==="
	-$(CLI) consumer ls MESSAGES_INBOUND
	@echo "=== consumers (outbound) ==="
	-$(CLI) consumer ls MESSAGES_OUTBOUND
