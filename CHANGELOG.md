# Changelog

All notable changes to this project are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.3.0] - 2026-08-12

Initial public release.

### Added
- Colorized `kubectl get --watch`-compatible output for any resource, with the same
  flags as kubectl (`-n`, `-l`, `-A`, `-o`, `--context`, etc.).
- Automatic crash diagnosis: when a pod enters an error state and stays there past a
  grace period, kwatch fetches and prints its logs and events inline.
- Deduplication of diagnosis blocks by status family (`Error`/`CrashLoopBackOff`/`OOMKilled`
  all count as one "crash" family per error cycle).
- Detection of silent restarts (restart count increases without a visible error status)
  and filtering of log lines from garbage-collected previous containers.
- Direct Kubernetes API access (watch + discovery + dynamic client) instead of shelling
  out to `kubectl`, with automatic reconnect/backoff and resource-version-expiry (410
  Gone) handling.
- `--timeout` flag to exit after a fixed duration.
- Shell completion for zsh/bash/fish/PowerShell, including live namespace and context
  completion.

[Unreleased]: https://github.com/svclimb/kwatch/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/svclimb/kwatch/releases/tag/v0.3.0
