# Contributing

Thanks for helping. Small, focused pull requests merge fastest.

## Before you open a PR

```sh
go build ./...
go test -race ./...
golangci-lint run          # v2, same version as CI
shellcheck --severity=warning bin/*
```

CI runs the same checks on Linux and macOS.

## Guidelines

- The relay (`serve`, `routes`, `check`, `models`, `status`) stays standard
  library only. New dependencies belong to `submux pick` or need a strong case.
- Never add a silent model substitution. Fallbacks stay explicit in config.
- Never log tokens, `Authorization` headers or request bodies that may carry them.
- Add or update a test next to the file you change.
- For a larger change, open an issue or discussion first so we can agree on the shape.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Please do not file them as public issues.
