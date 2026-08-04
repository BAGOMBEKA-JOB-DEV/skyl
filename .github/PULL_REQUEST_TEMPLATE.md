## What this changes

<!-- What is different afterwards, and why. If it fixes something, describe the
     failure rather than the fix: the failure is what a reviewer checks against. -->

## How it was verified

<!-- Which commands you ran, and what they said. `docs/rules.md` §3.5 asks for
     -race; coverage floors are per module. If you added a test, say what it
     would catch that nothing caught before. -->

```
```

## Checklist

- [ ] `gofmt`, `go vet` (including `-tags=integration` and `-tags=sandbox`), and
      `golangci-lint` are clean in every module I touched
- [ ] Tests pass under `-race` with `-count=1`
- [ ] Coverage did not fall; if it rose, the floor in `ci.yml` rose with it
- [ ] Exported symbols have doc comments that say something (`rules.md` §1.1)
- [ ] Errors are classified onto a sentinel, not stringified (§2.2)
- [ ] Nothing silently drops caller data (§6.1)
- [ ] No credential can reach an error, a log, or a test fixture (§7.2)
- [ ] Docs updated in this PR, not a later one (§8.1)
- [ ] Anything structural has an ADR in `docs/adr/`
- [ ] Breaking change? It is in `CHANGELOG.md` with a migration note (§1.2)
