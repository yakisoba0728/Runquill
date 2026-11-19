// Sample responses for the Backups view: the same shape as GET /api/backups/status.
function status() {
  const at = (seconds) => new Date(Date.now() - seconds * 1000).toISOString();
  return {
    scheduled: true,
    interval_seconds: 86400,
    directory: "/var/lib/runquill/backups",
    keep: 7,
    sealing: true,
    remote: { configured: true, bucket: "garden-lab-backups", endpoint_host: "s3.example.com", region: "us-east-1", keep: 30 },
    last: {
      last_attempt: at(5400), last_success: at(5400), name: "runquill-queue-20261119T030000Z.sqlite3.sealed", size: 2883584, sealed: true,
      remote: "uploaded", remote_at: at(5390),
    },
    verification: { name: "runquill-queue-20261119T030000Z.sqlite3.sealed", checked_at: at(1800), ok: true },
  };
}

export default { "GET /api/backups/status": status };
