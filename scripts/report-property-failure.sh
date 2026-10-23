#!/usr/bin/env bash
# Issue reporting is intentionally implemented only in the isolated workflow job.
# Keep this path as a clear guard against accidentally restoring a second API client.
set -euo pipefail
echo 'property failure reporting is available only through the trusted workflow reporter job' >&2
exit 2
