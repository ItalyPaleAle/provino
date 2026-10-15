# Coding Style Guidelines

## Go

Building and testing this project requires Go 1.27+.

Generating variants requires libvips through cgo, so `CGO_ENABLED=1` and the libvips development headers are needed to build and test. The `Makefile` sets the environment variable; install the headers with `apt-get install libvips-dev` (and `libheif-plugin-aomenc`, which AVIF output needs), or `brew install vips` on macOS.

Unit tests must be run with the `unit` build tag (`go test -tags unit ./...` or `make test`). The durable workflow tests in `internal/actors` skip themselves under the race detector, so run `make test-workflows` as well when changing the workflow.

If you modify any HTTP handlers, request/response types, or other API surface, run `make gen-openapi` to regenerate the spec in `api/openapi` before finishing the task.

If you modify `internal/config.Config` or any struct referenced from it, always run `make gen-config` before finishing the task.

The Helm chart in `helm` sets some configuration options itself and checks others (`server`, `actors`, `storage`, and `auth`). If you change any of those options, update the chart to match and run `make lint-chart`, which needs `helm`.

Never define variables inside `if` conditions. Always declare variables on a separate line before the conditional check.

```go
// Wrong
if err := something(); err != nil { ... }

// Wrong
if val, ok := something.(string); ok { ... }

// Right
err := something()
if err != nil { ... }

// Right
val, ok := something.(string)
if ok { ... }
```

In tests, never invoke functions inside assertions that check errors, such as `require.Error`, `require.NoError`, `require.ErrorIs`, `assert.ErrorContains`, and similar.
Always assign the result to an `err` variable on a separate line first, then pass the variable to the assertion.
This does not apply to assertions that do not check errors.

```go
// Wrong
require.Error(t, something())

// Wrong
require.ErrorIs(t, something(), context.Canceled)

// Wrong
require.NoError(t, something())

// Right
err := something()
require.Error(t, err)

// Right
err := something()
require.ErrorIs(t, err, context.Canceled)

// Right
err := something()
require.NoError(t, err)
```

## Comments

- One sentence per line: do NOT wrap to a max line length
- No trailing period on single-line comments
- Prefer comments that explain intent, invariants, or why a branch exists
- Avoid comments that restate the next line of code
- For multi-step logic, use short section comments to separate the steps and explain why each step exists
- When adding or changing functions, add section comments before each major step so the method reads as a sequence of decisions
- Section comments should make control-flow boundaries visually obvious, especially around validation, external calls, cache lookups, persistence, and error classification
- Comments should briefly emphasize why the step exists or what invariant it protects

```go
// Wrong: wrapped mid-sentence
// This function performs the main validation logic. It checks
// the input against the schema and returns an error if the
// input is invalid.

// Wrong: trailing period on single-line comment
// Validate the input.

// Right
// This function performs the main validation logic
// It checks the input against the schema and returns an error if the input is invalid

// Right
// Validate the input

// Right
// Normalize the request host so callers can pass either Host or X-Forwarded-Host values

// Right
// Read the previous manifest first so variants that still match are reused instead of encoded again
previous, found, err := w.previousManifest(ctx, namespace, imageID)

// Wrong: restates the code
// Trim whitespace and lowercase the host
host = strings.TrimSpace(strings.ToLower(host))

// Wrong: names the operation without explaining the branch or invariant
// Get the manifest
previous, found, err := w.previousManifest(ctx, namespace, imageID)
```

## Workflows

Variant generation uses Francis' built-in workflow engine. Workflow tasks are delivered at least once, so any change to `internal/actors` has to keep each step idempotent: a step that runs twice must leave the same result behind.

[`internal/actors/README.md`](./internal/actors/README.md) documents the definition, durable data flow, and idempotency and failure rules. Update it along with any change to the workflow.

## Markdown

Do not hard-wrap Markdown prose or limit it to a maximum line length.

## Running the Go linter

The Go linter is pinned to a specific version (included in `.golangci-lint-version`), which must be installed from the pre-compiled binary: builds from package managers are compiled with an older Go and refuse this module's Go version.

```sh
VERSION="2.x.x" # From .golangci-lint-version
curl -sSL https://github.com/golangci/golangci-lint/releases/download/v${VERSION}/golangci-lint-${VERSION}-linux-amd64.tar.gz | tar -xz -C /tmp
install /tmp/golangci-lint-${VERSION}-linux-amd64/golangci-lint /usr/local/bin
make lint
```

## Git

Do not stage or unstage changes unless the user explicitly asks you to.
