#!/bin/sh
# Runs inside pmm-client after the node is registered (PMM_AGENT_PRERUN_FILE).
set -e

pmm-admin status --wait=30s

# The exporter is scraped by PMM Server directly over the compose network.
pmm-admin add external-serverless \
  --external-name=mariadb-dev \
  --host=exporter \
  --listen-port=9104 \
  --metrics-path=/metrics \
  --scheme=http \
  --group=mariadb \
  --environment=dev \
  --cluster=dev-cluster \
  || true

# PMM's native MySQL monitoring of the same instance, for side-by-side comparison.
pmm-admin add mysql \
  --username=root --password=root \
  --host=mariadb --port=3306 \
  --query-source=perfschema \
  --environment=dev --cluster=dev-cluster \
  mariadb-dev-mysql \
  || true

pmm-admin list
