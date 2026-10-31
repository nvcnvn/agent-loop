SHELL := /bin/sh

DOCKER_COMPOSE ?= docker compose -f docker-compose.test.yml
OLLAMA_MODEL ?= gemma4:latest
OLLAMA_TIMEOUT_SECONDS ?= 900
AGENT_LOOP_REST_TIMEOUT ?= 900
LOCAL_API_ADDR ?= :8080
LOCAL_API_URL ?= http://localhost:8080
LOCAL_PG_DSN ?= postgres://postgres:postgres@localhost:45432/agentloop?sslmode=disable
DOCKER_OLLAMA_URL ?= http://localhost:41434
HOST_OLLAMA_URL ?= http://localhost:11434
HOST_DOCKER_OLLAMA_URL ?= http://host.docker.internal:11434

.DEFAULT_GOAL := help

.PHONY: help pg-up pg-down ollama-up ollama-down stack-down api-local-docker-ollama api-local-host-ollama test-local test-stack-docker-ollama test-stack-host-ollama

help:
	@printf '%s\n' \
	  'api-local-docker-ollama  Start local API with Postgres + Dockerized Ollama on localhost:41434' \
	  'api-local-host-ollama    Start local API with Postgres + host Ollama on localhost:11434' \
	  'test-local               Run REST integration tests against a local API on localhost:8080' \
	  'test-stack-docker-ollama Run the full Compose REST test stack with Dockerized Ollama' \
	  'test-stack-host-ollama   Run the full Compose REST test stack against host Ollama' \
	  'pg-up                    Start Postgres for local runs on localhost:45432' \
	  'ollama-up                Start Dockerized Ollama and pull $(OLLAMA_MODEL)' \
	  'stack-down               Stop and remove the Compose test stack'

pg-up:
	$(DOCKER_COMPOSE) up -d postgres

pg-down:
	$(DOCKER_COMPOSE) stop postgres

ollama-up:
	$(DOCKER_COMPOSE) up -d ollama
	$(DOCKER_COMPOSE) up --no-deps ollama-pull

ollama-down:
	$(DOCKER_COMPOSE) stop ollama

stack-down:
	$(DOCKER_COMPOSE) down

api-local-docker-ollama: pg-up ollama-up
	AGENT_LOOP_PG_DSN='$(LOCAL_PG_DSN)' \
	OLLAMA_BASE_URL='$(DOCKER_OLLAMA_URL)' \
	OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	OLLAMA_TIMEOUT_SECONDS='$(OLLAMA_TIMEOUT_SECONDS)' \
	go run ./cmd/agent-loop -addr '$(LOCAL_API_ADDR)'

api-local-host-ollama: pg-up
	AGENT_LOOP_PG_DSN='$(LOCAL_PG_DSN)' \
	OLLAMA_BASE_URL='$(HOST_OLLAMA_URL)' \
	OLLAMA_MODEL='$(OLLAMA_MODEL)' \
	OLLAMA_TIMEOUT_SECONDS='$(OLLAMA_TIMEOUT_SECONDS)' \
	go run ./cmd/agent-loop -addr '$(LOCAL_API_ADDR)'

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