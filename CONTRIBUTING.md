# Contributing

Start with the [README](README.md) and [docs/CLI.md](docs/CLI.md).

Rules:

- Every model follows its panel: general features work for all models, and
  model-specific ones appear when that model's panel declares them.
- No firmware/OTA writing.
- Tests never touch hardware or your HOME; use the fake transport.
- Generated files (`internal/protocol/table.json`, `models.json`) are not edited
  by hand; change the inputs and regenerate.

Before opening a pull request: `gofmt -l .` prints nothing, `go vet ./...` and
`go test -count=1 ./...` pass, and `CHANGELOG.md` is updated for user-visible changes.
