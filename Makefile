SHELL := /bin/sh

DOCKER_COMPOSE ?= docker compose -f docker-compose.test.yml
OLLAMA_MODEL ?= gemma4:e2b-it-qat
FUZZTIME ?= 30s
OLLAMA_TIMEOUT_SECONDS ?= 900
AGENT_LOOP_REST_TIMEOUT ?= 900
LOCAL_API_ADDR ?= :8080
LOCAL_API_URL ?= http://localhost:8080
LOCAL_PG_DSN ?= postgres://postgres:postgres@localhost:45432/agentloop?sslmode=disable
DOCKER_OLLAMA_URL ?= http://localhost:41434
HOST_OLLAMA_URL ?= http://localhost:11434
HOST_DOCKER_OLLAMA_URL ?= http://host.docker.internal:11434

.DEFAULT_GOAL := help

.PHONY: help test test-paid fuzz record-docker-ollama record-host-ollama record-openai pg-up pg-down ollama-up ollama-down stack-down api-local-docker-ollama api-local-host-ollama test-local test-stack-docker-ollama test-stack-host-ollama

help:
	@printf '%s\n' \
	  'test                     Unit tests, fuzz seeds and fixture-replayed scenarios; no network' \
	  'test-paid                Live tests against LLM_* from .env (ALLOW_PAID_LLM=1; costs money)' \
	  'fuzz                     Fuzz every Fuzz* target for $(FUZZTIME) each' \
	  'record-docker-ollama     Run ./scenario live against Dockerized Ollama and rewrite its fixtures' \
	  'record-host-ollama       Run ./scenario live against host Ollama on localhost:11434 and rewrite the fixtures of OLLAMA_MODEL' \
	  'record-openai            Run ./scenario live against LLM_BASE_URL/LLM_MODEL from .env (paid) and rewrite its fixtures' \
	  'api-local-docker-ollama  Start local API with Postgres + Dockerized Ollama on localhost:41434' \
	  'api-local-host-ollama    Start local API with Postgres + host Ollama on localhost:11434' \
	  'test-local               Run REST integration tests against a local API on localhost:8080' \
	  'test-stack-docker-ollama Run the full Compose REST test stack with Dockerized Ollama' \
	  'test-stack-host-ollama   Run the full Compose REST test stack against host Ollama' \
	  'pg-up                    Start Postgres for local runs on localhost:45432' \
	  'ollama-up                Start Dockerized Ollama and pull $(OLLAMA_MODEL)' \
	  'stack-down               Stop and remove the Compose test stack'

test:
	go test -race -count=1 ./...

fuzz:
	@for d in $$(grep -rl --include='*_test.go' '^func Fuzz' . | xargs -n1 dirname | sort -u); do \
	  for f in $$(grep -ho '^func Fuzz[A-Za-z0-9_]*' $$d/*_test.go | cut -c6-); do \
	    go test -run='^$$' -fuzz="^$$f$$" -fuzztime=$(FUZZTIME) $$d || exit 1; \
	  done; \
	done

record-docker-ollama: ollama-up
	OLLAMA_BASE_URL='$(DOCKER_OLLAMA_URL)' OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	go test -count=1 -v ./scenario -args -record

record-host-ollama:
	ollama pull '$(OLLAMA_MODEL)'
	OLLAMA_BASE_URL='$(HOST_OLLAMA_URL)' OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	go test -count=1 -v ./scenario -args -record

test-paid:
	set -a && . ./.env && set +a && \
	ALLOW_PAID_LLM=1 go test -count=1 -v -run '^TestLive' ./...

record-openai:
	set -a && . ./.env && set +a && \
	go test -count=1 -v ./scenario -args -record

pg-up:
	$(DOCKER_COMPOSE) up -d postgres

pg-down:
	$(DOCKER_COMPOSE) stop postgres

ollama-up:
	$(DOCKER_COMPOSE) up -d ollama
	$(DOCKER_COMPOSE) run --rm --no-deps ollama-pull

ollama-down:
	$(DOCKER_COMPOSE) stop ollama

stack-down:
	$(DOCKER_COMPOSE) down

api-local-docker-ollama: pg-up ollama-up
	AGENT_LOOP_PG_DSN='$(LOCAL_PG_DSN)' \
	OLLAMA_BASE_URL='$(DOCKER_OLLAMA_URL)' \
	OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	OLLAMA_TIMEOUT_SECONDS='$(OLLAMA_TIMEOUT_SECONDS)' \
	go run ./cmd/agent-loop -addr '$(LOCAL_API_ADDR)' -debug -trust-identity-headers

api-local-host-ollama: pg-up
	AGENT_LOOP_PG_DSN='$(LOCAL_PG_DSN)' \
	OLLAMA_BASE_URL='$(HOST_OLLAMA_URL)' \
	OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	OLLAMA_TIMEOUT_SECONDS='$(OLLAMA_TIMEOUT_SECONDS)' \
	go run ./cmd/agent-loop -addr '$(LOCAL_API_ADDR)' -debug -trust-identity-headers

test-local:
	AGENT_LOOP_REST_URL='$(LOCAL_API_URL)' \
	AGENT_LOOP_REST_TIMEOUT='$(AGENT_LOOP_REST_TIMEOUT)' \
	go test -tags=rest -count=1 ./api

test-stack-docker-ollama:
	OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	$(DOCKER_COMPOSE) up --build --abort-on-container-exit --exit-code-from rest-test ollama rest-test

test-stack-host-ollama:
	OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	OLLAMA_HOST='$(HOST_DOCKER_OLLAMA_URL)' \
	OLLAMA_BASE_URL='$(HOST_DOCKER_OLLAMA_URL)' \
	$(DOCKER_COMPOSE) up --build --abort-on-container-exit --exit-code-from rest-test postgres ollama-pull rest-test