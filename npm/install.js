const https = require("https");
const fs = require("fs");
const path = require("path");
const { execSync } = require("child_process");

const pkg = require("./package.json");
const version = pkg.version;

const PLATFORM_MAP = {
  linux: "linux",
  darwin: "darwin",
};

const ARCH_MAP = {
  x64: "amd64",
  arm64: "arm64",
};

function main() {
  const osPlatform = process.platform;
  const osArch = process.arch;

  const os = PLATFORM_MAP[osPlatform];
  const arch = ARCH_MAP[osArch];

  if (!os || !arch) {
    const label = !os && !arch
      ? `platform ${osPlatform} and architecture ${osArch}`
      : !os
        ? `platform ${osPlatform}`
        : `architecture ${osArch}`;
    console.error(`Unsupported ${label}.`);
    console.error(`Download manually from https://github.com/stricttools/saferm/releases`);
    console.error(`Or install via Go: go install github.com/stricttools/saferm@v0`);
    process.exit(1);
  }

  const url = `https://github.com/stricttools/saferm/releases/download/v${version}/saferm_${version}_${os}_${arch}.tar.gz`;

  const binName = "saferm";
  const destPath = path.join(__dirname, "bin", binName);

  console.log(`Downloading saferm v${version} for ${os}/${arch}...`);

  download(url, (err, data) => {
    if (err) {
      console.error(`Failed to download saferm: ${err.message}`);
      console.error(`URL: ${url}`);
      console.error(`Download manually from https://github.com/stricttools/saferm/releases`);
      console.error(`Or install via Go: go install github.com/stricttools/saferm@v0`);
      process.exit(1);
    }

    extractTarGz(data, binName, destPath);
    fs.chmodSync(destPath, 0o755);
    console.log(`saferm v${version} installed successfully.`);
  });
}

function download(url, callback, redirects) {
  if (redirects === undefined) redirects = 0;
  if (redirects > 5) {
    callback(new Error("Too many redirects"));
    return;
  }

  const mod = url.startsWith("https") ? https : require("http");

  mod.get(url, (res) => {
    if (res.statusCode === 301 || res.statusCode === 302) {
      download(res.headers.location, callback, redirects + 1);
      return;
    }

    if (res.statusCode !== 200) {
      callback(new Error(`HTTP ${res.statusCode}: ${url}`));
      return;
    }

    const chunks = [];
    res.on("data", (chunk) => chunks.push(chunk));
    res.on("end", () => callback(null, Buffer.concat(chunks)));
    res.on("error", callback);
  }).on("error", callback);
}

function extractTarGz(data, binName, destPath) {
  const tmpArchive = path.join(__dirname, "_tmp_archive.tar.gz");
  const tmpDir = path.join(__dirname, "_tmp_extract");

  try {
    fs.writeFileSync(tmpArchive, data);
    fs.mkdirSync(tmpDir, { recursive: true });

    execSync(`tar xzf "${tmpArchive}" -C "${tmpDir}"`, { stdio: "pipe" });

    const extracted = findFile(tmpDir, binName);
    if (!extracted) {
      throw new Error(`Binary "${binName}" not found in archive`);
    }

    fs.copyFileSync(extracted, destPath);
  } finally {
    try { fs.unlinkSync(tmpArchive); } catch (_) {}
    try { fs.rmSync(tmpDir, { recursive: true }); } catch (_) {}
  }
}

function findFile(dir, name) {
  const entries = fs.readdirSync(dir, { withFileTypes: true });
  for (const entry of entries) {
    const fullPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      const found = findFile(fullPath, name);
      if (found) return found;
    } else if (entry.name === name) {
      return fullPath;
    }
  }
  return null;
}

main();
