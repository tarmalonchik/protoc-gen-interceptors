.PHONY: generate
generate:
	scripts/main.sh generate

.PHONY: lint
lint:
	golangci-lint run

.PHONY: install
install:
	scripts/main.sh dependencies