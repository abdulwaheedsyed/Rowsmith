#!/bin/sh
# Seeds Elasticsearch or OpenSearch once (see dev/compose.yaml): a products
# index with a catalog alias, and an events-app data stream.
#   URL, AUTH (user:password), CURL_OPTS (-k for a self-signed certificate)
set -e
c() { curl -sS -f $CURL_OPTS -u "$AUTH" "$@"; }
json() { c -H 'Content-Type: application/json' "$@"; }
nd() { c -H 'Content-Type: application/x-ndjson' "$@"; }
until c "$URL/_cluster/health?wait_for_status=yellow&timeout=5s" >/dev/null 2>&1; do sleep 3; done
if ! c -I "$URL/products" >/dev/null 2>&1; then
  json -X PUT "$URL/products" --data-binary @/seed/elastic-products.json >/dev/null
  nd -X POST "$URL/_bulk?refresh=true" --data-binary @/seed/elastic-products.ndjson >/dev/null
  json -X PUT "$URL/_index_template/events" -d '{"index_patterns":["events-*"],"data_stream":{},"priority":500,
    "template":{"settings":{"number_of_replicas":0},"mappings":{"properties":{"@timestamp":{"type":"date"},"level":{"type":"keyword"},
    "service":{"type":"keyword"},"message":{"type":"text"},"latency_ms":{"type":"integer"}}}}}' >/dev/null
  json -X PUT "$URL/_data_stream/events-app" >/dev/null
  nd -X POST "$URL/events-app/_bulk?refresh=true" --data-binary @/seed/elastic-events.ndjson >/dev/null
  json -X POST "$URL/_aliases" -d '{"actions":[{"add":{"index":"products","alias":"catalog"}}]}' >/dev/null
fi
touch /tmp/ready
exec sleep infinity
