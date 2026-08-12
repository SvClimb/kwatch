# Contributing

Thanks for considering a contribution to kwatch.

## Development

```bash
make build          # build to bin/kwatch
make test           # run tests with the race detector
make lint           # go vet + gofmt check
make lint-examples  # validate examples/*.yaml against Kubernetes schemas (requires kubeconform)
```

Tests don't require a live cluster — `Diagnoser` takes overridable `logsFunc`/`eventsFunc`
hooks (see `newTestDiagnoser` in `diagnose_test.go`) so crash-diagnosis logic can be
tested without hitting a real Kubernetes API.

Before opening a PR, make sure `make lint` and `make test` both pass, and `make
lint-examples` too if you touched `examples/`. CI runs the same checks on every push and
pull request.

## Code style

- Standard `gofmt` formatting, no exceptions.
- Comments explain *why*, not *what* — skip a comment if the code is already clear from
  naming.
- Prefer extending existing tests over adding a parallel test file for the same behavior.

## Reporting bugs / requesting features

Open a GitHub issue with the relevant template. For bugs, include your kwatch version
(`kwatch --version`), the exact command, and what you expected vs. what happened.

## Pull requests

- Keep PRs focused — one logical change per PR is easier to review than a bundle.
- Add or update tests for behavior changes.
- Update `README.md` if you change flags, defaults, or user-facing behavior.
