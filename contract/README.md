# Shared contract snapshot

`registry.yaml` is a snapshot of `contract/registry.yaml` in
[`srcfl/ftw-webapp`](https://github.com/srcfl/ftw-webapp), which is where it is
edited. It names everything the app and the box both have to agree on: frozen
field ids, capabilities, scopes, error codes, source states and history
resolutions.

Both sides generate from it rather than typing the names out, because three
separate authorisation namespaces already grew in this codebase once. The Go
constants live in
[`go/internal/appproto/contract_gen.go`](../go/internal/appproto/contract_gen.go)
and are produced by:

```bash
go generate ./internal/appproto/...
```

`TestContractGenIsCurrent` fails when the checked-in file no longer matches the
YAML, so a snapshot update that skips the generator does not merge.

Changing a name means changing it upstream first, then copying the file here and
regenerating. Editing this copy on its own only creates the drift the registry
exists to prevent.

The `modes:` block is the exception in the other direction: those keys belong to
`control.AllModes()` in this repository and are copied *into* the registry so the
app can validate them. `TestRegistryModesMatchCatalog` holds the two together.
