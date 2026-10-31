# provino uses libvips through cgo
export CGO_ENABLED := 1

.PHONY: test
test:
	go test -tags unit ./...

.PHONY: test-race
test-race:
	go test -race -tags unit ./...

# The durable workflow tests skip themselves when the race detector is on because their Francis peer-server lifecycle is exercised in a separate non-race run
# They are the only end-to-end coverage of the workflow, so they get a run of their own without it
.PHONY: test-workflows
test-workflows:
	go test -tags unit ./internal/actors/...

# Run the example app and provino together, as listed in example/Procfile, until Ctrl+C stops both
.PHONY: example
example:
# goreman's RPC server, which restarts one server at a time, listens on loopback only rather than on every interface
	pnpm --dir example/web install --frozen-lockfile
	GOREMAN_RPC_ADDR=127.0.0.1 go tool goreman -f example/Procfile -set-ports=false -logtime=false start

# Delete everything the example stored, so it starts over
.PHONY: example-reset
example-reset:
	@if curl -s -o /dev/null http://localhost:3001/ || curl -s -o /dev/null http://127.0.0.1:3000/; then \
		echo "The example is still running: stop it first"; \
		exit 1; \
	fi
	rm -rf example/data

.PHONY: lint
lint:
	golangci-lint run -c .golangci.yaml

# Every scenario in helm/ci is linted and rendered to reach more errors
.PHONY: lint-chart
lint-chart:
	@for values in helm/ci/*-values.yaml; do \
		echo "Checking the chart with $$values"; \
		helm lint helm --strict --quiet --values "$$values" && \
		helm template provino helm --values "$$values" >/dev/null || exit 1; \
	done

.PHONY: gen-config
gen-config:
	go tool gen-config \
		-struct internal/config/config.go \
		-yaml config.sample.yaml \
		-md config.md

# The service embeds only the minified JSON form of its document
# openapi-convert always writes both representations, so it emits into the temporary directory and jq compacts the JSON into the tree while the YAML is discarded
.PHONY: gen-openapi
gen-openapi:
	OPENAPI_TMPDIR="$$(mktemp -d)"; \
	trap 'rm -rf "$$OPENAPI_TMPDIR"' EXIT; \
	go tool swag init --quiet --dir . --generalInfo cmd/provino/main.go --output "$$OPENAPI_TMPDIR" --outputTypes json --parseDependency --parseInternal && \
	go tool openapi-convert -in "$$OPENAPI_TMPDIR/swagger.json" \
		-path-prefix /v1/ \
		-title "provino API" \
		-description "HTTP API for uploading images and generating their variants in durable workflows" \
		-json "$$OPENAPI_TMPDIR/provino.json" -yaml "$$OPENAPI_TMPDIR/provino.yaml" && \
	jq -c . "$$OPENAPI_TMPDIR/provino.json" > api/openapi/provino.json

# Generated configuration documents must stay synchronized with the config struct
.PHONY: check-config-diff
check-config-diff: gen-config
	git diff --exit-code config.sample.yaml config.md

# The generated API document must stay synchronized with the handlers
.PHONY: check-openapi-diff
check-openapi-diff: gen-openapi
	git diff --exit-code api/openapi/provino.json
