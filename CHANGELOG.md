# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.1.0] - 2026-07-26


### Added

- Prove of concept
- Make cligate cli-framework agnostic ([#1](https://github.com/go-devkit/clink/pull/1))
- Remove direct exposure of ssh types ([#3](https://github.com/go-devkit/clink/pull/3))
- Remove need for password ([#8](https://github.com/go-devkit/clink/pull/8))
- Upgrade charm deps to v2 ([#9](https://github.com/go-devkit/clink/pull/9))
- Shell-based tui handoff & per-session ctx ([#11](https://github.com/go-devkit/clink/pull/11))
- Unified interactive tui path ([#12](https://github.com/go-devkit/clink/pull/12))
- Local-command dispatch, file forwarding, AutoPTY ([#13](https://github.com/go-devkit/clink/pull/13))
- Forward client signals and window changes to the daemon ([#15](https://github.com/go-devkit/clink/pull/15))
- Confirm file transfers end-to-end, exit 127 on unhandled ([#21](https://github.com/go-devkit/clink/pull/21))
- Bound daemon shutdown with a configurable grace period ([#23](https://github.com/go-devkit/clink/pull/23))

### Changed

- Drop dead -- separator stripping ([#17](https://github.com/go-devkit/clink/pull/17))

### Fixed

- Avoid filesystem host key & graceful shutdown ([#2](https://github.com/go-devkit/clink/pull/2))
- Guard SIGWINCH forwarding behind a unix build tag ([#19](https://github.com/go-devkit/clink/pull/19))
- Propagate command exit status end-to-end ([#24](https://github.com/go-devkit/clink/pull/24))

### Documentation

- Handler concurrency, security model, and version assumptions ([#18](https://github.com/go-devkit/clink/pull/18))
- Package-level documentation header ([#20](https://github.com/go-devkit/clink/pull/20))

