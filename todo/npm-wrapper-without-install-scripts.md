# The npm wrapper without install scripts

## Context

The npm package `saferemove` installs the Go binary through a `postinstall`
script (`npm/install.js`) that downloads the release asset for the user's
platform. npm 12 turns lifecycle scripts off by default (`allowScripts`), so
a plain install leaves a package whose `saferm` command is missing.

The npm publish of this package has also been failing through continuous
integration with a 404 on upload, most likely because of the package scope
of the npm token stored in this repository's secrets.

## What to do

- Ship the binary without an install script, the way esbuild does: one small
  package per platform carrying its binary, listed as optional dependencies
  of `saferemove`, so npm installs the matching one with no script. The
  platform packages' names need the owner's approval before anything is
  published.
- Keep the PyPI wrapper and the Go install path working as they do.
- Test that a fresh install with scripts disabled provides a working
  `saferm`.

## Open decisions

- Whether the npm and PyPI wrappers of Go tools keep publishing at all during
  the current Go-only refinement.
- How npm publishing authenticates: a fresh token with every package in
  scope, or npm trusted publishing with no stored token.

## Effort

Medium: new per-platform packages, wrapper changes, and publish workflow
changes.
