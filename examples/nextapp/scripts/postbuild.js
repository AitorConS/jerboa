// Completes the `output: "standalone"` build: Next.js doesn't copy
// .next/static or public/ into .next/standalone on its own.
const fs = require("fs");
const path = require("path");

const root = path.join(__dirname, "..");
const standalone = path.join(root, ".next", "standalone");

fs.cpSync(path.join(root, ".next", "static"), path.join(standalone, ".next", "static"), {
  recursive: true,
});
fs.cpSync(path.join(root, "public"), path.join(standalone, "public"), { recursive: true });
fs.rmSync(path.join(root, ".next", "cache"), { recursive: true, force: true });
