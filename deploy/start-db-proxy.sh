#!/bin/sh
# Local development connection only. Sign in using `gcloud auth login` first.
# Keep this listener on loopback; the proxy encrypts the remote Cloud SQL hop.
set -eu
exec cloud-sql-proxy --gcloud-auth --address 127.0.0.1 --port 15432 \
  dulie-assistant:asia-southeast1:dulie-db
