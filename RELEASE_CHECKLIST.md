# Release Checklist

Use this before broadly opening or promoting the repository.

## Repository Basics

- [ ] Root `README.md` is current
- [ ] `KNOWN_LIMITATIONS.md` is current
- [ ] `CONTRIBUTING.md` is current
- [ ] `SECURITY.md` exists and is current
- [ ] top-level license and notice files are correct

## Messaging

- [ ] project is framed as public beta / experimental if that is still the honest state
- [ ] no public copy implies parity with Docker or general-purpose container runtimes
- [ ] Windows support is described as WSL2-based

## Documentation

- [ ] root docs and `docs/` describe the same platform model
- [ ] current networking limits are documented
- [ ] current Windows/WSL behavior is documented
- [ ] current daemon/CLI command surface is documented

## Validation

- [ ] `make build`
- [ ] `make test`
- [ ] `make lint`
- [ ] integration path verified on Linux with KVM
- [ ] one Windows WSL2 path verified end to end
- [ ] one Firecracker path verified end to end

## Public Presentation

- [ ] remove or ignore stray local artifacts before publishing screenshots or examples
- [ ] examples used in docs actually run
- [ ] release notes say what is stable enough and what is still rough

## Strong Recommendation

Do not do broad public promotion until licensing and attribution are correct.
