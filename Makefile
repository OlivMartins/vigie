.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/OlivMartins/vigie/internal/version.version=$(VERSION) \
	-X github.com/OlivMartins/vigie/internal/version.commit=$(COMMIT)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
SONDE ?= ../sonde

export CGO_ENABLED := 0

.PHONY: help build demo demo-gateway test race cover fuzz bench lint compat conformance loadtest sonde-check dist clean

help: ## Affiche cette aide
	@grep -E '^[a-z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Compile bin/vigie et le serveur de la démo
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/vigie ./cmd/vigie
	go build -trimpath -o bin/demo-server ./examples/demo/server

demo: build ## Joue la démo : un serveur MCP mis à jour avec une description piégée
	@go run ./examples/demo

demo-gateway: build ## Joue la même démo en HTTP, à travers la passerelle
	@go run ./examples/demo -gateway

test: ## Lance les tests
	go test ./...

race: ## Lance les tests avec le détecteur de concurrence
	CGO_ENABLED=1 go test -race ./...

cover: ## Mesure la couverture des tests (cover.html)
	go test -coverprofile=cover.out ./...
	go tool cover -html=cover.out -o cover.html
	@go tool cover -func=cover.out | tail -1

fuzz: ## Fuzz l'analyseur JSON-RPC, le lecteur SSE, le détecteur de secrets et les détections, 30 s chacun
	go test -run='^$$' -fuzz=FuzzParseFrame -fuzztime=30s ./internal/jsonrpc
	go test -run='^$$' -fuzz=FuzzReader -fuzztime=30s ./internal/sse
	go test -run='^$$' -fuzz=FuzzScanJSON -fuzztime=30s ./internal/secrets
	go test -run='^$$' -fuzz=FuzzScan$$ -fuzztime=30s ./internal/detect

bench: ## Mesure le temps de traitement d'un appel d'outil et d'un tools/list
	go test -run='^$$' -bench=. -benchmem ./internal/proxy ./internal/gateway

lint: ## Vérifie le style et l'analyse statique (golangci-lint)
	golangci-lint run ./...

compat: build ## Teste Vigie entre les SDK MCP officiels (TypeScript v1 et v2)
	cd test/compat && npm ci --no-audit --no-fund && npm test

loadtest: build ## Mesure la passerelle sous charge (débit et latences, en direct et à travers Vigie)
	go run ./test/load -vigie bin/vigie

conformance: build ## Passe la suite de conformance MCP officielle à travers la passerelle
	test/conformance/run.sh

sonde-check: build ## Vérifie l'intégration Sonde (SONDE=chemin d'un clone de Sonde)
	go run ./examples/demo -traces bin/demo-traces.jsonl > /dev/null
	go run ./examples/demo -gateway -traces bin/demo-gateway-traces.jsonl > /dev/null
	python3 scripts/sonde_check.py --sonde $(SONDE) --traces bin/demo-traces.jsonl --traces bin/demo-gateway-traces.jsonl

dist: ## Construit les archives de release et leurs sommes de contrôle dans dist/
	@rm -rf dist && mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		name=vigie_$(VERSION:v%=%)_$${os}_$${arch}; \
		mkdir -p dist/$$name && \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$name/vigie$$ext ./cmd/vigie && \
		cp LICENSE NOTICE README.md dist/$$name/ && \
		if [ $$os = windows ]; then (cd dist && zip -qr $$name.zip $$name); \
		else tar -C dist -czf dist/$$name.tar.gz $$name; fi && \
		rm -rf dist/$$name && echo "  dist/$$name" || exit 1; \
	done
	@cd dist && sha256sum vigie_* > SHA256SUMS

clean: ## Supprime les binaires, archives et rapports
	rm -rf bin dist cover.out cover.html
