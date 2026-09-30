import io
import os
import platform
import subprocess
import sys
import tarfile
import urllib.request


__version__ = "0.10.2"
_BIN_DIR = os.path.join(os.path.dirname(__file__), "_bin")


def main():
    bin_path = _ensure_binary()
    result = subprocess.run([bin_path] + sys.argv[1:])
    sys.exit(result.returncode)


def _ensure_binary():
    """Download the binary on first run if not present."""
    name = "saferm"
    bin_path = os.path.join(_BIN_DIR, name)
    if os.path.exists(bin_path):
        return bin_path

    os.makedirs(_BIN_DIR, exist_ok=True)

    os_name = _detect_os()
    arch = _detect_arch()

    url = (
        f"https://github.com/stricttools/saferm/releases/download/v{__version__}/"
        f"saferm_{__version__}_{os_name}_{arch}.tar.gz"
    )

    print(f"Downloading saferm v{__version__} for {os_name}/{arch}...", file=sys.stderr)

    try:
        response = urllib.request.urlopen(url)
        data = response.read()
    except Exception as e:
        print(f"Failed to download saferm: {e}", file=sys.stderr)
        print(f"URL: {url}", file=sys.stderr)
        print(
            "Download manually from https://github.com/stricttools/saferm/releases",
            file=sys.stderr,
        )
        print(
            "Or install via Go: go install github.com/stricttools/saferm@v0",
            file=sys.stderr,
        )
        sys.exit(1)

    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tar:
        for member in tar.getmembers():
            if member.name == "saferm" or member.name.endswith("/saferm"):
                member.name = name
                tar.extract(member, _BIN_DIR)
                break

    os.chmod(bin_path, 0o755)
    return bin_path


def _detect_os():
    s = platform.system().lower()
    if s == "linux":
        return "linux"
    if s == "darwin":
        return "darwin"
    raise RuntimeError(
        f"Unsupported OS: {s}. "
        "saferm currently supports Linux and macOS only. "
        "Download manually from https://github.com/stricttools/saferm/releases"
    )


def _detect_arch():
    m = platform.machine().lower()
    if m in ("x86_64", "amd64"):
        return "amd64"
    if m in ("arm64", "aarch64"):
        return "arm64"
    raise RuntimeError(
        f"Unsupported architecture: {m}. "
        "Download manually from https://github.com/stricttools/saferm/releases"
    )
