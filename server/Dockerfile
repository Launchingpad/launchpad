# CapRover forwards every app environment variable to `docker build` as a
# --build-arg. Declaring them here consumes the args so the build log has no
# warnings. None is baked into the image: runtime values come from CapRover.
ARG CAPROVER_GIT_COMMIT_SHA
ARG DATABASE_URL
ARG PORT
ARG LOG_ENV
ARG PUBLIC_BASE_URL
ARG S3_ENDPOINT
ARG S3_ACCESS_KEY
ARG S3_SECRET_KEY
ARG S3_BUCKET
ARG S3_PUBLIC_ENDPOINT
ARG S3_REGION
ARG POSTMARK_SERVER_TOKEN
ARG POSTMARK_FROM
ARG AUTH_OTP_PEPPER
ARG AUTH_FIXED_OTP
ARG WEB_ORIGINS

# The production toolchain, pinned to the patch AND the multi-arch digest, the
# same image CI uses. CI tests on go1.27.3 too
# (GOTOOLCHAIN in the CI files). Move them together.
FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# The runtime base, pinned to the tag AND the multi-arch digest.
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
ARG CAPROVER_GIT_COMMIT_SHA
ENV CAPROVER_GIT_COMMIT_SHA=$CAPROVER_GIT_COMMIT_SHA
# Soft heap limit below an assumed >= 512 MiB container. Change both together.
ENV GOMEMLIMIT=400MiB
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 devops \
    && mkdir -p /work/import \
    && chown 10001:10001 /work/import \
    && chmod 0700 /work/import
COPY --from=builder /out/server /bin/server
# IMPORT_WORK_DIR (LAUN-8): the web import's exports. A named volume mounted
# here starts with this owner and mode; the server makes the mode 0700 again
# at boot and proves it can write there.
USER devops
EXPOSE 8080
CMD ["/bin/server"]
