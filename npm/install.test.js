const assert = require("assert");

// Platform mapping (must match install.js)
const PLATFORM_MAP = { linux: "linux", darwin: "darwin" };
const ARCH_MAP = { x64: "amd64", arm64: "arm64" };

function getDownloadUrl(version, platform, arch) {
  const os = PLATFORM_MAP[platform];
  const goarch = ARCH_MAP[arch];
  if (!os) throw new Error(`Unsupported platform: ${platform}`);
  if (!goarch) throw new Error(`Unsupported architecture: ${arch}`);
  return `https://github.com/stricttools/saferm/releases/download/v${version}/saferm_${version}_${os}_${goarch}.tar.gz`;
}

// Test platform mapping
assert.strictEqual(getDownloadUrl("0.1.1", "linux", "x64"),
  "https://github.com/stricttools/saferm/releases/download/v0.1.1/saferm_0.1.1_linux_amd64.tar.gz");
assert.strictEqual(getDownloadUrl("0.1.1", "darwin", "arm64"),
  "https://github.com/stricttools/saferm/releases/download/v0.1.1/saferm_0.1.1_darwin_arm64.tar.gz");
assert.strictEqual(getDownloadUrl("0.1.1", "linux", "arm64"),
  "https://github.com/stricttools/saferm/releases/download/v0.1.1/saferm_0.1.1_linux_arm64.tar.gz");
assert.strictEqual(getDownloadUrl("0.1.1", "darwin", "x64"),
  "https://github.com/stricttools/saferm/releases/download/v0.1.1/saferm_0.1.1_darwin_amd64.tar.gz");

// Test unsupported platforms
assert.throws(() => getDownloadUrl("0.1.1", "win32", "x64"), /Unsupported platform/);
assert.throws(() => getDownloadUrl("0.1.1", "freebsd", "x64"), /Unsupported platform/);

// Test unsupported arch
assert.throws(() => getDownloadUrl("0.1.1", "linux", "ia32"), /Unsupported architecture/);

// All URLs use tar.gz (no Windows support)
assert.ok(getDownloadUrl("0.1.1", "linux", "x64").endsWith(".tar.gz"));
assert.ok(getDownloadUrl("0.1.1", "darwin", "arm64").endsWith(".tar.gz"));

console.log("All npm wrapper tests passed");
