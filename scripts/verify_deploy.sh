#!/bin/bash
set -e

# API URL
API="http://localhost:8080"
API_TOKEN="${STRANGER_API_TOKEN:-dev-api-token}"

AUTH_HEADER="Authorization: Bearer $API_TOKEN"

# 1. Create Project
echo "Creating Project..."
REPO_PATH=$(pwd)/tests/fixtures/simple-app
PROJECT_RESP=$(curl -s -X POST "$API/projects" \
    -H "$AUTH_HEADER" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"test-project\",\"repo_url\":\"$REPO_PATH\",\"template\":\"nextjs\"}")
PROJECT_ID=$(echo $PROJECT_RESP | jq -r .id)
echo "Project ID: $PROJECT_ID"

if [ "$PROJECT_ID" == "null" ]; then
    echo "Failed to create project"
    exit 1
fi

# 2. Deploy
echo "Saving encrypted project secret..."
SECRET_RESP=$(curl -s -X PUT "$API/projects/$PROJECT_ID/secrets/APP_ENV" \
    -H "$AUTH_HEADER" \
    -H "Content-Type: application/json" \
    -d '{"value":"production"}')
SECRET_KEY=$(echo "$SECRET_RESP" | jq -r .key)
if [ "$SECRET_KEY" != "APP_ENV" ]; then
    echo "Failed to create secret"
    echo "$SECRET_RESP"
    exit 1
fi

echo "Listing project secrets..."
SECRETS_RESP=$(curl -s -X GET "$API/projects/$PROJECT_ID/secrets" -H "$AUTH_HEADER")
SECRET_COUNT=$(echo "$SECRETS_RESP" | jq 'length')
if [ "$SECRET_COUNT" -lt 1 ]; then
    echo "Expected project secrets but found none"
    exit 1
fi

echo "Running dry-run secret rotation..."
ROTATE_RESP=$(curl -s -X POST "$API/admin/secrets/rotate" \
    -H "$AUTH_HEADER" \
    -H "Content-Type: application/json" \
    -d "{\"project_id\":\"$PROJECT_ID\",\"dry_run\":true}")
ROTATE_FAILED=$(echo "$ROTATE_RESP" | jq -r .failed)
if [ "$ROTATE_FAILED" != "0" ]; then
    echo "Secret rotation dry-run reported failures"
    echo "$ROTATE_RESP"
    exit 1
fi

# 3. Deploy
echo "Deploying..."
DEPLOY_RESP=$(curl -s -X POST "$API/deploy" \
    -H "$AUTH_HEADER" \
    -H "Content-Type: application/json" \
    -d "{\"project_id\":\"$PROJECT_ID\"}")
JOB_ID=$(echo $DEPLOY_RESP | jq -r .job_id)
echo "Job ID: $JOB_ID"

if [ "$JOB_ID" == "null" ]; then
    echo "Failed to deploy"
    exit 1
fi

# 4. Wait for deploy completion
echo "Waiting for deploy queue..."
for i in {1..60}; do
    JOB_RESP=$(curl -s -X GET "$API/deploy/jobs/$JOB_ID" -H "$AUTH_HEADER")
    STATUS=$(echo "$JOB_RESP" | jq -r .status)
    ERROR=$(echo "$JOB_RESP" | jq -r .error)
    echo "Attempt $i - status: $STATUS"

    if [ "$STATUS" == "succeeded" ]; then
        break
    fi
    if [ "$STATUS" == "failed" ]; then
        echo "Deploy job failed: $ERROR"
        exit 1
    fi
    sleep 1
done

# 5. Stream Logs (Tail for 10 seconds)
echo "Streaming Logs for 10 seconds..."
curl -N -H "$AUTH_HEADER" "$API/logs?project_id=$PROJECT_ID" &
PID=$!

sleep 10
kill $PID
echo "Done."
