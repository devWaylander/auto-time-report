# Список команд: make или make help

.DEFAULT_GOAL := help

BIN     := bin/bot
# --env-file /dev/null: .env нужен только контейнеру бота (env_file), подстановки в compose-файле из него не берём,
# иначе compose пытается раскрыть $ в паролях и сыплет предупреждениями
COMPOSE := docker compose --env-file /dev/null
# Линтер собирается тем же Go, что указан в go.mod: системный golangci-lint может быть собран старым Go
GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod)
LINT       := GOTOOLCHAIN=go$(GO_VERSION) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
# dbmate запускается в контейнере из docker-compose.yml, локальная установка не нужна
DBMATE  := $(COMPOSE) run --rm --no-deps migrate

.PHONY: help
help: ## Показать список команд
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- Разработка ---

.PHONY: build
build: ## Собрать бинарник в bin/bot
	go build -o $(BIN) ./cmd/bot

.PHONY: run
run: data ## Запустить бота локально (переменные из .env)
	go run ./cmd/bot

.PHONY: test
test: ## Прогнать тесты
	go test ./...

.PHONY: lint
lint: ## Прогнать golangci-lint
	$(LINT) run ./...

.PHONY: fmt
fmt: ## Отформатировать код
	gofmt -w cmd internal

.PHONY: tidy
tidy: ## Привести в порядок go.mod и go.sum
	go mod tidy

# --- Миграции (dbmate) ---

.PHONY: migrate-up
migrate-up: data ## Накатить все новые миграции
	$(DBMATE) up

.PHONY: migrate-down
migrate-down: data ## Откатить последнюю миграцию
	$(DBMATE) down

.PHONY: migrate-status
migrate-status: data ## Показать, какие миграции накатаны
	$(DBMATE) status

.PHONY: migrate-new
migrate-new: data ## Создать миграцию: make migrate-new name=add_something
	@test -n "$(name)" || { echo "Укажи имя: make migrate-new name=add_something"; exit 1; }
	$(DBMATE) new $(name)

# --- Docker ---

.PHONY: up
up: data ## Собрать и поднять всё в Docker (миграции накатятся сами)
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Остановить контейнеры
	$(COMPOSE) down

.PHONY: logs
logs: ## Смотреть логи бота
	$(COMPOSE) logs -f bot

.PHONY: ps
ps: ## Статус контейнеров
	$(COMPOSE) ps

# Каталог для базы создаём заранее, иначе Docker создаст его от root
data:
	@mkdir -p data
