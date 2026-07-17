# Contributing

## Version policy

- Language and standard-library examples target the Go version in `go.mod`.
- Runtime chapters must name the exact Go tag they describe.
- Kubernetes chapters must keep all `k8s.io/*` modules on one minor version and follow controller-runtime's `go.mod`.
- Historical behavior must be labeled with an explicit version boundary.

## Checks

Run before submitting changes:

```bash
gofmt -w cmd examples
go vet ./...
go test ./...
go test -race ./...
go run ./cmd/bookcheck .
mdbook build
```

Code that is intentionally incomplete must be labeled as pseudocode in the surrounding text. New factual claims about Runtime internals should link to an official source tag or release note.

