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
docker build -f aws/Dockerfile -t s3-hsts-transcoder-aws .
```

## Push to ECR

```sh
# 1. Set common shell vars.
export AWS_REGION=us-east-1
export ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export ECR_REPO=s3-hsts-transcoder-aws
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
aws iam create-role --role-name s3-hsts-transcoder \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": { "Service": "lambda.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }]
  }'
aws iam put-role-policy --role-name s3-hsts-transcoder \
  --policy-name s3-hsts-transcoder-policy \
  --policy-document file://aws/iam-policy.json   # see Sample IAM policy below

# 2. Create the Lambda function from the container image.
aws lambda create-function \
  --function-name s3-hsts-transcoder \
  --package-type Image \
  --code ImageUri=$IMAGE_URI \
  --architectures arm64 \
  --memory-size 10240 \
  --timeout 900 \
  --role arn:aws:iam::$ACCOUNT_ID:role/s3-hsts-transcoder \
  --environment "Variables={SOURCE_BUCKET=...,SOURCE_ENDPOINT=...,...}"

# 3. Create the EventBridge cron rule.
aws events put-rule \
  --name s3-hsts-transcoder-cron \
  --schedule-expression 'cron(0/15 * * * ? *)'

# 4. Allow EventBridge to invoke the function. (Without this, the rule fires
#    but the invoke is denied — easy to miss.)
aws lambda add-permission \
  --function-name s3-hsts-transcoder \
  --statement-id eventbridge-invoke \
  --action lambda:InvokeFunction \
  --principal events.amazonaws.com \
  --source-arn arn:aws:events:$AWS_REGION:$ACCOUNT_ID:rule/s3-hsts-transcoder-cron

# 5. Wire the rule to the function.
aws events put-targets \
  --rule s3-hsts-transcoder-cron \
  --targets "Id=1,Arn=arn:aws:lambda:$AWS_REGION:$ACCOUNT_ID:function:s3-hsts-transcoder"
```

To **update** after a code change: rebuild + push the image, then `aws lambda update-function-code --function-name s3-hsts-transcoder --image-uri $IMAGE_URI`.

## Local test (optional)

The Lambda Runtime Interface Emulator runs the image locally:

```sh
docker run --rm -p 9000:8080 \
    --env-file ../local/.env \
    s3-hsts-transcoder-aws

# In another shell:
curl -X POST 'http://localhost:9000/2015-03-31/functions/function/invocations' -d '{}'
```

## Sample IAM policy

[`iam-policy.json`](./iam-policy.json) in this folder is the policy referenced by the deploy snippet above. Replace `SOURCE_BUCKET` and `DEST_BUCKET` with your bucket names before applying. If your S3-compatible buckets live outside AWS (e.g., R2), the function only needs the CloudWatch Logs statement; credentials for the third-party endpoint come from env vars.
