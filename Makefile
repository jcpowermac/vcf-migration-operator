.PHONY: help build-index

help:  ## Show this help
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS=":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build-index: ## Run build-catalog.sh with SNAPSHOT=<snapshot-or-url>
	@if [ -z "$(SNAPSHOT)" ]; then \
		echo "Error: SNAPSHOT variable not set. Use: make build-index SNAPSHOT=<snapshot-or-url>"; \
		exit 1; \
	fi
	./hack/build-catalog.sh $(SNAPSHOT)
