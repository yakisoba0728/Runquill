// Sample-panel fixtures for the Migrate screen: "GET /api/..." -> (query, body) => data.
// The sample panel refuses everything but GET, so the preview below stands in
// for a POST /api/migrate/preview response (same shape). It was produced by
// the Go rewriter (rewriteWorkflow, unifiedDiff) for the mapping
// ubuntu-latest -> default, ubuntu-24.04 -> build.
const ci = ".github/workflows/ci.yml";
const nightly = ".github/workflows/nightly.yml";
const release = ".github/workflows/release.yml";

const preview = {
  repository: "garden-lab/papertrail",
  base: "main",
  head: "4f3c2a91d0b7e6f5a4c3b2a1908f7e6d5c4b3a29",
  hash: "9b1e4c7a2f6d3e8b0a5c9d1f7e2b6a4c8d0e3f5a7b9c1d2e4f6a8b0c2d4e6f80",
  files: [
    {
      path: ci,
      changed: true,
      diff: "--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n@@ -4,7 +4,7 @@\n   pull_request:\n jobs:\n   test:\n-    runs-on: ubuntu-latest\n+    runs-on: ${{ format('runquill-{0}-{1}-test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}\n     strategy:\n       matrix:\n         go: [\"1.24\", \"1.25\"]\n@@ -12,7 +12,7 @@\n       - uses: actions/checkout@v4\n       - run: go test ./...\n   build:\n-    runs-on: ubuntu-24.04\n+    runs-on: ${{ format('runquill-{0}-{1}-pool__build__build', github.run_id, github.run_attempt) }}\n     steps:\n       - uses: actions/checkout@v4\n       - run: make\n",
      jobs: [
        { file: ci, job: "test", line: 7, labels: ["ubuntu-latest"], changed: true, code: "changed",
          runs_on: "${{ format('runquill-{0}-{1}-test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}" },
        { file: ci, job: "build", line: 15, labels: ["ubuntu-24.04"], changed: true, code: "changed",
          runs_on: "${{ format('runquill-{0}-{1}-pool__build__build', github.run_id, github.run_attempt) }}" },
        { file: ci, job: "docs", line: 20, labels: ["${{ inputs.os }}"], changed: false, code: "expression",
          reason: "runs-on is an expression" },
        { file: ci, job: "macos", line: 24, labels: ["macos-latest"], changed: false, code: "unmapped",
          reason: "no mapping matches these runs-on labels" },
      ],
    },
    { path: nightly, changed: false, refused: "the file indents with tabs", jobs: null },
    {
      path: release,
      changed: true,
      diff: "--- a/.github/workflows/release.yml\n+++ b/.github/workflows/release.yml\n@@ -4,8 +4,7 @@\n     tags: [\"v*\"]\n jobs:\n   publish:\n-    runs-on:\n-      - ubuntu-latest\n+    runs-on: ${{ format('runquill-{0}-{1}-publish', github.run_id, github.run_attempt) }}\n     steps:\n       - run: ./release.sh\n   deploy:\n",
      jobs: [
        { file: release, job: "publish", line: 7, labels: ["ubuntu-latest"], changed: true, code: "changed",
          runs_on: "${{ format('runquill-{0}-{1}-publish', github.run_id, github.run_attempt) }}" },
        { file: release, job: "deploy", line: 11, changed: false, code: "reusable-call",
          reason: "the job calls a reusable workflow" },
        { file: release, job: "sign", line: 14, labels: ["self-hosted", "signing"], changed: false, code: "self-hosted",
          reason: "the job already targets self-hosted runners" },
      ],
    },
  ],
};

export default {
  "GET /api/migrate/status": () => ({ ready: true, required: ["contents", "pull_requests", "workflows"], missing: [] }),
  // Not a server route: the sample preview, read with GET because the
  // sample panel answers nothing else.
  "GET /api/migrate/preview": () => preview,
};
