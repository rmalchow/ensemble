# Releasing

Releases are **tag-driven**. Ordinary branch commits run the full pipeline
(UI build → tests → e2e → cross-compile) and leave 4-week dev artifacts on
the build jobs, but publish nothing.

To release:

```bash
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

That single push triggers a release pipeline which:

1. runs the full gate (vet, unit tests, gofmt, the 19-step loopback e2e);
2. builds `ensemble-linux-amd64` + `ensemble-linux-arm64` with
   `-X main.version=v0.2.0` (artifacts never expire);
3. generates release notes from the commits since the previous `v*` tag;
4. publishes a GitLab Release with asset links addressing the exact build
   jobs by id.

Version scheme: `vMAJOR.MINOR.PATCH`. The binary reports its version via
`ensemble --version` (dev builds report the commit sha).
