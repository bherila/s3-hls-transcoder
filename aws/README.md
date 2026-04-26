# aws — AWS Lambda entrypoint

Lambda container image. Triggered by an EventBridge cron rule.

See **[../PLAN.md](../PLAN.md)** for architecture and **[../CLAUDE.md](../CLAUDE.md)** for conventions.

## Configuration

Set the env vars listed in [`local/.env.sample`](../local/.env.sample) on the Lambda function (`Configuration → Environment variables` in the AWS console). Use AWS Secrets Manager / KMS for `*_SECRET_ACCESS_KEY` if treating them as secrets. On this entrypoint, `MAX_RUNTIME_SECONDS` defaults to **900** (Lambda's hard cap).

## Memory / timeout

- Memory: **3008–10240 MB**. More memory ≈ proportionally more vCPU; for transcoding, set high.
- Timeout: **900s** (Lambda max). The transcoder self-imposes a 75% runtime budget (default 675s) and exits cleanly before the platform kill.

## Prerequisites

- AWS CLI v2 installed and configured (`aws configure`).
- Docker (with buildx for multi-arch) installed locally.
- IAM permissions in your account for: ECR, Lambda, IAM, EventBridge, CloudWatch Logs.
- (Recommended) the bucket region for SOURCE/DEST should match the Lambda region to avoid cross-region transfer cost.

## Build

From the repo root:

```sh
docker build -f aws/Dockerfile -t s3-hls-transcoder-aws .
```

## Push to ECR

```sh
# 1. Set common shell vars.
export AWS_REGION=us-east-1
export ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export ECR_REPO=s3-hls-transcoder-aws
export IMAGE_URI=$ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/$ECR_REPO:latest

# 2. Create the ECR repo (one-time).
aws ecr create-repository --repository-name $ECR_REPO --region $AWS_REGION

# 3. Authenticate Docker against ECR.
aws ecr get-login-password --region $AWS_REGION \
  | docker login --username AWS --password-stdin $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com

# 4. Build + push. Graviton (ARM64) is cheaper; pick one platform per Lambda
#    function (Lambda doesn't multi-arch dispatch).
docker buildx build -f aws/Dockerfile \
    --platform linux/arm64 \
    -t $IMAGE_URI \
    --push .
```

## Deploy

```sh
# 1. Create the IAM role for the function. Trust policy + the policy below
#    (substitute SOURCE_BUCKET / DEST_BUCKET).
aws iam create-role --role-name s3-hls-transcoder \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": { "Service": "lambda.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }]
  }'
aws iam put-role-policy --role-name s3-hls-transcoder \
  --policy-name s3-hls-transcoder-policy \
  --policy-document file://aws/iam-policy.json   # see Sample IAM policy below

# 2. Create the Lambda function from the container image.
aws lambda create-function \
  --function-name s3-hls-transcoder \
  --package-type Image \
  --code ImageUri=$IMAGE_URI \
  --architectures arm64 \
  --memory-size 10240 \
  --timeout 900 \
  --role arn:aws:iam::$ACCOUNT_ID:role/s3-hls-transcoder \
  --environment "Variables={SOURCE_BUCKET=...,SOURCE_ENDPOINT=...,...}"

# 3. Create the EventBridge cron rule.
aws events put-rule \
  --name s3-hls-transcoder-cron \
  --schedule-expression 'cron(0/15 * * * ? *)'

# 4. Allow EventBridge to invoke the function. (Without this, the rule fires
#    but the invoke is denied — easy to miss.)
aws lambda add-permission \
  --function-name s3-hls-transcoder \
  --statement-id eventbridge-invoke \
  --action lambda:InvokeFunction \
  --principal events.amazonaws.com \
  --source-arn arn:aws:events:$AWS_REGION:$ACCOUNT_ID:rule/s3-hls-transcoder-cron

# 5. Wire the rule to the function.
aws events put-targets \
  --rule s3-hls-transcoder-cron \
  --targets "Id=1,Arn=arn:aws:lambda:$AWS_REGION:$ACCOUNT_ID:function:s3-hls-transcoder"
```

To **update** after a code change: rebuild + push the image, then `aws lambda update-function-code --function-name s3-hls-transcoder --image-uri $IMAGE_URI`.

## Alternative: event-driven triggering (S3 → SQS or SNS → Lambda)

The EventBridge cron polls on a fixed schedule. If you want to trigger a transcoding pass automatically when a video is uploaded to the source bucket, you can use S3 event notifications instead of (or alongside) the cron rule.

**How it works**: the Lambda still scans the whole source bucket on each invocation and uses the global lock to prevent concurrent runs. The S3 event just wakes it up sooner. Multiple upload events arriving in quick succession are harmless — the second and later invocations will see the lock held and exit cleanly.

### Option A — S3 → SQS → Lambda (recommended)

SQS decouples the event from the invocation and batches rapid-fire uploads into fewer Lambda runs.

```sh
# 1. Create the queue.
SQS_ARN=$(aws sqs create-queue \
  --queue-name s3-hls-transcoder-events \
  --attributes '{"VisibilityTimeout":"900"}' \
  --query 'QueueUrl' --output text \
  | xargs aws sqs get-queue-attributes \
      --attribute-names QueueArn \
      --query 'Attributes.QueueArn' --output text)

# 2. Allow S3 to send messages to the queue.
#    Replace SOURCE_BUCKET with your actual bucket name.
aws sqs set-queue-attributes \
  --queue-url $(aws sqs get-queue-url --queue-name s3-hls-transcoder-events --query QueueUrl --output text) \
  --attributes "{
    \"Policy\": \"{\\\"Version\\\":\\\"2012-10-17\\\",\\\"Statement\\\":[{\\\"Effect\\\":\\\"Allow\\\",\\\"Principal\\\":{\\\"Service\\\":\\\"s3.amazonaws.com\\\"},\\\"Action\\\":\\\"sqs:SendMessage\\\",\\\"Resource\\\":\\\"$SQS_ARN\\\",\\\"Condition\\\":{\\\"ArnLike\\\":{\\\"aws:SourceArn\\\":\\\"arn:aws:s3:::SOURCE_BUCKET\\\"}}}]}\"
  }"

# 3. Enable S3 event notifications on the source bucket.
#    Filters to common video extensions; adjust as needed.
aws s3api put-bucket-notification-configuration \
  --bucket SOURCE_BUCKET \
  --notification-configuration "{
    \"QueueConfigurations\": [{
      \"QueueArn\": \"$SQS_ARN\",
      \"Events\": [\"s3:ObjectCreated:*\"],
      \"Filter\": {
        \"Key\": {
          \"FilterRules\": [
            {\"Name\": \"suffix\", \"Value\": \".mp4\"}
          ]
        }
      }
    }]
  }"

# 4. Grant the Lambda role permission to consume from the queue.
aws iam put-role-policy --role-name s3-hls-transcoder \
  --policy-name s3-hls-transcoder-sqs \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Action": ["sqs:ReceiveMessage","sqs:DeleteMessage","sqs:GetQueueAttributes"],
      "Resource": "'"$SQS_ARN"'"
    }]
  }'

# 5. Wire the queue as a Lambda event source.
#    batch-size=1 means one upload = one Lambda invocation (conservative; raise if uploads are bursty).
aws lambda create-event-source-mapping \
  --function-name s3-hls-transcoder \
  --event-source-arn $SQS_ARN \
  --batch-size 1
```

The Lambda handler ignores the SQS event payload — it only uses it as a wake-up signal and then scans the full source bucket normally. You can add filters for other extensions (`.mov`, `.mkv`, etc.) by adding more `QueueConfigurations` or using a wildcard suffix filter.

### Option B — S3 → SNS → Lambda (fan-out)

Use SNS if you want to notify other systems (e.g., a monitoring topic) at the same time.

```sh
# 1. Create the topic.
SNS_ARN=$(aws sns create-topic --name s3-hls-transcoder-events \
  --query TopicArn --output text)

# 2. Allow S3 to publish to the topic.
aws sns set-topic-attributes --topic-arn $SNS_ARN \
  --attribute-name Policy \
  --attribute-value "{
    \"Version\": \"2012-10-17\",
    \"Statement\": [{
      \"Effect\": \"Allow\",
      \"Principal\": {\"Service\": \"s3.amazonaws.com\"},
      \"Action\": \"sns:Publish\",
      \"Resource\": \"$SNS_ARN\",
      \"Condition\": {\"ArnLike\": {\"aws:SourceArn\": \"arn:aws:s3:::SOURCE_BUCKET\"}}
    }]
  }"

# 3. Subscribe the Lambda function to the topic.
aws sns subscribe \
  --topic-arn $SNS_ARN \
  --protocol lambda \
  --notification-endpoint arn:aws:lambda:$AWS_REGION:$ACCOUNT_ID:function:s3-hls-transcoder

# 4. Allow SNS to invoke the Lambda.
aws lambda add-permission \
  --function-name s3-hls-transcoder \
  --statement-id sns-invoke \
  --action lambda:InvokeFunction \
  --principal sns.amazonaws.com \
  --source-arn $SNS_ARN

# 5. Enable S3 event notifications on the source bucket.
aws s3api put-bucket-notification-configuration \
  --bucket SOURCE_BUCKET \
  --notification-configuration "{
    \"TopicConfigurations\": [{
      \"TopicArn\": \"$SNS_ARN\",
      \"Events\": [\"s3:ObjectCreated:*\"]
    }]
  }"
```

### Combining cron + events

You can keep the EventBridge cron rule alongside event-driven triggering. The cron acts as a catch-all for uploads that happened while the function was already running (and thus locked). The global lock ensures only one scan runs at a time regardless of how the Lambda is triggered.

## Local test (optional)

The Lambda Runtime Interface Emulator runs the image locally:

```sh
docker run --rm -p 9000:8080 \
    --env-file ../local/.env \
    s3-hls-transcoder-aws

# In another shell:
curl -X POST 'http://localhost:9000/2015-03-31/functions/function/invocations' -d '{}'
```

## Sample IAM policy

[`iam-policy.json`](./iam-policy.json) in this folder is the policy referenced by the deploy snippet above. Replace `SOURCE_BUCKET` and `DEST_BUCKET` with your bucket names before applying. If your S3-compatible buckets live outside AWS (e.g., R2), the function only needs the CloudWatch Logs statement; credentials for the third-party endpoint come from env vars.
