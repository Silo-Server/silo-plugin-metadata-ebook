# Contributing to the Ebook Metadata Plugin

The [Silo contribution guide](https://github.com/Silo-Server/.github/blob/main/CONTRIBUTING.md)
covers project-wide coordination, focused changes, evidence, AI disclosure, and
pull request expectations. Those requirements apply here; this guide adds the
plugin-specific workflow.

## Before you start

Open an [issue](https://github.com/Silo-Server/silo-plugin-metadata-ebook/issues)
before adding a source or changing source selection, identifier routing,
fallback order, configuration, or the advertised capability. This repository
owns ebook provider behavior; plugin contracts belong in
[`silo-plugin-sdk`](https://github.com/Silo-Server/silo-plugin-sdk), while host
metadata orchestration belongs in
[`silo-server`](https://github.com/Silo-Server/silo-server).

## Development setup

Use the Go version declared in `go.mod`. A local `go.work` may point at a sibling
SDK checkout while developing both repositories, but committed code and CI must
resolve the tagged SDK dependency with `GOWORK=off`. Never commit provider API
keys or a local filesystem `replace` directive.

## Validate your change

```sh
GOWORK=off go test ./...
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go mod tidy -diff
GOWORK=off go build ./...
gofmt -l .
```

`gofmt -l .` should print nothing. If it reports unrelated pre-existing drift,
none of the Go files touched by your change may appear in the output; do not add
to the output, and report what remains. Add focused coverage for source parsing,
ISBN and provider-ID routing, fallback order, rate limits, and failure isolation
when those behaviors change.

## Open the pull request

Use a Conventional Commit title, explain any matching or upstream-service risk,
and paste the actual validation results. Read the
[AI-assisted contribution policy](https://github.com/Silo-Server/silo-server/blob/main/docs/ai-contributions.md)
and include its disclosure block.
