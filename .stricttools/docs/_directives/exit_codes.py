"""selfdoc custom directive: render exit codes from exitcodes.go.

Registered in selfdoc.json as ``"table-exit-codes"``. selfdoc loads this
file and calls ``resolve(attrs, config, body) -> str``; the returned
markdown replaces the ``:-: table-exit-codes`` directive line.

The source of truth is the const block in ``exitcodes.go`` at the repo
root. This directive parses that file directly so the generated table
can never drift from the code.
"""

from __future__ import annotations

import os
import re


def _parse_exit_codes(go_source: str) -> list[tuple[str, int]]:
    """Extract (name, value) pairs from the const block in *go_source*."""
    # Match lines like:  ExitSuccess      = 0
    pattern = re.compile(r"^\s*(Exit\w+)\s*=\s*(\d+)", re.MULTILINE)
    return [(m.group(1), int(m.group(2))) for m in pattern.finditer(go_source)]


def resolve(attrs, config, body):
    """Return a markdown table of exit codes parsed from exitcodes.go."""
    repo_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    exitcodes_path = os.path.join(repo_root, "exitcodes.go")

    with open(exitcodes_path) as f:
        source = f.read()

    codes = _parse_exit_codes(source)
    if not codes:
        raise RuntimeError(f"no exit codes found in {exitcodes_path}")

    lines = [
        "| Code | Name | Value |",
        "| --- | --- | --- |",
    ]
    for name, value in sorted(codes, key=lambda c: c[1]):
        lines.append(f"| {value} | `{name}` | {value} |")

    return "\n".join(lines)
