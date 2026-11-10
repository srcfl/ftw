# FTW shared contracts

Core owns `registry.yaml` and `push-catalogue.yaml`. These files and their
future changes use Apache-2.0, as LICENSE states. They remain public when
the official clients are private.

The registry fixes field ids, capabilities, scopes, errors, source states
and history resolutions. The catalogue holds notification templates.
Edit these files here, then copy them byte for byte into
`ftw-webapp/contract/` and `ftw-app/protocol/` in paired PRs. Copy LICENSE too.
The clients compare their snapshots against a public Core checkout. Core
never needs a private client checkout to build or test.

From `go/`, run `go generate ./internal/...` and the contract tests:

```bash
go test ./internal/appproto ./internal/apiauth ./internal/notifications
```

The generator tests reject stale constants and templates. The registry's
`modes:` must match `control.AllModes()`. Public wire types live in
`go/protocol/`; the Cloud protocol is in `docs/protocol-cloud.md`.

Earlier client releases and their original licenses remain public in
[the webapp archive](https://github.com/srcfl/ftw-webapp-source-archive) and
[the native archive](https://github.com/srcfl/ftw-app-source-archive).
