#!/bin/sh
# Seeds DynamoDB Local once (see dev/compose.yaml): Customers (a partition
# key, nested maps, string sets, a global index), Orders (partition and
# sort key, lists of maps, a local index) and Sessions (binary, TTL).
set -e
ddb() { aws dynamodb --endpoint-url "$URL" --region us-east-1 --output text "$@"; }
until ddb list-tables >/dev/null 2>&1; do sleep 1; done
if ! ddb describe-table --table-name Customers >/dev/null 2>&1; then
  ddb create-table --table-name Customers --billing-mode PAY_PER_REQUEST \
    --attribute-definitions AttributeName=customer_id,AttributeType=S AttributeName=tier,AttributeType=S AttributeName=signed_up,AttributeType=S \
    --key-schema AttributeName=customer_id,KeyType=HASH \
    --global-secondary-indexes 'IndexName=ByTier,KeySchema=[{AttributeName=tier,KeyType=HASH},{AttributeName=signed_up,KeyType=RANGE}],Projection={ProjectionType=ALL}' >/dev/null
  ddb create-table --table-name Orders --billing-mode PAY_PER_REQUEST \
    --attribute-definitions AttributeName=customer_id,AttributeType=S AttributeName=order_id,AttributeType=S AttributeName=total,AttributeType=N \
    --key-schema AttributeName=customer_id,KeyType=HASH AttributeName=order_id,KeyType=RANGE \
    --local-secondary-indexes 'IndexName=ByTotal,KeySchema=[{AttributeName=customer_id,KeyType=HASH},{AttributeName=total,KeyType=RANGE}],Projection={ProjectionType=KEYS_ONLY}' >/dev/null
  ddb create-table --table-name Sessions --billing-mode PROVISIONED --provisioned-throughput ReadCapacityUnits=5,WriteCapacityUnits=5 \
    --attribute-definitions AttributeName=session_id,AttributeType=S --key-schema AttributeName=session_id,KeyType=HASH >/dev/null
  ddb update-time-to-live --table-name Sessions --time-to-live-specification Enabled=true,AttributeName=expires_at >/dev/null
  for f in /seed/dynamodb/*.json; do ddb batch-write-item --request-items "file://$f" >/dev/null; done
fi
touch /tmp/ready
exec sleep infinity
